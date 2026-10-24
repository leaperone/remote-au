use std::collections::{BTreeSet, HashMap};
use std::net::SocketAddr;
use std::time::{Duration, Instant};

use crate::crypto::{random_id, DirectionKeys, ReplayWindow};
use crate::mixer::Mixer;
use crate::protocol::{
    decode_audio, decode_canonical, decode_hello, decode_probe, encode_feedback, FeedbackBody,
    KIND_AUDIO, KIND_HELLO, KIND_PROBE, MAX_STREAMS,
};
use crate::transport::adapt::ReceiverAdapt;
use crate::transport::packet::{open_packet, seal_packet};

pub const MAX_SESSION_HISTORY: usize = 4096;

struct Peer {
    keys: DirectionKeys,
    replay: ReplayWindow,
    send_counter: u64,
    addr: SocketAddr,
    name: String,
    active: bool,
    received: u32,
    lost: u32,
    max_audio_seq: u64,
    loss_cursor: u64,
    seen_audio: BTreeSet<u64>,
    jitter: f64,
    last_transit: Option<i64>,
    latest_send_ts: u64,
    latest_arrival: Instant,
    suggested_ms: u16,
    buffer_ms: u32,
    probe_gen: u32,
    probe_bytes: [u32; 5],
    probe_recv_first_us: [u64; 5],
    probe_recv_last_us: [u64; 5],
    last_feedback: Option<Instant>,
    underruns_mark: u64,
    adapt: ReceiverAdapt,
}

#[derive(Clone)]
pub struct PeerReport {
    pub id: String,
    pub name: String,
    pub received: u32,
    pub lost: u32,
    pub jitter_us: u32,
    pub buffer_ms: u32,
}

pub struct ReceiverEngine {
    token: [u8; 32],
    epoch: [u8; 16],
    origin: Instant,
    peers: HashMap<[u8; 16], Peer>,
    history_cap: usize,
    pub packets_received: u64,
    pub packets_rejected: u64,
}

impl ReceiverEngine {
    pub fn new(token: [u8; 32], epoch: [u8; 16]) -> Self {
        Self::with_history_cap(token, epoch, MAX_SESSION_HISTORY)
    }

    #[doc(hidden)]
    pub fn with_history_cap(token: [u8; 32], epoch: [u8; 16], history_cap: usize) -> Self {
        Self {
            token,
            epoch,
            origin: Instant::now(),
            peers: HashMap::new(),
            history_cap,
            packets_received: 0,
            packets_rejected: 0,
        }
    }

    pub fn epoch(&self) -> [u8; 16] {
        self.epoch
    }

    pub fn history_len(&self) -> usize {
        self.peers.len()
    }

    pub fn peer_count(&self) -> usize {
        self.peers.values().filter(|peer| peer.active).count()
    }

    pub fn handle_datagram(&mut self, packet: &[u8], src: SocketAddr, mixer: &Mixer) {
        let Some(view) = crate::protocol::parse_packet(packet) else {
            self.packets_rejected += 1;
            return;
        };
        if view.epoch != self.epoch || view.counter == 0 {
            self.packets_rejected += 1;
            return;
        }
        let known = self.peers.contains_key(&view.session);
        let active = self.peers.values().filter(|peer| peer.active).count();
        if !known && (view.kind != KIND_HELLO || active >= MAX_STREAMS) {
            self.packets_rejected += 1;
            return;
        }
        let keys = if let Some(peer) = self.peers.get(&view.session) {
            peer.keys.clone()
        } else {
            DirectionKeys::derive(&self.token, &self.epoch, &view.session)
        };
        let Some(opened) = open_packet(&keys.sender_to_receiver, packet) else {
            self.packets_rejected += 1;
            return;
        };
        if !known {
            let Some(name) = decode_hello(&opened.body) else {
                self.packets_rejected += 1;
                return;
            };
            if self.peers.len() >= self.history_cap {
                self.rotate_epoch(mixer);
                self.packets_rejected += 1;
                return;
            }
            let id = hex::encode(opened.session);
            if mixer.ensure_stream(&id, &name).is_err() {
                self.packets_rejected += 1;
                return;
            }
            let mut peer = Peer::new(keys, src, name);
            if !peer.replay.commit(opened.counter) {
                mixer.remove_stream(&id);
                self.packets_rejected += 1;
                return;
            }
            self.peers.insert(opened.session, peer);
            self.packets_received += 1;
            return;
        }
        let session = opened.session;
        let counter = opened.counter;
        {
            let Some(peer) = self.peers.get_mut(&session) else {
                self.packets_rejected += 1;
                return;
            };
            if !peer.replay.commit(counter) {
                self.packets_rejected += 1;
                return;
            }
            peer.addr = src;
        }
        match opened.kind {
            KIND_HELLO => {
                if let Some(name) = decode_hello(&opened.body) {
                    if let Some(peer) = self.peers.get_mut(&session) {
                        peer.name = name.clone();
                        peer.active = true;
                    }
                    let _ = mixer.ensure_stream(hex::encode(session), name);
                }
                self.packets_received += 1;
            }
            KIND_AUDIO if self.peers.get(&session).is_some_and(|peer| peer.active) => {
                self.apply_audio(session, &opened.body, mixer);
            }
            KIND_PROBE if self.peers.get(&session).is_some_and(|peer| peer.active) => {
                self.apply_probe(session, &opened.body, mixer);
            }
            KIND_AUDIO | KIND_PROBE => {}
            _ => self.packets_rejected += 1,
        }
    }

