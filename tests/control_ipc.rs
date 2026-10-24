use remote_au::{
    config::{private_write, ConfigPaths},
    control::{control_request, run_control_server, ControlResponse},
};
use std::{sync::Arc, time::Duration};

#[tokio::test]
async fn control_requires_authentication_and_survives_oversized_clients() {
    let dir = tempfile::tempdir().unwrap();
    let paths = ConfigPaths::resolve(Some(dir.path().join("config"))).unwrap();
    let p = paths.clone();
    let key = "11".repeat(32);
    let k = key.clone();
    let server = tokio::spawn(async move {
        run_control_server(
            &p,
            k,
            Arc::new(|req| ControlResponse {
                ok: true,
                body: serde_json::json!({"command":req.command,"answer":42}),
            }),
        )
        .await
    });
    for _ in 0..50 {
        if paths.root.join("control.json").exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let reply = control_request(&paths, &key, "status", serde_json::json!({}))
        .await
        .unwrap();
    assert!(reply.ok);
    assert_eq!(
        reply.body,
        serde_json::json!({"command":"status","answer":42})
    );
    assert!(
        !control_request(&paths, &"22".repeat(32), "status", serde_json::json!({}))
            .await
            .unwrap()
            .ok
    );
    let addr: String =
        serde_json::from_slice(&std::fs::read(paths.root.join("control.json")).unwrap()).unwrap();
    assert!(addr.starts_with("127.0.0.1:"));
    use tokio::io::AsyncWriteExt;
    let mut client = tokio::net::TcpStream::connect(addr).await.unwrap();
    let _ = client.write_all(&vec![b'x'; 32768]).await;
    drop(client);
    assert!(
        control_request(&paths, &key, "status", serde_json::json!({}))
            .await
            .unwrap()
            .ok
    );
    let duplicate = run_control_server(&paths, key, Arc::new(|_| unreachable!())).await;
    assert_eq!(duplicate.unwrap_err().kind(), std::io::ErrorKind::AddrInUse);
    server.abort();
}

#[test]
fn preferences_survive_reload_and_private_writes_replace_existing_data() {
    let dir = tempfile::tempdir().unwrap();
    let paths = ConfigPaths::resolve(Some(dir.path().join("config"))).unwrap();
    let mut p = paths.preferences().unwrap();
    assert_eq!(p.server, None);
    p.server = Some("192.168.2.20:47000".into());
    p.master_gain = Some(0.5);
    paths.save_preferences(&p).unwrap();
    let loaded = paths.preferences().unwrap();
    assert_eq!(loaded.server.as_deref(), Some("192.168.2.20:47000"));
    assert_eq!(loaded.master_gain, Some(0.5));
    let file = paths.root.join("private.json");
    private_write(&file, b"old").unwrap();
    private_write(&file, b"new").unwrap();
    assert_eq!(std::fs::read(&file).unwrap(), b"new");
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            std::fs::metadata(file).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(
            std::fs::metadata(&paths.root).unwrap().permissions().mode() & 0o777,
            0o700
        );
    }
}

#[test]
fn daemon_lock_prevents_duplicate_processes_and_releases_on_drop() {
    let dir = tempfile::tempdir().unwrap();
    let paths = ConfigPaths::resolve(Some(dir.path().join("config"))).unwrap();
    let guard = paths.acquire_daemon_lock().unwrap();
    assert!(paths.acquire_daemon_lock().is_err());
    drop(guard);
    assert!(paths.acquire_daemon_lock().is_ok());
}
