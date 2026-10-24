use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tokio::net::UdpSocket;
use tokio::sync::{mpsc, watch};

use crate::audio::{open_capture, open_playback, CaptureHandle, CaptureSource, PlaybackBackend};
use crate::config::ConfigPaths;
use crate::control::{self, ControlRequest, ControlResponse};
use crate::crypto::{parse_invitation, random_id, PairingStore};
use crate::discovery::{
    contact_addr, dedup_prefer_direct, directed_query_targets, encode_query, fresh_instance,
    local_lan_v4, open_announce, respond_to_query, Announce, DEFAULT_DISCOVERY_PORTS,
};
use crate::mixer::{mono_ms, Mixer};
use crate::protocol::{AudioFormat, MAX_DATAGRAM, MAX_PCM_CHUNK, STREAM_IDLE};
use crate::transport::{
    advance_pace, plan_probe, ReceiverEngine, SenderEngine, SenderSnapshot, MAX_PROBE_BYTES,
};

pub struct ServerOptions {
    pub paths: ConfigPaths,
    pub format: AudioFormat,
    pub listen_addr: SocketAddr,
    pub name: String,
    pub device_index: Option<usize>,
    pub playback_backend: PlaybackBackend,
    pub discovery: bool,
}

pub struct SenderOptions {
    pub paths: ConfigPaths,
    pub format: AudioFormat,
    pub to: Option<String>,
    pub peer: Option<String>,
    pub source: CaptureSource,
    pub name: String,
    pub device_index: Option<usize>,
}

struct ChosenPeer {
    epoch: [u8; 16],
    addr: SocketAddr,
    name: String,
    instance: [u8; 16],
}

pub async fn run_server(opts: ServerOptions) -> Result<(), String> {
    opts.format.require_wire()?;
    let invitation = load_invitation(&opts.paths)?;
    let token = parse_invitation(&invitation).map_err(|err| err.to_string())?;
    let epoch = random_id();
    let instance = fresh_instance();
    let mixer = Arc::new(Mixer::new(opts.format));
    if let Ok(prefs) = opts.paths.preferences() {
        if let Some(gain) = prefs.master_gain {
            mixer.set_master_gain(gain);
        }
    }
    let engine = Arc::new(Mutex::new(ReceiverEngine::new(token, epoch)));
    let (device_tx, device_rx) = watch::channel(opts.device_index);

    let paths = opts.paths.clone();
    let control_invitation = invitation.clone();
    let control_mixer = Arc::clone(&mixer);
    let control_engine = Arc::clone(&engine);
    let control_device = device_tx.clone();
    tokio::spawn(async move {
        let handler = Arc::new(move |req: ControlRequest| {
            receiver_control(&control_mixer, &control_engine, &control_device, req)
        });
        if let Err(err) = control::run_control_server(&paths, control_invitation, handler).await {
            tracing::warn!("receiver control stopped: {err}");
        }
    });

    let playback_mixer = Arc::clone(&mixer);
    tokio::spawn(async move {
        supervise_playback(
            opts.format,
            opts.playback_backend,
            device_rx,
            playback_mixer,
        )
        .await;
    });

    let socket = udp_socket(opts.listen_addr, false)?;
    let bound = socket.local_addr().map_err(|err| err.to_string())?;
    let mut template = Announce {
        tcp_port: bound.port(),
        instance_id: instance,
        advertised_addr: local_lan_v4(),
        name: opts.name.clone(),
        epoch,
    };
    if opts.discovery {
        for port in DEFAULT_DISCOVERY_PORTS {
            let addr = SocketAddr::from((Ipv4Addr::UNSPECIFIED, port));
            let Ok(sock) = udp_socket(addr, true) else {
                tracing::warn!("discovery port {port} unavailable");
                continue;
            };
            let mut template = template.clone();
            let discovery_engine = engine.clone();
            let invitation = invitation.clone();
            tokio::spawn(async move {
                let mut buf = [0u8; 1024];
                loop {
                    let Ok((len, src)) = sock.recv_from(&mut buf).await else {
                        continue;
                    };
                    if len == 0 || len > 512 {
                        continue;
                    }
                    template.epoch = guard(&discovery_engine).epoch();
                    if let Some(packet) = respond_to_query(&buf[..len], &invitation, &template) {
                        let _ = sock.send_to(&packet, src).await;
                    }
                }
            });
        }
    }

    let mut buf = [0u8; 2048];
    let mut feedback = tokio::time::interval(Duration::from_millis(500));
    feedback.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut expire = tokio::time::interval(Duration::from_secs(1));
    expire.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        tokio::select! {
            received = socket.recv_from(&mut buf) => {
                let (len, src) = received.map_err(|err| err.to_string())?;
                if len == 0 || len > MAX_DATAGRAM {
                    guard(&engine).packets_rejected += 1;
                    continue;
                }
                if len <= 512 {
                    template.epoch = guard(&engine).epoch();
                    if let Some(packet) = respond_to_query(&buf[..len], &invitation, &template) {
                        let _ = socket.send_to(&packet, src).await;
                        continue;
                    }
                }
                guard(&engine).handle_datagram(&buf[..len], src, &mixer);
            }
            _ = feedback.tick() => {
                let packets = guard(&engine).feedback_due(Instant::now(), &mixer);
                for (addr, packet) in packets {
                    let _ = socket.send_to(&packet, addr).await;
                }
            }
            _ = expire.tick() => {
                guard(&engine).expire(STREAM_IDLE, Instant::now(), &mixer);
            }
        }
    }
}

