use remote_au::{
    mixer::Mixer,
    protocol::AudioFormat,
    transport::{ReceiverEngine, SenderEngine},
};
use std::time::{Duration, Instant};

#[test]
fn recorded_session_cannot_return_after_idle_expiry() {
    let token = [7; 32];
    let epoch = [3; 16];
    let address = "127.0.0.1:30123".parse().unwrap();
    let mut sender = SenderEngine::new(token, "test-source".into());
    sender.bind_peer(epoch, address, "test-receiver".into(), [4; 16]);
    let hello = sender.hello().unwrap();
    let audio = sender.audio(&vec![0; 960], 1000).unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::new(token, epoch);
    receiver.handle_datagram(&hello, address, &mixer);
    receiver.handle_datagram(&audio, address, &mixer);
    assert_eq!(receiver.peer_count(), 1);
    let before = receiver.feedback_due(Instant::now(), &mixer);
    let before_counter = remote_au::protocol::parse_packet(&before[0].1)
        .unwrap()
        .counter;
    receiver.expire(
        Duration::from_secs(10),
        Instant::now() + Duration::from_secs(11),
        &mixer,
    );
    assert_eq!(receiver.peer_count(), 0);
    receiver.handle_datagram(&hello, address, &mixer);
    receiver.handle_datagram(&audio, address, &mixer);
    assert_eq!(
        receiver.peer_count(),
        0,
        "expired sessions must retain replay tombstones until the receiver epoch changes"
    );
    assert!(mixer.stream_ids().is_empty());
    let resumed_epoch = receiver.epoch();
    sender.bind_peer(resumed_epoch, address, "receiver".into(), [4; 16]);
    receiver.handle_datagram(&sender.hello().unwrap(), address, &mixer);
    receiver.handle_datagram(&sender.audio(&vec![0; 960], 2000).unwrap(), address, &mixer);
    assert_eq!(
        receiver.peer_count(),
        1,
        "a fresh authenticated reconnect must recover without restarting the sender"
    );
    let after = receiver.feedback_due(Instant::now() + Duration::from_secs(1), &mixer);
    let after_counter = remote_au::protocol::parse_packet(&after[0].1)
        .unwrap()
        .counter;
    if resumed_epoch == epoch {
        assert!(
            after_counter > before_counter,
            "feedback nonce cannot restart under the same epoch/session key"
        );
    }
}

#[test]
fn reordered_audio_is_not_reported_as_packet_loss() {
    let token = [9; 32];
    let epoch = [8; 16];
    let address = "127.0.0.1:30124".parse().unwrap();
    let mut sender = SenderEngine::new(token, "reordered-source".into());
    sender.bind_peer(epoch, address, "receiver".into(), [2; 16]);
    let hello = sender.hello().unwrap();
    let a = sender.audio(&vec![0; 960], 5000).unwrap();
    let b = sender.audio(&vec![0; 960], 10000).unwrap();
    let c = sender.audio(&vec![0; 960], 15000).unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::new(token, epoch);
    for packet in [&hello, &a, &c, &b] {
        receiver.handle_datagram(packet, address, &mixer);
    }
    let feedback = receiver.feedback_due(Instant::now(), &mixer);
    assert_eq!(feedback.len(), 1);
    assert!(sender.handle_feedback(&feedback[0].1, 20000));
    assert_eq!(
        sender.snapshot().loss,
        0.0,
        "out of order packets arrived intact and must not force a bitrate downgrade"
    );
}
