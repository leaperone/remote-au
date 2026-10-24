use remote_au::crypto::{generate_invitation_token, parse_invitation, DirectionKeys, ReplayWindow};
use remote_au::mixer::Mixer;
use remote_au::protocol::{
    decode_canonical, encode_canonical, split_pcm, AudioFormat, CODEC_ADPCM48, CODEC_PCM,
    MAX_DATAGRAM, MAX_PCM_CHUNK,
};
use remote_au::transport::SenderEngine;
use std::net::SocketAddr;

#[test]
fn pcm_and_adpcm_fit_the_datagram() {
    let pcm = vec![0u8; MAX_PCM_CHUNK];
    let pcm_payload = encode_canonical(CODEC_PCM, &pcm).unwrap();
    assert_eq!(pcm_payload, pcm);
    let adpcm = encode_canonical(CODEC_ADPCM48, &pcm).unwrap();
    assert!(adpcm.len() < MAX_PCM_CHUNK);
    assert_eq!(decode_canonical(CODEC_PCM, &pcm_payload).unwrap(), pcm);

    let token = parse_invitation(&generate_invitation_token()).unwrap();
    let mut sender = SenderEngine::new(token, "node".into());
    sender.bind_peer(
        [7u8; 16],
        "192.168.2.8:47000".parse().unwrap(),
        "rx".into(),
        [1u8; 16],
    );
    let packet = sender.audio(&pcm, 1_000).unwrap();
    assert!(packet.len() <= MAX_DATAGRAM);
}

#[test]
fn split_pcm_never_exceeds_960() {
    let pcm = vec![1u8; 1920];
    let chunks = split_pcm(&pcm);
    assert_eq!(chunks.len(), 2);
    assert!(chunks.iter().all(|chunk| chunk.len() <= MAX_PCM_CHUNK));
}

#[test]
fn replay_tombstones_consumed_sequences() {
    let mut window = ReplayWindow::new(64);
    assert!(window.commit(5));
    assert!(window.commit(1));
    assert!(!window.commit(5));
    assert!(window.commit(100_000));
    assert!(!window.commit(1));
    assert!(!window.commit(100_000));
}

#[test]
fn directions_epochs_and_peers_do_not_share_keys() {
    let token = parse_invitation(&generate_invitation_token()).unwrap();
    let epoch = [1u8; 16];
    let session = [2u8; 16];
    let keys = DirectionKeys::derive(&token, &epoch, &session);
    let sealed = keys.sender_to_receiver.seal(1, b"aad", b"pcm").unwrap();
    assert_eq!(
        keys.sender_to_receiver.open(1, b"aad", &sealed).unwrap(),
        b"pcm"
    );
    assert!(keys.sender_to_receiver.open(1, b"aad", &sealed).is_ok());
    assert!(keys.receiver_to_sender.open(1, b"aad", &sealed).is_err());
    let other_peer = DirectionKeys::derive(&token, &epoch, &[3u8; 16]);
    assert!(other_peer
        .sender_to_receiver
        .open(1, b"aad", &sealed)
        .is_err());
    let restarted = DirectionKeys::derive(&token, &[9u8; 16], &session);
    assert!(restarted
        .sender_to_receiver
        .open(1, b"aad", &sealed)
        .is_err());
    let mut tampered = sealed.clone();
    tampered[0] ^= 0x5a;
    assert!(keys.sender_to_receiver.open(1, b"aad", &tampered).is_err());
    let again = keys.sender_to_receiver.seal(2, b"aad", b"pcm").unwrap();
    assert_ne!(sealed, again);
}

#[test]
fn mixer_sums_two_streams_once() {
    let format = AudioFormat {
        sample_rate: 48_000,
        channels: 1,
        frame_samples: 1,
    };
    let mixer = Mixer::new(format);
    mixer.set_target_packets(1);
    mixer.ensure_stream("a", "a").unwrap();
    mixer.ensure_stream("a", "renamed").unwrap();
    mixer.ensure_stream("b", "b").unwrap();
    assert_eq!(mixer.stream_ids().len(), 2);
    let pcm = 1000i16.to_le_bytes();
    assert!(mixer.push_packet("a", 1, &pcm));
    assert!(mixer.push_packet("b", 1, &pcm));
    let mut out = [0u8; 2];
    mixer.pull(&mut out, 1);
    let mixed = i16::from_le_bytes(out);
    assert!((mixed - 2000).abs() < 5, "{mixed}");
    for index in 0..6 {
        mixer.ensure_stream(format!("s{index}"), "x").unwrap();
    }
    assert!(mixer.ensure_stream("overflow", "x").is_err());
    let _addr: SocketAddr = "127.0.0.1:1".parse().unwrap();
}
