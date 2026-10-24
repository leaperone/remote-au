use crate::{
    audio::{list_devices, CaptureSource, PlaybackBackend},
    config::{audio_format_from_globals, private_write, ConfigPaths},
    control::control_request,
    crypto::{generate_invitation_token, PairingConfig, PairingStore},
    daemon::{discover_nodes, run_sender, run_server, SenderOptions, ServerOptions},
    VERSION,
};
use clap::{Parser, Subcommand};
use serde_json::json;
use std::{net::SocketAddr, path::PathBuf, time::Duration};

#[derive(Parser)]
#[command(name="remote-au", version=VERSION, about="Paired LAN audio daemon. Start server on the speaker machine and send on sources.")]
pub struct Cli {
    #[arg(long, global = true)]
    pub config_dir: Option<PathBuf>,
    #[arg(long, global = true, default_value = "48000")]
    pub rate: u32,
    #[arg(long, global = true, default_value = "2")]
    pub channels: u8,
    #[arg(long, global = true, default_value = "5")]
    pub frame_ms: u32,
    #[arg(long, global = true, default_value = "info")]
    pub log_level: String,
    #[command(subcommand)]
    pub command: Commands,
}

#[derive(Subcommand)]
pub enum Commands {
    /// List playback endpoints. Windows loopback selects these same endpoint indices.
    Devices {
        #[arg(long)]
        json: bool,
    },
    /// Create a group invitation, import a private invitation file, or export to a file.
    Pair {
        #[arg(long, conflicts_with_all=["import_file", "export_file", "rotate"])]
        show: bool,
        #[arg(long, conflicts_with_all=["export_file", "rotate"])]
        import_file: Option<PathBuf>,
        #[arg(long, conflicts_with = "rotate")]
        export_file: Option<PathBuf>,
        /// Replace the group invitation. All peers must pair again and daemons restart.
        #[arg(long)]
        rotate: bool,
        #[arg(long)]
        receiver_name: Option<String>,
    },
    /// Discover authenticated receivers, with the direct 192.168.2/24 link preferred.
    Nodes {
        #[arg(long, default_value = "")]
        name: String,
        #[arg(long, default_value = "1200")]
        timeout_ms: u64,
    },
    /// Query the running daemon through authenticated localhost control.
    Status,
    /// Persist a receiver address or return to automatic discovery. Restart sender to apply.
    SelectServer {
        #[arg(default_value = "auto")]
        address: String,
    },
    /// Persist endpoint indices. Restart the daemon to apply device changes.
    Device {
        #[arg(long)]
        output: Option<usize>,
        #[arg(long)]
        capture: Option<usize>,
        #[arg(long)]
        reset: bool,
    },
    /// Set receiver gain (0..2). Master gain is saved across restarts.
    Volume {
        #[arg(long)]
        master: Option<f64>,
        #[arg(long)]
        stream: Option<String>,
        #[arg(long)]
        gain: Option<f64>,
    },
    /// Receive, mix and play paired sources. Remains running while sources are offline.
    Server {
        #[arg(long, default_value = "0.0.0.0:47000")]
        addr: String,
        #[arg(long)]
        name: Option<String>,
        #[arg(long)]
        device: Option<usize>,
        #[arg(long)]
        no_discovery: bool,
        /// Consume audio without opening a speaker (integration tests only).
        #[arg(long)]
        null_playback: bool,
    },
    /// Capture and stream; retry discovery and audio devices until available.
    Send {
        #[arg(long)]
        to: Option<String>,
        #[arg(long)]
        peer: Option<String>,
        #[arg(long, default_value="loopback", value_parser=["mic","loopback","synth","null"])]
        source: String,
        #[arg(long)]
        name: Option<String>,
        #[arg(long)]
        device: Option<usize>,
    },
}

fn token(paths: &ConfigPaths) -> Result<String, String> {
    let p = PairingStore::new(&paths.root)
        .load()
        .map_err(|e| e.to_string())?
        .ok_or("not paired; run pair on the receiver and import its invitation file on sources")?;
    Ok(p.invitation_token)
}
fn valid_token(s: &str) -> bool {
    s.len() == 64 && s.bytes().all(|c| c.is_ascii_hexdigit())
}