pub async fn run_sender(opts: SenderOptions) -> Result<(), String> {
    opts.format.require_wire()?;
    let invitation = load_invitation(&opts.paths)?;
    let token = parse_invitation(&invitation).map_err(|err| err.to_string())?;
    let (ips, port_filter, name_filter) = target_of(&opts);
    let direct = direct_sockets(&ips, port_filter);
    let mut engine = SenderEngine::new(token, opts.name.clone());
    let snap = Arc::new(Mutex::new(engine.snapshot()));
    let (device_tx, mut device_rx) = watch::channel(opts.device_index);
    let paths = opts.paths.clone();
    let control_invitation = invitation.clone();
    let control_snap = Arc::clone(&snap);
    let control_device = device_tx;
    tokio::spawn(async move {
        let handler = Arc::new(move |req: ControlRequest| {
            sender_control(&control_snap, &control_device, req)
        });
        if let Err(err) = control::run_control_server(&paths, control_invitation, handler).await {
            tracing::warn!("sender control stopped: {err}");
        }
    });

    let mut capture = open_capture_retry(opts.format, opts.source, opts.device_index).await;
    let audio = udp_socket(SocketAddr::from((Ipv4Addr::UNSPECIFIED, 0)), false)?;
    let (verify_tx, mut verify_rx) = mpsc::channel::<Vec<(Announce, SocketAddr)>>(1);
    let mut buf = [0u8; 2048];

    loop {
        let chosen = wait_for_peer(
            &invitation,
            &ips,
            &direct,
            port_filter,
            name_filter.as_deref(),
        )
        .await;
        tracing::info!(peer = %chosen.addr, name = %chosen.name, "receiver authenticated");
        engine.bind_peer(
            chosen.epoch,
            chosen.addr,
            chosen.name.clone(),
            chosen.instance,
        );
        *guard(&snap) = engine.snapshot();
        let mut session_addr = chosen.addr;
        for _ in 0..2 {
            if let Some(packet) = engine.hello() {
                let _ = audio.send_to(&packet, session_addr).await;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }

        let mut probing = false;
        let mut gradual = false;
        let mut plan = Vec::new();
        let mut plan_at = 0usize;
        let mut sent = [0u32; 5];
        let mut next_probe = Instant::now();
        let mut probe_done: Option<Instant> = None;
        let mut next_audio = Instant::now();
        let mut next_hello = Instant::now() + Duration::from_secs(1);
        let mut next_verify = Instant::now() + Duration::from_secs(2);
        let mut verify_inflight = false;
        let mut misses = 0u32;
        let mut last_train = Instant::now();
        let mut reprobe = false;
        begin_train(
            &mut engine,
            &mut plan,
            &mut plan_at,
            &mut sent,
            &mut next_probe,
            &mut probe_done,
            &mut probing,
            &mut last_train,
        );

        loop {
            if reprobe || (!probing && last_train.elapsed() >= Duration::from_secs(30)) {
                begin_train(
                    &mut engine,
                    &mut plan,
                    &mut plan_at,
                    &mut sent,
                    &mut next_probe,
                    &mut probe_done,
                    &mut probing,
                    &mut last_train,
                );
                reprobe = false;
            }
            let now = Instant::now();
            let hello_wait = next_hello.saturating_duration_since(now);
            let probe_wait = next_probe.saturating_duration_since(now);
            let audio_wait = next_audio.saturating_duration_since(now);
            let verify_wait = next_verify.saturating_duration_since(now);
            tokio::select! {
                _ = tokio::time::sleep(hello_wait) => {
                    if let Some(packet) = engine.hello() {
                        let _ = audio.send_to(&packet, session_addr).await;
                    }
                    next_hello = Instant::now() + Duration::from_secs(1);
                }
                _ = tokio::time::sleep(probe_wait), if probing => {
                    if plan_at >= plan.len() {
                        if probe_done.is_none() {
                            probe_done = Some(Instant::now());
                        }
                        if probe_done.is_some_and(|at| at.elapsed() >= Duration::from_millis(700)) {
                            let before = engine.profile();
                            engine.note_probe(sent, gradual);
                            gradual = true;
                            probing = false;
                            next_audio = Instant::now();
                            let current = engine.snapshot();
                            if engine.profile() != before {
                                tracing::info!(
                                    codec = current.codec,
                                    goodput = current.estimated_goodput_bps,
                                    "selected paced probe floor"
                                );
                            }
                            *guard(&snap) = current;
                        } else {
                            next_probe = Instant::now() + Duration::from_millis(50);
                        }
                    } else {
                        let spec = plan[plan_at];
                        if let Some(packet) = engine.probe(spec.step, spec.payload, mono_us()) {
                            let _ = audio.send_to(&packet, session_addr).await;
                            let step = usize::from(spec.step);
                            sent[step] = sent[step].saturating_add(u32::try_from(spec.payload).unwrap_or(u32::MAX));
                        }
                        plan_at += 1;
                        next_probe = Instant::now() + spec.delay_after;
                    }
                }
                _ = tokio::time::sleep(audio_wait) => {
                    let mut pcm = [0u8; MAX_PCM_CHUNK];
                    let _ = capture.try_read(&mut pcm);
                    if let Some(packet) = engine.audio(&pcm, mono_us()) {
                        let _ = audio.send_to(&packet, session_addr).await;
                    }
                    let woke = Instant::now();
                    next_audio = advance_pace(next_audio, woke);
                }
                received = audio.recv_from(&mut buf) => {
                    match received {
                        Ok((len, _)) if (1..=MAX_DATAGRAM).contains(&len) => {
                            if engine.handle_feedback(&buf[..len], mono_us()) {
                                let current = engine.snapshot();
                                if current.loss >= 0.10 && !probing && last_train.elapsed() >= Duration::from_secs(10) {
                                    reprobe = true;
                                }
                                *guard(&snap) = current;
                            }
                        }
                        Ok(_) => {}
                        Err(err) => {
                            tracing::warn!("sender recv: {err}");
                            tokio::time::sleep(Duration::from_millis(20)).await;
                        }
                    }
                }
                maybe = verify_rx.recv() => {
                    verify_inflight = false;
                    next_verify = Instant::now() + Duration::from_secs(2);
                    let Some(nodes) = maybe else { continue };
                    if let Some((ann, src)) = nodes.into_iter().find(|(ann, src)| {
                        peer_matches(ann, *src, chosen.instance, &ips, port_filter, name_filter.as_deref())
                    }) {
                        if engine.epoch() != Some(ann.epoch) {
                            tracing::info!("receiver restarted; rekeying");
                            break;
                        }
                        let addr = if direct.contains(&src) {src} else {contact_addr(&ann, src)};
                        if engine.addr() != Some(addr) {
                            engine.bind_peer(ann.epoch, addr, ann.name, ann.instance_id);
                            session_addr = addr;
                            *guard(&snap) = engine.snapshot();
                            tracing::info!(peer = %addr, "receiver address updated");
                        }
                        misses = 0;
                    } else {
                        misses += 1;
                        if misses >= 3 {
                            tracing::info!("receiver disappeared; discovering again");
                            break;
                        }
                    }
                }
                _ = tokio::time::sleep(verify_wait), if !verify_inflight => {
                    verify_inflight = true;
                    next_verify = Instant::now() + Duration::from_secs(2);
                    let tx = verify_tx.clone();
                    let invitation = invitation.clone();
                    let ips = ips.clone();
                    let direct = direct.clone();
                    let query = name_filter.clone().unwrap_or_default();
                    tokio::spawn(async move {
                        let found = discover_toward(&query, &invitation, Duration::from_millis(700), &ips, &direct)
                            .await
                            .unwrap_or_default();
                        let _ = tx.send(found).await;
                    });
                }
                changed = device_rx.changed() => {
                    if changed.is_err() {
                        continue;
                    }
                    let device = *device_rx.borrow();
                    capture.stop();
                    capture = open_capture_retry(opts.format, opts.source, device).await;
                }
            }
        }
    }
}

pub async fn discover_nodes(
    query_name: &str,
    token: &str,
    timeout: Duration,
) -> Result<Vec<(Announce, SocketAddr)>, String> {
    discover_toward(query_name, token, timeout, &[], &[]).await
}

async fn discover_toward(
    query_name: &str,
    token: &str,
    timeout: Duration,
    extra: &[Ipv4Addr],
    direct: &[SocketAddr],
) -> Result<Vec<(Announce, SocketAddr)>, String> {
    let query = encode_query(query_name, token)?;
    let socket = udp_socket(SocketAddr::from((Ipv4Addr::UNSPECIFIED, 0)), true)?;
    for target in direct {
        let _ = socket.send_to(&query.bytes, *target).await;
    }
    if direct.is_empty() {
        for target in directed_query_targets(extra) {
            let _ = socket.send_to(&query.bytes, target).await;
        }
    }
    let mut found = Vec::new();
    let deadline = Instant::now() + timeout;
    let mut buf = [0u8; 1024];
    while Instant::now() < deadline {
        let remain = deadline.saturating_duration_since(Instant::now());
        match tokio::time::timeout(remain, socket.recv_from(&mut buf)).await {
            Ok(Ok((len, src))) => {
                if len == 0 || len > 512 {
                    continue;
                }
                if let Some(announce) = open_announce(&buf[..len], token, &query.challenge) {
                    if query_name.is_empty() || announce.name == query_name {
                        found.push((announce, src));
                    }
                }
            }
            Ok(Err(_)) => continue,
            Err(_) => break,
        }
    }
    Ok(dedup_prefer_direct(found))
}

fn receiver_control(
    mixer: &Mixer,
    engine: &Mutex<ReceiverEngine>,
    device_tx: &watch::Sender<Option<usize>>,
    req: ControlRequest,
) -> ControlResponse {
    match req.command.as_str() {
        "status" => {
            let engine = guard(engine);
            let reports = engine.reports();
            let stats = mixer.stats();
            let underruns = stats.iter().map(|stream| stream.underruns).sum::<u64>();
            let streams = reports
                .into_iter()
                .map(|report| {
                    let stream_underruns = stats
                        .iter()
                        .find(|stream| stream.id == report.id)
                        .map(|stream| stream.underruns)
                        .unwrap_or(0);
                    json!({
                        "id": report.id,
                        "name": report.name,
                        "packets": report.received,
                        "lost": report.lost,
                        "underruns": stream_underruns,
                        "jitter_us": report.jitter_us,
                        "buffer_ms": report.buffer_ms,
                    })
                })
                .collect::<Vec<_>>();
            ControlResponse {
                ok: true,
                body: json!({
                    "role": "receiver",
                    "packets_received": engine.packets_received,
                    "packets_rejected": engine.packets_rejected,
                    "callbacks": mixer.callbacks(),
                    "underruns": underruns,
                    "master_gain": mixer.master_gain(),
                    "playback": crate::audio::playback_status(),
                    "streams": streams,
                }),
            }
        }
        "volume" => match apply_volume(mixer, &req.args) {
            Ok(()) => ControlResponse {
                ok: true,
                body: json!({}),
            },
            Err(err) => ControlResponse {
                ok: false,
                body: json!({"error": err}),
            },
        },
        "device" => match device_index(&req.args) {
            Ok(index) => {
                let _ = device_tx.send(index);
                ControlResponse {
                    ok: true,
                    body: json!({}),
                }
            }
            Err(err) => ControlResponse {
                ok: false,
                body: json!({"error": err}),
            },
        },
        _ => ControlResponse {
            ok: false,
            body: json!({"error": "unknown command"}),
        },
    }
}

fn sender_control(
    snap: &Mutex<SenderSnapshot>,
    device_tx: &watch::Sender<Option<usize>>,
    req: ControlRequest,
) -> ControlResponse {
    match req.command.as_str() {
        "status" => ControlResponse {
            ok: true,
            body: guard(snap).to_json(),
        },
        "volume" => ControlResponse {
            ok: false,
            body: json!({"error": "volume applies to the receiver"}),
        },
        "device" => match device_index(&req.args) {
            Ok(index) => {
                let _ = device_tx.send(index);
                ControlResponse {
                    ok: true,
                    body: json!({}),
                }
            }
            Err(err) => ControlResponse {
                ok: false,
                body: json!({"error": err}),
            },
        },
        _ => ControlResponse {
            ok: false,
            body: json!({"error": "unknown command"}),
        },
    }
}

fn apply_volume(mixer: &Mixer, args: &Value) -> Result<(), String> {
    if let Some(value) = args.get("master").filter(|value| !value.is_null()) {
        mixer.set_master_gain(finite_gain(value)?);
    }
    if let Some(id) = args.get("stream").and_then(|value| value.as_str()) {
        if let Some(value) = args.get("gain").filter(|value| !value.is_null()) {
            if !mixer.set_stream_gain(id, finite_gain(value)?) {
                return Err("unknown stream".into());
            }
        }
    }
    Ok(())
}

fn finite_gain(value: &Value) -> Result<f32, String> {
    let gain = value
        .as_f64()
        .filter(|gain| gain.is_finite() && (0.0..=2.0).contains(gain))
        .ok_or("gain must be finite and between 0 and 2")?;
    Ok(gain as f32)
}

fn device_index(args: &Value) -> Result<Option<usize>, String> {
    match args.get("index") {
        None => Err("device command requires index".into()),
        Some(value) if value.is_null() => Ok(None),
        Some(value) => {
            let index = value.as_u64().ok_or("device index must be an integer")?;
            usize::try_from(index)
                .map(Some)
                .map_err(|_| "device index too large".to_string())
        }
    }
}

async fn supervise_playback(
    format: AudioFormat,
    backend: PlaybackBackend,
    mut device_rx: watch::Receiver<Option<usize>>,
    mixer: Arc<Mixer>,
) {
    let pull_mixer = Arc::clone(&mixer);
    let pull: Arc<dyn Fn(&mut [u8], usize) + Send + Sync> =
        Arc::new(move |out, frames| pull_mixer.pull(out, frames));
    loop {
        let device = *device_rx.borrow();
        match backend {
            PlaybackBackend::Null => {
                let _handle = open_playback(format, backend, device, Arc::clone(&pull));
                if device_rx.changed().await.is_err() {
                    return;
                }
            }
            PlaybackBackend::Cpal => {
                match open_playback(format, backend, device, Arc::clone(&pull)) {
                    Ok(mut handle) => {
                        let opened = Instant::now();
                        loop {
                            tokio::select! {
                                changed = device_rx.changed() => {
                                    if changed.is_ok() {
                                        break;
                                    }
                                }
                                _ = tokio::time::sleep(Duration::from_secs(1)) => {
                                    if opened.elapsed() > Duration::from_secs(5) {
                                        let last = mixer.last_callback_ms();
                                        let now = mono_ms();
                                        if last == 0 || now.saturating_sub(last) > 3_000 {
                                            tracing::warn!("playback callbacks stalled; reopening output");
                                            break;
                                        }
                                    }
                                }
                            }
                        }
                        handle.stop();
                    }
                    Err(err) => {
                        tracing::warn!("playback open failed: {err}");
                        tokio::time::sleep(Duration::from_secs(2)).await;
                    }
                }
            }
        }
    }
}

async fn open_capture_retry(
    format: AudioFormat,
    source: CaptureSource,
    device: Option<usize>,
) -> CaptureHandle {
    loop {
        match open_capture(format, source, device) {
            Ok(handle) => return handle,
            Err(err) => {
                tracing::warn!("capture open failed: {err}");
                tokio::time::sleep(Duration::from_secs(2)).await;
            }
        }
    }
}

async fn wait_for_peer(
    token: &str,
    ips: &[Ipv4Addr],
    direct: &[SocketAddr],
    port: Option<u16>,
    name: Option<&str>,
) -> ChosenPeer {
    let query = name.unwrap_or("");
    loop {
        match discover_toward(query, token, Duration::from_millis(800), ips, direct).await {
            Ok(nodes) => {
                if let Some((announce, src)) = nodes.into_iter().find(|(announce, src)| {
                    if direct.is_empty() {
                        select_match(announce, *src, ips, port, name)
                    } else {
                        direct.contains(src) && name.map(|n| announce.name == n).unwrap_or(true)
                    }
                }) {
                    return ChosenPeer {
                        epoch: announce.epoch,
                        addr: if direct.contains(&src) {
                            src
                        } else {
                            contact_addr(&announce, src)
                        },
                        name: announce.name,
                        instance: announce.instance_id,
                    };
                }
            }
            Err(err) => tracing::warn!("discovery: {err}"),
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

fn select_match(
    announce: &Announce,
    src: SocketAddr,
    ips: &[Ipv4Addr],
    port: Option<u16>,
    name: Option<&str>,
) -> bool {
    if name.is_some_and(|name| announce.name != name) {
        return false;
    }
    if ips.first().is_some_and(|ip| src.ip() != IpAddr::V4(*ip)) {
        return false;
    }
    if port.is_some_and(|port| announce.tcp_port != port) {
        return false;
    }
    true
}

fn peer_matches(
    announce: &Announce,
    src: SocketAddr,
    instance: [u8; 16],
    ips: &[Ipv4Addr],
    port: Option<u16>,
    name: Option<&str>,
) -> bool {
    if let (Some(ip), Some(port)) = (ips.first(), port) {
        return src == SocketAddr::new(IpAddr::V4(*ip), port)
            && name.map(|n| announce.name == n).unwrap_or(true);
    }
    if announce.instance_id == instance {
        return true;
    }
    if ips.first().is_some_and(|ip| src.ip() == IpAddr::V4(*ip)) {
        return port.map(|port| announce.tcp_port == port).unwrap_or(true);
    }
    if ips.is_empty() {
        if let Some(name) = name {
            return announce.name == name;
        }
    }
    false
}

fn direct_sockets(ips: &[Ipv4Addr], port: Option<u16>) -> Vec<SocketAddr> {
    let Some(port) = port else {
        return Vec::new();
    };
    ips.iter()
        .copied()
        .map(|ip| SocketAddr::new(IpAddr::V4(ip), port))
        .collect()
}

fn target_of(opts: &SenderOptions) -> (Vec<Ipv4Addr>, Option<u16>, Option<String>) {
    let mut name = opts.peer.clone();
    let Some(to) = opts.to.as_deref() else {
        return (Vec::new(), None, name);
    };
    if let Ok(sock) = to.parse::<SocketAddr>() {
        let ips = match sock.ip() {
            IpAddr::V4(ip) => vec![ip],
            IpAddr::V6(_) => Vec::new(),
        };
        return (ips, Some(sock.port()), name);
    }
    if name.is_none() {
        name = Some(to.to_string());
    }
    (Vec::new(), None, name)
}

fn begin_train(
    engine: &mut SenderEngine,
    plan: &mut Vec<crate::transport::ProbePacketSpec>,
    plan_at: &mut usize,
    sent: &mut [u32; 5],
    next_probe: &mut Instant,
    probe_done: &mut Option<Instant>,
    probing: &mut bool,
    last_train: &mut Instant,
) {
    engine.begin_probe();
    *plan = plan_probe(MAX_PROBE_BYTES);
    *plan_at = 0;
    *sent = [0; 5];
    *next_probe = Instant::now();
    *probe_done = None;
    *probing = true;
    *last_train = Instant::now();
}

fn load_invitation(paths: &ConfigPaths) -> Result<String, String> {
    // Group invitation: every holder is trusted. Rotate the token to evict the group.
    let cfg = PairingStore::new(&paths.root)
        .load()
        .map_err(|err| err.to_string())?
        .ok_or_else(|| "not paired; run pair first (plaintext mode is disabled)".to_string())?;
    parse_invitation(&cfg.invitation_token)
        .map_err(|_| "pairing invitation must be 32-byte (256-bit) hex".to_string())?;
    Ok(cfg.invitation_token)
}

fn udp_socket(addr: SocketAddr, broadcast: bool) -> Result<UdpSocket, String> {
    let socket =
        std::net::UdpSocket::bind(addr).map_err(|err| format!("udp bind {addr}: {err}"))?;
    if broadcast && socket.set_broadcast(true).is_err() {
        tracing::warn!("udp broadcast enable failed on {addr}");
    }
    socket
        .set_nonblocking(true)
        .map_err(|err| err.to_string())?;
    UdpSocket::from_std(socket).map_err(|err| err.to_string())
}

fn guard<T>(lock: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    lock.lock().unwrap_or_else(|err| err.into_inner())
}

fn mono_us() -> u64 {
    static ORIGIN: std::sync::OnceLock<Instant> = std::sync::OnceLock::new();
    ORIGIN.get_or_init(Instant::now).elapsed().as_micros() as u64
}
