use remote_au::crypto::{generate_invitation_token, parse_invitation, random_id};
use remote_au::discovery::{
    contact_addr, dedup_prefer_direct, directed_query_targets, encode_query, local_lan_v4,
    open_announce, respond_to_query, Announce,
};
use remote_au::mixer::Mixer;
use remote_au::protocol::{AudioFormat, MAX_STREAMS};
use remote_au::transport::{ReceiverEngine, SenderEngine};
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::time::Duration;

fn token() -> [u8; 32] {
    parse_invitation(&generate_invitation_token()).unwrap()
}

fn addr() -> SocketAddr {
    "192.168.2.20:47000".parse().unwrap()
}

fn tone() -> Vec<u8> {
    let mut pcm = vec![0u8; 960];
    for frame in 0..240 {
        let sample = (8_000i16).to_le_bytes();
        let at = frame * 4;
        pcm[at..at + 2].copy_from_slice(&sample);
        pcm[at + 2..at + 4].copy_from_slice(&sample);
    }
    pcm
}

fn pair(token: [u8; 32], epoch: [u8; 16], name: &str) -> (SenderEngine, ReceiverEngine, Mixer) {
    let mixer = Mixer::new(AudioFormat::canonical());
    mixer.set_target_packets(1);
    let mut sender = SenderEngine::new(token, name.into());
    sender.bind_peer(epoch, addr(), "rx".into(), [4u8; 16]);
    let receiver = ReceiverEngine::new(token, epoch);
    (sender, receiver, mixer)
}

#[test]
fn hello_is_required_and_replay_follows_authentication() {
    let epoch = random_id();
    let (mut sender, mut receiver, mixer) = pair(token(), epoch, "mic");
    let early = sender.audio(&tone(), 1_000).unwrap();
    receiver.handle_datagram(&early, addr(), &mixer);
    assert_eq!(receiver.peer_count(), 0);

    let hello = sender.hello().unwrap();
    receiver.handle_datagram(&hello, addr(), &mixer);
    receiver.handle_datagram(&hello, addr(), &mixer);
    assert_eq!(mixer.stream_ids().len(), 1);

    let audio = sender.audio(&tone(), 2_000).unwrap();
    receiver.handle_datagram(&audio, addr(), &mixer);
    let accepted = receiver.packets_received;
    let mut tampered = audio.clone();
    tampered[70] ^= 0xff;
    receiver.handle_datagram(&tampered, addr(), &mixer);
    assert!(receiver.packets_rejected > 0);
    let next = sender.audio(&tone(), 3_000).unwrap();
    receiver.handle_datagram(&next, addr(), &mixer);
    assert!(receiver.packets_received > accepted);
    let after = receiver.packets_received;
    receiver.handle_datagram(&next, addr(), &mixer);
    assert_eq!(receiver.packets_received, after);
    let _ = sender;
}

#[test]
fn multipeer_limit_and_restart_epoch() {
    let invitation = token();
    let epoch = random_id();
    let mixer = Mixer::new(AudioFormat::canonical());
    mixer.set_target_packets(1);
    let mut receiver = ReceiverEngine::new(invitation, epoch);
    for index in 0..MAX_STREAMS {
        let mut sender = SenderEngine::new(invitation, format!("p{index}"));
        sender.bind_peer(epoch, addr(), "rx".into(), [9u8; 16]);
        receiver.handle_datagram(&sender.hello().unwrap(), addr(), &mixer);
    }
    assert_eq!(receiver.peer_count(), MAX_STREAMS);
    let mut extra = SenderEngine::new(invitation, "extra".into());
    extra.bind_peer(epoch, addr(), "rx".into(), [9u8; 16]);
    let rejected = receiver.packets_rejected;
    receiver.handle_datagram(&extra.hello().unwrap(), addr(), &mixer);
    assert_eq!(receiver.peer_count(), MAX_STREAMS);
    assert!(receiver.packets_rejected > rejected);

    let mut live = SenderEngine::new(invitation, "live".into());
    live.bind_peer(epoch, addr(), "rx".into(), [9u8; 16]);
    let packet = live.hello().unwrap();
    let mut restarted = ReceiverEngine::new(invitation, random_id());
    let fresh = Mixer::new(AudioFormat::canonical());
    restarted.handle_datagram(&packet, addr(), &fresh);
    assert_eq!(restarted.peer_count(), 0);
    assert!(fresh.stream_ids().is_empty());
}