    pub fn feedback_due(&mut self, now: Instant, mixer: &Mixer) -> Vec<(SocketAddr, Vec<u8>)> {
        let epoch = self.epoch;
        let sessions: Vec<[u8; 16]> = self.peers.keys().copied().collect();
        let mut out = Vec::new();
        for session in sessions {
            let due = self.peers.get(&session).is_some_and(|peer| {
                peer.active
                    && match peer.last_feedback {
                        None => true,
                        Some(at) => now.saturating_duration_since(at) >= Duration::from_millis(500),
                    }
            });
            if !due {
                continue;
            }
            let id = hex::encode(session);
            let underruns = mixer
                .stats()
                .into_iter()
                .find(|s| s.id == id)
                .map(|s| s.underruns)
                .unwrap_or(0);
            let Some(peer) = self.peers.get_mut(&session) else {
                continue;
            };
            let delta = underruns.saturating_sub(peer.underruns_mark);
            peer.underruns_mark = underruns;
            let local = peer
                .adapt
                .observe(peer.jitter as u32, delta.min(u32::MAX as u64) as u32);
            peer.buffer_ms = local.max(u32::from(peer.suggested_ms)).clamp(15, 150);
            let buffer_ms = peer.buffer_ms;
            let _ = mixer.set_stream_target_ms(&id, buffer_ms);
            let delay_us =
                u32::try_from(peer.latest_arrival.elapsed().as_micros()).unwrap_or(u32::MAX);
            let probe_recv_span_us = probe_recv_spans(peer);
            let feedback = FeedbackBody {
                echo_send_ts_us: peer.latest_send_ts,
                delay_us,
                echo_seq: peer.max_audio_seq,
                received: peer.received,
                lost: peer.lost,
                jitter_us: peer.jitter.min(u32::MAX as f64) as u32,
                underruns: u32::try_from(underruns / 240).unwrap_or(u32::MAX),
                buffer_ms: u16::try_from(buffer_ms).unwrap_or(u16::MAX),
                probe_gen: peer.probe_gen,
                probe_bytes: peer.probe_bytes,
                probe_recv_span_us,
            };
            peer.send_counter = peer.send_counter.saturating_add(1);
            let counter = peer.send_counter;
            if counter == 0 {
                continue;
            }
            let body = encode_feedback(&feedback);
            let Some(packet) = seal_packet(
                &peer.keys.receiver_to_sender,
                &epoch,
                &session,
                counter,
                crate::protocol::KIND_FEEDBACK,
                &body,
            ) else {
                continue;
            };
            peer.last_feedback = Some(now);
            out.push((peer.addr, packet));
        }
        out
    }

    pub fn expire(&mut self, max_idle: Duration, now: Instant, mixer: &Mixer) {
        let removed = mixer.expire_idle(max_idle, now);
        for (session, peer) in &mut self.peers {
            let id = hex::encode(session);
            if removed.iter().any(|gone| gone == &id) {
                peer.active = false;
            }
        }
    }

    pub fn reports(&self) -> Vec<PeerReport> {
        self.peers
            .iter()
            .filter(|(_, peer)| peer.active)
            .map(|(session, peer)| PeerReport {
                id: hex::encode(session),
                name: peer.name.clone(),
                received: peer.received,
                lost: peer.lost,
                jitter_us: peer.jitter.min(u32::MAX as f64) as u32,
                buffer_ms: peer.buffer_ms,
            })
            .collect()
    }

