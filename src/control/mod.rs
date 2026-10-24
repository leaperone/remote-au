use std::{path::Path, sync::Arc, time::Duration};

use blake2::{Blake2b512, Digest};
use serde::{Deserialize, Serialize};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, TcpStream},
    sync::Semaphore,
    time::timeout,
};

use crate::config::ConfigPaths;

const MAX_CONTROL_BYTES: u64 = 16384;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ControlRequest {
    pub token: String,
    pub command: String,
    pub args: serde_json::Value,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ControlResponse {
    pub ok: bool,
    pub body: serde_json::Value,
}

pub fn derive_control_token(invitation: &str) -> String {
    let mut hasher = Blake2b512::new();
    hasher.update(b"remote-au-local-control-v2");
    hasher.update(invitation.as_bytes());
    hex::encode(&hasher.finalize()[..32])
}

fn authorized(a: &str, b: &str) -> bool {
    use subtle::ConstantTimeEq;
    a.as_bytes().ct_eq(b.as_bytes()).into()
}

pub async fn run_control_server(
    paths: &ConfigPaths,
    invitation: String,
    handler: Arc<dyn Fn(ControlRequest) -> ControlResponse + Send + Sync>,
) -> Result<(), std::io::Error> {
    let endpoint_file = paths.root.join("control.json");
    if let Ok(old) = read_endpoint(&endpoint_file) {
        if timeout(Duration::from_millis(300), TcpStream::connect(old))
            .await
            .is_ok_and(|r| r.is_ok())
        {
            return Err(std::io::Error::new(
                std::io::ErrorKind::AddrInUse,
                "a daemon already owns this config directory",
            ));
        }
    }
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let endpoint = listener.local_addr()?;
    crate::config::private_write(&endpoint_file, &serde_json::to_vec(&endpoint.to_string())?)?;
    let expected = derive_control_token(&invitation);
    let slots = Arc::new(Semaphore::new(16));
    loop {
        let (stream, _) = listener.accept().await?;
        let Ok(slot) = slots.clone().try_acquire_owned() else {
            continue;
        };
        let expected = expected.clone();
        let handler = handler.clone();
        tokio::spawn(async move {
            let _slot = slot;
            let _ = timeout(Duration::from_secs(3), async {
                let (read, mut write) = stream.into_split();
                let mut line = String::new();
                let mut reader = BufReader::new(read.take(MAX_CONTROL_BYTES + 1));
                reader.read_line(&mut line).await?;
                if line.len() as u64 > MAX_CONTROL_BYTES || !line.ends_with('\n') {
                    return Ok::<(), std::io::Error>(());
                }
                let resp = match serde_json::from_str::<ControlRequest>(&line) {
                    Ok(req) if authorized(&req.token, &expected) => handler(req),
                    _ => ControlResponse {
                        ok: false,
                        body: serde_json::json!({"error":"unauthorized or malformed request"}),
                    },
                };
                write
                    .write_all(format!("{}\n", serde_json::to_string(&resp)?).as_bytes())
                    .await?;
                Ok(())
            })
            .await;
        });
    }
}

fn read_endpoint(path: &Path) -> Result<std::net::SocketAddr, std::io::Error> {
    let addr: String = serde_json::from_slice(&std::fs::read(path)?)?;
    let addr: std::net::SocketAddr = addr.parse().map_err(|_| {
        std::io::Error::new(std::io::ErrorKind::InvalidData, "invalid control endpoint")
    })?;
    if !addr.ip().is_loopback() {
        return Err(std::io::Error::new(
            std::io::ErrorKind::PermissionDenied,
            "control endpoint must be loopback",
        ));
    }
    Ok(addr)
}

pub async fn control_request(
    paths: &ConfigPaths,
    invitation: &str,
    command: &str,
    args: serde_json::Value,
) -> Result<ControlResponse, String> {
    timeout(Duration::from_secs(4), async {
        let endpoint = read_endpoint(&paths.root.join("control.json"))
            .map_err(|e| format!("daemon control unavailable: {e}"))?;
        let mut stream = TcpStream::connect(endpoint)
            .await
            .map_err(|e| format!("daemon is not running: {e}"))?;
        let req = ControlRequest {
            token: derive_control_token(invitation),
            command: command.into(),
            args,
        };
        stream
            .write_all(
                format!(
                    "{}\n",
                    serde_json::to_string(&req).map_err(|e| e.to_string())?
                )
                .as_bytes(),
            )
            .await
            .map_err(|e| e.to_string())?;
        let mut line = String::new();
        BufReader::new(stream.take(1024 * 1024))
            .read_line(&mut line)
            .await
            .map_err(|e| e.to_string())?;
        serde_json::from_str(&line).map_err(|e| e.to_string())
    })
    .await
    .map_err(|_| "daemon control timed out".to_string())?
}

pub fn localhost_only_addr() -> &'static str {
    "127.0.0.1"
}
