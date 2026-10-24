use remote_au::crypto::random_id;
use remote_au::mixer::Mixer;
use remote_au::protocol::AudioFormat;
use remote_au::transport::{ReceiverEngine, SenderEngine, MAX_SESSION_HISTORY};
use std::time::{Duration, Instant};

const TOKEN: [u8; 32] = [5; 32];

#[test]
fn max_session_history_constant_is_4096() {
    assert_eq!(MAX_SESSION_HISTORY, 4096);
}

#[test]
fn full_history_rotates_epoch_on_new_authenticated_hello() {
    let epoch = random_id();
    let cap = 4usize;
    let addr = "127.0.0.1:40200".parse().unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::with_history_cap(TOKEN, epoch, cap);

    for i in 0..cap {
        let mut sender = SenderEngine::new(TOKEN, format!("peer-{i}"));
        let session = sender.session();
        sender.bind_peer(epoch, addr, "rx".into(), [9; 16]);
        receiver.handle_datagram(&sender.hello().unwrap(), addr, &mixer);
        receiver.expire(
            Duration::from_secs(1),
            Instant::now() + Duration::from_secs(2),
            &mixer,
        );
        assert_eq!(receiver.history_len(), i + 1);
        let _ = session;
    }
    assert_eq!(receiver.history_len(), cap);

    let mut newcomer = SenderEngine::new(TOKEN, "new-peer".into());
    newcomer.bind_peer(epoch, addr, "rx".into(), [8; 16]);
    let before = receiver.epoch();
    receiver.handle_datagram(&newcomer.hello().unwrap(), addr, &mixer);
    let after = receiver.epoch();
    assert_ne!(before, after);
    assert_eq!(receiver.history_len(), 0);
    assert_eq!(receiver.peer_count(), 0);
    assert!(mixer.stream_ids().is_empty());

    newcomer.bind_peer(after, addr, "rx".into(), [8; 16]);
    receiver.handle_datagram(&newcomer.hello().unwrap(), addr, &mixer);
    assert_eq!(receiver.peer_count(), 1);
}

#[test]
fn unauth_bogus_hello_at_cap_does_not_rotate() {
    let epoch = [11u8; 16];
    let cap = 3usize;
    let addr = "127.0.0.1:40201".parse().unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::with_history_cap(TOKEN, epoch, cap);

    for i in 0..cap {
        let mut sender = SenderEngine::new(TOKEN, format!("p{i}"));
        sender.bind_peer(epoch, addr, "rx".into(), [7; 16]);
        receiver.handle_datagram(&sender.hello().unwrap(), addr, &mixer);
        receiver.expire(
            Duration::from_secs(1),
            Instant::now() + Duration::from_secs(2),
            &mixer,
        );
    }
    assert_eq!(receiver.history_len(), cap);

    let garbage = [0u8; 120];
    receiver.handle_datagram(&garbage, addr, &mixer);
    assert_eq!(receiver.epoch(), epoch);
    assert_eq!(receiver.history_len(), cap);
}

#[test]
fn expired_same_session_resumes_with_counter_retained() {
    let epoch = [12u8; 16];
    let addr = "127.0.0.1:40202".parse().unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::new(TOKEN, epoch);
    let mut sender = SenderEngine::new(TOKEN, "resume".into());
    sender.bind_peer(epoch, addr, "rx".into(), [4; 16]);
    let hello = sender.hello().unwrap();
    receiver.handle_datagram(&hello, addr, &mixer);
    receiver.handle_datagram(&sender.audio(&vec![0; 960], 1000).unwrap(), addr, &mixer);
    let before = receiver
        .feedback_due(Instant::now(), &mixer)
        .first()
        .map(|(_, p)| remote_au::protocol::parse_packet(p).unwrap().counter)
        .unwrap();
    receiver.expire(
        Duration::from_secs(5),
        Instant::now() + Duration::from_secs(6),
        &mixer,
    );
    assert_eq!(receiver.peer_count(), 0);
    receiver.handle_datagram(&sender.hello().unwrap(), addr, &mixer);
    receiver.handle_datagram(&sender.audio(&vec![0; 960], 2000).unwrap(), addr, &mixer);
    assert_eq!(receiver.peer_count(), 1);
    let after = receiver
        .feedback_due(Instant::now() + Duration::from_secs(1), &mixer)
        .first()
        .map(|(_, p)| remote_au::protocol::parse_packet(p).unwrap().counter)
        .unwrap();
    assert!(after > before);
}

#[test]
fn history_map_never_exceeds_cap() {
    let epoch = [13u8; 16];
    let cap = 6usize;
    let addr = "127.0.0.1:40203".parse().unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::with_history_cap(TOKEN, epoch, cap);
    for i in 0..cap + 2 {
        let mut sender = SenderEngine::new(TOKEN, format!("h{i}"));
        sender.bind_peer(receiver.epoch(), addr, "rx".into(), [6; 16]);
        receiver.handle_datagram(&sender.hello().unwrap(), addr, &mixer);
        receiver.expire(
            Duration::from_millis(100),
            Instant::now() + Duration::from_millis(200),
            &mixer,
        );
        assert!(receiver.history_len() <= cap);
    }
}
