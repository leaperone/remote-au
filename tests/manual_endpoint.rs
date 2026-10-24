use std::{
    fs,
    net::UdpSocket,
    path::Path,
    process::{Child, Command, Stdio},
    thread,
    time::{Duration, Instant},
};

struct Process(Child);
impl Drop for Process {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
fn command(config: &Path, args: &[&str]) -> std::process::Output {
    Command::new(env!("CARGO_BIN_EXE_remote-au"))
        .arg("--config-dir")
        .arg(config)
        .args(args)
        .output()
        .unwrap()
}
fn spawn(config: &Path, args: &[&str], log: &Path) -> Process {
    Process(
        Command::new(env!("CARGO_BIN_EXE_remote-au"))
            .arg("--config-dir")
            .arg(config)
            .args(args)
            .stdout(Stdio::null())
            .stderr(Stdio::from(fs::File::create(log).unwrap()))
            .spawn()
            .unwrap(),
    )
}
#[test]
fn pinned_sender_works_when_broadcast_discovery_is_disabled() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path();
    let receiver = root.join("receiver");
    let sender = root.join("sender");
    let invitation = root.join("invite.json");
    assert!(command(&receiver, &["pair"]).status.success());
    assert!(command(
        &receiver,
        &["pair", "--export-file", invitation.to_str().unwrap()]
    )
    .status
    .success());
    assert!(command(
        &sender,
        &["pair", "--import-file", invitation.to_str().unwrap()]
    )
    .status
    .success());
    let port = UdpSocket::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let target = format!("127.0.0.1:{port}");
    let receiver_log = root.join("receiver.log");
    let sender_log = root.join("sender.log");
    let _r = spawn(
        &receiver,
        &[
            "server",
            "--addr",
            &target,
            "--null-playback",
            "--no-discovery",
        ],
        &receiver_log,
    );
    let _s = spawn(
        &sender,
        &["send", "--to", &target, "--source", "synth"],
        &sender_log,
    );
    let end = Instant::now() + Duration::from_secs(10);
    while Instant::now() < end {
        thread::sleep(Duration::from_millis(200));
        let result = command(&receiver, &["status"]);
        if let Ok(value) = serde_json::from_slice::<serde_json::Value>(&result.stdout) {
            if value["streams"].as_array().is_some_and(|streams| {
                streams
                    .iter()
                    .any(|s| s["packets"].as_u64().unwrap_or(0) > 10)
            }) {
                return;
            }
        }
    }
    panic!("pinned paired sender must authenticate over the specified audio endpoint even with broadcast discovery disabled. receiver={} sender={}",fs::read_to_string(receiver_log).unwrap_or_default(),fs::read_to_string(sender_log).unwrap_or_default());
}