    fn apply_audio(&mut self, session: [u8; 16], body: &[u8], mixer: &Mixer) {
        let Some(audio) = decode_audio(body) else {
            self.packets_rejected += 1;
            return;
        };
        let Some(pcm) = decode_canonical(audio.codec, &audio.payload) else {
            self.packets_rejected += 1;
            return;
        };
        let id = hex::encode(session);
        let arrival = self.origin.elapsed().as_micros() as i64;
        let seq = audio.seq;
        {
            let Some(peer) = self.peers.get_mut(&session) else {
                return;
            };
            observe_audio_seq(peer, seq);
            let transit = arrival.saturating_sub(audio.send_ts_us as i64);
            if let Some(prev) = peer.last_transit {
                let delta = (transit - prev).unsigned_abs() as f64;
                peer.jitter += (delta - peer.jitter) / 16.0;
            }
            peer.last_transit = Some(transit);
            peer.latest_send_ts = audio.send_ts_us;
            peer.latest_arrival = Instant::now();
            peer.suggested_ms = audio.buffer_ms;
        }
        self.packets_received += 1;
        mixer.touch(&id);
        let _ = mixer.push_packet(&id, seq, &pcm);
    }

    fn apply_probe(&mut self, session: [u8; 16], body: &[u8], mixer: &Mixer) {
        let Some(probe) = decode_probe(body) else {
            self.packets_rejected += 1;
            return;
        };
        {
            let Some(peer) = self.peers.get_mut(&session) else {
                return;
            };
            if probe.gen != peer.probe_gen {
                peer.probe_gen = probe.gen;
                peer.probe_bytes = [0; 5];
                peer.probe_recv_first_us = [0; 5];
                peer.probe_recv_last_us = [0; 5];
            }
            let step = usize::from(probe.step);
            let arrival_us = self.origin.elapsed().as_micros() as u64;
            if step < peer.probe_bytes.len() {
                peer.probe_bytes[step] =
                    peer.probe_bytes[step].saturating_add(probe.payload.len() as u32);
                if peer.probe_recv_first_us[step] == 0 {
                    peer.probe_recv_first_us[step] = arrival_us;
                }
                peer.probe_recv_last_us[step] = arrival_us;
            }
            peer.latest_send_ts = probe.send_ts_us;
            peer.latest_arrival = Instant::now();
        }
        self.packets_received += 1;
        mixer.touch(&hex::encode(session));
    }
}

fn probe_recv_spans(peer: &Peer) -> [u32; 5] {
    let mut out = [0u32; 5];
    for i in 0..5 {
        if peer.probe_bytes[i] == 0 {
            continue;
        }
        let first = peer.probe_recv_first_us[i];
        let last = peer.probe_recv_last_us[i];
        if first == 0 {
            continue;
        }
        if last <= first {
            out[i] = u32::MAX;
            continue;
        }
        out[i] = u32::try_from(last - first).unwrap_or(u32::MAX).max(1);
    }
    out
}

fn observe_audio_seq(peer: &mut Peer, seq: u64) {
    if seq == 0 || !peer.seen_audio.insert(seq) {
        return;
    }
    peer.received = peer.received.saturating_add(1);
    if seq > peer.max_audio_seq {
        peer.max_audio_seq = seq;
    }
    if peer.loss_cursor == 0 {
        peer.loss_cursor = 1;
    }
    let horizon = peer.max_audio_seq.saturating_sub(32);
    if horizon > peer.loss_cursor.saturating_add(48) {
        let seen = peer.seen_audio.range(peer.loss_cursor..horizon).count() as u64;
        let span = horizon - peer.loss_cursor;
        peer.lost = peer
            .lost
            .saturating_add(u32::try_from(span.saturating_sub(seen)).unwrap_or(u32::MAX));
        peer.seen_audio.retain(|&seen| seen >= horizon);
        peer.loss_cursor = horizon;
        return;
    }
    while peer.loss_cursor < horizon {
        if !peer.seen_audio.remove(&peer.loss_cursor) {
            peer.lost = peer.lost.saturating_add(1);
        }
        peer.loss_cursor += 1;
    }
}

impl ReceiverEngine {
    fn rotate_epoch(&mut self, mixer: &Mixer) {
        self.epoch = random_id();
        for id in mixer.stream_ids() {
            mixer.remove_stream(&id);
        }
        self.peers.clear();
    }
}

impl Peer {
    fn new(keys: DirectionKeys, addr: SocketAddr, name: String) -> Self {
        Self {
            keys,
            replay: ReplayWindow::new(2048),
            send_counter: 0,
            addr,
            name,
            active: true,
            received: 0,
            lost: 0,
            max_audio_seq: 0,
            loss_cursor: 0,
            seen_audio: BTreeSet::new(),
            jitter: 0.0,
            last_transit: None,
            latest_send_ts: 0,
            latest_arrival: Instant::now(),
            suggested_ms: 20,
            buffer_ms: 20,
            probe_gen: 0,
            probe_bytes: [0; 5],
            probe_recv_first_us: [0; 5],
            probe_recv_last_us: [0; 5],
            last_feedback: None,
            underruns_mark: 0,
            adapt: ReceiverAdapt::new(),
        }
    }
}