#[test]
fn hostile_datagrams_create_no_state() {
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::new(token(), random_id());
    let samples: [Vec<u8>; 4] = [Vec::new(), vec![0], vec![1u8; 2000], vec![9u8; 300]];
    for packet in samples {
        receiver.handle_datagram(&packet, addr(), &mixer);
    }
    assert_eq!(receiver.peer_count(), 0);
    assert!(receiver.packets_rejected >= 4);
}

#[test]
fn discovery_authenticates_challenge_and_prefers_direct_source() {
    let invitation = generate_invitation_token();
    let query = encode_query("office", &invitation).unwrap();
    let announce = Announce {
        tcp_port: 47000,
        instance_id: [3u8; 16],
        advertised_addr: Ipv4Addr::LOCALHOST,
        name: "office".into(),
        epoch: [9u8; 16],
    };
    let response = respond_to_query(&query.bytes, &invitation, &announce).unwrap();
    let opened = open_announce(&response, &invitation, &query.challenge).unwrap();
    assert_eq!(opened.name, "office");
    let mut tampered = response.clone();
    tampered[40] ^= 1;
    assert!(open_announce(&tampered, &invitation, &query.challenge).is_none());
    let mut wrong = [0u8; 16];
    wrong[0] = 1;
    assert!(open_announce(&response, &invitation, &wrong).is_none());
    assert!(open_announce(&response, &generate_invitation_token(), &query.challenge).is_none());
    let other = Announce {
        name: "other".into(),
        ..announce.clone()
    };
    assert!(respond_to_query(&query.bytes, &invitation, &other).is_none());

    let source = SocketAddr::from((Ipv4Addr::new(192, 168, 2, 9), 40000));
    assert_eq!(
        contact_addr(&opened, source),
        SocketAddr::from((Ipv4Addr::new(192, 168, 2, 9), 47000))
    );
    let ranked = dedup_prefer_direct(vec![
        (opened.clone(), SocketAddr::from((Ipv4Addr::LOCALHOST, 1))),
        (opened, source),
    ]);
    assert_eq!(ranked[0].1.ip(), IpAddr::V4(Ipv4Addr::new(192, 168, 2, 9)));

    let targets = directed_query_targets(&[]);
    assert_eq!(
        targets[0],
        SocketAddr::from((Ipv4Addr::new(192, 168, 2, 255), 47001))
    );
    assert!(targets
        .iter()
        .any(|item| item.ip() == IpAddr::V4(Ipv4Addr::new(192, 168, 1, 255))));
    assert!(targets
        .iter()
        .any(|item| item.ip() == IpAddr::V4(Ipv4Addr::BROADCAST)));
    assert!(targets
        .iter()
        .any(|item| item.ip() == IpAddr::V4(Ipv4Addr::LOCALHOST)));
    assert!(!local_lan_v4().is_loopback());
    let _ = Duration::from_millis(1);
}

#[tokio::test]
async fn localhost_discovery_replies_to_the_source() {
    use tokio::net::UdpSocket;
    let invitation = generate_invitation_token();
    let query = encode_query("", &invitation).unwrap();
    let listener = UdpSocket::bind("127.0.0.1:0").await.unwrap();
    listener.set_broadcast(true).unwrap();
    let client = UdpSocket::bind("127.0.0.1:0").await.unwrap();
    client.set_broadcast(true).unwrap();
    client
        .send_to(&query.bytes, listener.local_addr().unwrap())
        .await
        .unwrap();
    let mut buf = [0u8; 1024];
    let (len, src) = listener.recv_from(&mut buf).await.unwrap();
    let template = Announce {
        tcp_port: 47000,
        instance_id: [8u8; 16],
        advertised_addr: Ipv4Addr::new(10, 1, 2, 3),
        name: "desk".into(),
        epoch: [4u8; 16],
    };
    let response = respond_to_query(&buf[..len], &invitation, &template).unwrap();
    listener.send_to(&response, src).await.unwrap();
    let (len, from) = client.recv_from(&mut buf).await.unwrap();
    let announce = open_announce(&buf[..len], &invitation, &query.challenge).unwrap();
    let contact = contact_addr(&announce, from);
    assert_eq!(contact.ip(), from.ip());
    assert_ne!(contact.ip(), IpAddr::V4(Ipv4Addr::new(10, 1, 2, 3)));
    assert_eq!(contact.port(), 47000);
}