pub async fn run(cli: Cli) -> Result<(), String> {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(&cli.log_level));
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_ansi(false)
        .with_writer(std::io::stderr)
        .init();
    let paths = ConfigPaths::resolve(cli.config_dir).map_err(|e| e.to_string())?;
    let _daemon_guard = if matches!(
        &cli.command,
        Commands::Server { .. } | Commands::Send { .. }
    ) {
        Some(
            paths
                .acquire_daemon_lock()
                .map_err(|e| format!("cannot start daemon: {e}"))?,
        )
    } else {
        None
    };
    if cli.rate != 48000 || cli.channels != 2 || cli.frame_ms != 5 {
        return Err("Rust daemon uses 48000 Hz stereo and 5 ms packets; remove custom --rate/--channels/--frame-ms".into());
    }
    let format = audio_format_from_globals(cli.rate, cli.channels, cli.frame_ms);
    match cli.command {
        Commands::Devices { json } => {
            let d = list_devices();
            let inputs = crate::audio::list_capture_devices();
            if json {
                println!(
                    "{}",
                    serde_json::to_string_pretty(&serde_json::json!({"outputs":d,"inputs":inputs}))
                        .map_err(|e| e.to_string())?
                );
            } else {
                println!("Playback endpoints (Windows loopback --device INDEX):");
                for d in d {
                    println!(
                        "[{}] {}{}",
                        d.index,
                        d.name,
                        if d.is_default { " (default)" } else { "" }
                    );
                }
                println!("Input endpoints (send --source mic --device INDEX):");
                for d in inputs {
                    println!("[{}] {}", d.index, d.name);
                }
            }
        }
        Commands::Pair {
            show,
            import_file,
            export_file,
            rotate,
            receiver_name,
        } => {
            let store = PairingStore::new(&paths.root);
            let old = store.load().map_err(|e| e.to_string())?;
            if show {
                println!(
                    "{}",
                    if old.is_some() {
                        "paired (group invitation configured)"
                    } else {
                        "not paired"
                    }
                );
                return Ok(());
            }
            if let Some(file) = import_file {
                let bytes =
                    std::fs::read(&file).map_err(|e| format!("read invitation file: {e}"))?;
                if bytes.len() > 8192 {
                    return Err("invitation file is too large".into());
                }
                let mut cfg: PairingConfig =
                    serde_json::from_slice(&bytes).map_err(|_| "invalid invitation file")?;
                if !valid_token(&cfg.invitation_token) {
                    return Err("invitation requires a 256-bit hexadecimal token".into());
                }
                if receiver_name.is_some() {
                    cfg.receiver_name = receiver_name;
                }
                store.save(&cfg).map_err(|e| e.to_string())?;
                println!("Invitation imported. Restart any running daemon to apply.");
            } else if let Some(file) = export_file {
                let cfg = old.ok_or("not paired; run pair first")?;
                if file.exists() {
                    return Err("export path already exists; choose a new private file".into());
                }
                private_write(
                    &file,
                    &serde_json::to_vec_pretty(&cfg).map_err(|e| e.to_string())?,
                )
                .map_err(|e| e.to_string())?;
                println!("Private invitation exported. Transfer it through a trusted channel, import it, then delete the transfer copy.");
            } else {
                if old.is_some() && !rotate {
                    return Err("already paired; use --export-file to pair another node or --rotate to revoke the whole group".into());
                }
                store
                    .save(&PairingConfig {
                        invitation_token: generate_invitation_token(),
                        receiver_name,
                    })
                    .map_err(|e| e.to_string())?;
                println!("Group invitation saved privately. Use pair --export-file PATH to prepare a source. Restart daemons after rotation.");
            }
        }
        Commands::Nodes { name, timeout_ms } => {
            if !(50..=10000).contains(&timeout_ms) {
                return Err("--timeout-ms must be 50..10000".into());
            }
            let nodes =
                discover_nodes(&name, &token(&paths)?, Duration::from_millis(timeout_ms)).await?;
            for (a, peer) in nodes {
                println!(
                    "{} {}:{} (authenticated, via {})",
                    a.name,
                    peer.ip(),
                    a.tcp_port,
                    peer
                );
            }
        }
        Commands::Status => {
            let r = control_request(&paths, &token(&paths)?, "status", json!({})).await?;
            if !r.ok {
                return Err(r.body.to_string());
            }
            println!(
                "{}",
                serde_json::to_string_pretty(&r.body).map_err(|e| e.to_string())?
            );
        }
        Commands::SelectServer { address } => {
            let mut prefs = paths.preferences().map_err(|e| e.to_string())?;
            prefs.server = if address == "auto" {
                None
            } else {
                let endpoint: SocketAddr = address
                    .parse()
                    .map_err(|_| "use auto or a numeric IP:port")?;
                if !endpoint.is_ipv4() || endpoint.port() == 0 {
                    return Err("use an IPv4 address and port 1..65535".into());
                }
                Some(address)
            };
            paths.save_preferences(&prefs).map_err(|e| e.to_string())?;
            println!(
                "Receiver selection saved: {}. Restart sender to apply.",
                prefs.server.as_deref().unwrap_or("automatic discovery")
            );
        }
        Commands::Device {
            output,
            capture,
            reset,
        } => {
            let mut p = paths.preferences().map_err(|e| e.to_string())?;
            if reset {
                p.output_device = None;
                p.capture_device = None;
            }
            if output.is_some() {
                p.output_device = output;
            }
            if capture.is_some() {
                p.capture_device = capture;
            }
            paths.save_preferences(&p).map_err(|e| e.to_string())?;
            println!(
                "Output: {:?}; capture: {:?}. None uses default endpoint. Restart daemon to apply.",
                p.output_device, p.capture_device
            );
        }
        Commands::Volume {
            master,
            stream,
            gain,
        } => {
            if master.is_none() && (stream.is_none() || gain.is_none()) {
                return Err("use --master GAIN or --stream ID --gain GAIN".into());
            }
            for g in [master, gain].into_iter().flatten() {
                if !g.is_finite() || !(0.0..=2.0).contains(&g) {
                    return Err("gain must be finite and between 0 and 2".into());
                }
            }
            let r = control_request(
                &paths,
                &token(&paths)?,
                "volume",
                json!({"master":master,"stream":stream,"gain":gain}),
            )
            .await?;
            if !r.ok {
                return Err(r.body.to_string());
            }
            if let Some(g) = master {
                let mut p = paths.preferences().map_err(|e| e.to_string())?;
                p.master_gain = Some(g as f32);
                paths.save_preferences(&p).map_err(|e| e.to_string())?;
            }
            println!("Volume updated.");
        }
        Commands::Server {
            addr,
            name,
            device,
            no_discovery,
            null_playback,
        } => {
            token(&paths)?;
            let prefs = paths.preferences().map_err(|e| e.to_string())?;
            run_server(ServerOptions {
                paths,
                format,
                listen_addr: addr
                    .parse()
                    .map_err(|e: std::net::AddrParseError| e.to_string())?,
                name: name.unwrap_or_else(default_hostname),
                device_index: device.or(prefs.output_device),
                playback_backend: if null_playback {
                    PlaybackBackend::Null
                } else {
                    PlaybackBackend::Cpal
                },
                discovery: !no_discovery,
            })
            .await?;
        }
        Commands::Send {
            to,
            peer,
            source,
            name,
            device,
        } => {
            token(&paths)?;
            let prefs = paths.preferences().map_err(|e| e.to_string())?;
            let source = match source.as_str() {
                "loopback" => CaptureSource::Loopback,
                "mic" => CaptureSource::Microphone,
                "synth" => CaptureSource::Synthetic,
                _ => CaptureSource::Null,
            };
            let chosen_to = to.or(prefs.server);
            if let Some(ref target) = chosen_to {
                let endpoint: SocketAddr = target
                    .parse()
                    .map_err(|_| "--to requires a numeric IPv4:port; use --peer for a node name")?;
                if !endpoint.is_ipv4() || endpoint.port() == 0 {
                    return Err("--to requires IPv4 and port 1..65535".into());
                }
            }
            let peer = peer.or(PairingStore::new(&paths.root)
                .load()
                .map_err(|e| e.to_string())?
                .and_then(|p| p.receiver_name));
            let _clock = crate::audio::request_low_latency_clock();
            run_sender(SenderOptions {
                paths,
                format,
                to: chosen_to,
                peer,
                source,
                name: name.unwrap_or_else(default_hostname),
                device_index: device.or(prefs.capture_device),
            })
            .await?;
        }
    }
    Ok(())
}
fn default_hostname() -> String {
    hostname::get()
        .ok()
        .and_then(|h| h.into_string().ok())
        .unwrap_or_else(|| "remote-au".into())
}
