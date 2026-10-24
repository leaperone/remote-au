use remote_au::audio::{CaptureSource, PlaybackBackend};
use remote_au::config::ConfigPaths;
use remote_au::crypto::{generate_invitation_token, parse_invitation, random_id};
use remote_au::daemon::{run_sender, run_server, SenderOptions, ServerOptions};
use remote_au::mixer::{JitterBuffer, Mixer};
use remote_au::protocol::{
    decode_canonical, encode_canonical, AudioFormat, CODEC_ADPCM24, CODEC_ADPCM48, CODEC_PCM,
    MAX_PCM_CHUNK,
};
use remote_au::transport::{
    advance_pace, highest_sustainable_step, next_pace, plan_probe, AdaptState, PathSample, Profile,
    ReceiverAdapt, ReceiverEngine, SenderEngine, MAX_PROBE_BYTES,
};
use std::time::{Duration, Instant};

fn pcm_constant(sample: i16) -> Vec<u8> {
    let bytes = sample.to_le_bytes();
    let mut pcm = vec![0u8; MAX_PCM_CHUNK];
    for frame in 0..240 {
        let at = frame * 4;
        pcm[at..at + 2].copy_from_slice(&bytes);
        pcm[at + 2..at + 4].copy_from_slice(&bytes);
    }
    pcm
}

fn pcm_sine() -> Vec<u8> {
    let mut pcm = vec![0u8; MAX_PCM_CHUNK];
    for frame in 0..240 {
        let t = frame as f32 / 48_000.0;
        let sample = ((std::f32::consts::TAU * 440.0 * t).sin() * 8_000.0) as i16;
        let bytes = sample.to_le_bytes();
        let at = frame * 4;
        pcm[at..at + 2].copy_from_slice(&bytes);
        pcm[at + 2..at + 4].copy_from_slice(&bytes);
    }
    pcm
}

fn read_sample(pcm: &[u8], frame: usize, channel: usize) -> i16 {
    let at = (frame * 2 + channel) * 2;
    i16::from_le_bytes([pcm[at], pcm[at + 1]])
}

fn mean_abs(left: &[u8], right: &[u8]) -> (i32, i64) {
    let mut max = 0i32;
    let mut sum = 0i64;
    let mut count = 0i64;
    for frame in 0..240 {
        for channel in 0..2 {
            let err = i32::from(read_sample(left, frame, channel))
                - i32::from(read_sample(right, frame, channel));
            max = max.max(err.abs());
            sum += i64::from(err.abs());
            count += 1;
        }
    }
    (max, sum / count)
}

#[test]
fn codecs_roundtrip_against_literal_pcm() {
    let silence = pcm_constant(0);
    for codec in [CODEC_PCM, CODEC_ADPCM48, CODEC_ADPCM24] {
        let decoded = decode_canonical(codec, &encode_canonical(codec, &silence).unwrap()).unwrap();
        assert_eq!(decoded, silence, "codec {codec}");
    }
    let constant = pcm_constant(10_000);
    for codec in [CODEC_ADPCM48, CODEC_ADPCM24] {
        let encoded = encode_canonical(codec, &constant).unwrap();
        let decoded = decode_canonical(codec, &encoded).unwrap();
        let (max, mean) = mean_abs(&constant, &decoded);
        assert!(max < 80 && mean < 20, "codec {codec} max {max} mean {mean}");
    }
    let sine = pcm_sine();
    let decoded = decode_canonical(
        CODEC_ADPCM48,
        &encode_canonical(CODEC_ADPCM48, &sine).unwrap(),
    )
    .unwrap();
    let (max, mean) = mean_abs(&sine, &decoded);
    assert!(max < 4_000 && mean < 800, "sine max {max} mean {mean}");
    let pcm = encode_canonical(CODEC_PCM, &sine).unwrap();
    assert_eq!(decode_canonical(CODEC_PCM, &pcm).unwrap(), sine);

    let second = pcm_constant(3_000);
    let encoded = encode_canonical(CODEC_ADPCM48, &second).unwrap();
    let alone = decode_canonical(CODEC_ADPCM48, &encoded).unwrap();
    let (max, _) = mean_abs(&second, &alone);
    assert!(max < 80, "packet-local reset drifted by {max}");
}

#[test]
fn jitter_reorders_without_underflow() {
    let mut jitter = JitterBuffer::new(2, 2);
    jitter.set_target_packets(2);
    let first = 1i16.to_le_bytes().repeat(2);
    let second = 3i16.to_le_bytes().repeat(2);
    assert!(jitter.push(2, &second));
    assert!(jitter.push(1, &first));
    let mut out = [0u8; 4];
    assert_eq!(jitter.pull(&mut out, 2), 2);
    assert_eq!(&out[..2], &1i16.to_le_bytes());
    assert_eq!(jitter.pull(&mut out, 2), 2);
    assert_eq!(&out[..2], &3i16.to_le_bytes());

    let mut loss = JitterBuffer::new(2, 2);
    loss.set_target_packets(1);
    assert!(loss.push(1, &first));
    assert!(loss.push(3, &second));
    assert_eq!(loss.pull(&mut out, 2), 2);
    assert_eq!(&out[..2], &1i16.to_le_bytes());
    assert_eq!(loss.pull(&mut out, 2), 2);
    assert_eq!(out, [0, 0, 0, 0]);
    assert_eq!(loss.pull(&mut out, 2), 2);
    assert_eq!(&out[..2], &3i16.to_le_bytes());

    let mut empty = JitterBuffer::new(2, 2);
    for _ in 0..1_000 {
        assert_eq!(empty.pull(&mut out, 8), 2);
        assert_eq!(out, [0, 0, 0, 0]);
    }
    assert!(empty.push(1, &first));

    let mut bounded = JitterBuffer::new(2, 1);
    bounded.set_target_packets(1);
    let one = 4i16.to_le_bytes();
    for seq in 1..100 {
        bounded.push(seq, &one);
    }
    assert!(bounded.buffered_packets() <= 32);
    let mut tiny = [0u8; 2];
    bounded.pull(&mut tiny, 50);
}

#[test]
fn adaptation_degrades_and_recovers_inside_buffer_bounds() {
    let mut state = AdaptState::start();
    state.observe(PathSample {
        loss: 0.2,
        rtt_us: 2_000,
        jitter_us: 1_000,
        underruns: 0,
    });
    assert_eq!(state.profile, Profile::Adpcm48);
    state.observe(PathSample {
        loss: 0.2,
        rtt_us: 2_000,
        jitter_us: 1_000,
        underruns: 0,
    });
    assert_eq!(state.profile, Profile::Adpcm24);

    let mut mild = AdaptState::start();
    mild.observe(PathSample {
        loss: 0.05,
        rtt_us: 0,
        jitter_us: 0,
        underruns: 0,
    });
    assert_eq!(mild.profile, Profile::Pcm);
    mild.observe(PathSample {
        loss: 0.05,
        rtt_us: 0,
        jitter_us: 0,
        underruns: 0,
    });
    assert_eq!(mild.profile, Profile::Adpcm48);

    let mut wide = AdaptState::start();
    wide.observe(PathSample {
        loss: 0.0,
        rtt_us: 10_000_000,
        jitter_us: 10_000_000,
        underruns: 8,
    });
    assert!((15..=150).contains(&wide.target_buffer_ms));

    let mut recover = AdaptState::start();
    recover.apply_floor(None, false);
    assert_eq!(recover.profile, Profile::Adpcm24);
    recover.apply_floor(Some(8_000_000), true);
    assert_eq!(recover.profile, Profile::Adpcm48);
    recover.apply_floor(Some(8_000_000), true);
    assert_eq!(recover.profile, Profile::Pcm);

    let mut poor = AdaptState::start();
    poor.apply_floor(Some(500_000), true);
    assert_eq!(poor.profile, Profile::Adpcm24);

    let floor = highest_sustainable_step(&[
        (8_000_000, 1000, 400),
        (2_000_000, 1000, 1000),
        (4_000_000, 1000, 100),
    ]);
    assert_eq!(floor, Some(2_000_000));
    let plan = plan_probe(MAX_PROBE_BYTES);
    let bytes: usize = plan.iter().map(|step| step.datagram_len).sum();
    assert!(bytes <= MAX_PROBE_BYTES);
    assert!(plan.iter().all(|step| step.delay_after > Duration::ZERO));
    assert!(plan_probe(1_000).len() < plan.len());

    assert_eq!(next_pace(1_000, 1_000), 6_000);
    assert_eq!(next_pace(0, 20_000), 25_000);
    let start = Instant::now();
    assert_eq!(advance_pace(start, start), start + Duration::from_millis(5));
    assert_eq!(
        advance_pace(start, start + Duration::from_millis(20)),
        start + Duration::from_millis(25)
    );

    let mut playout = ReceiverAdapt::new();
    assert!(playout.observe(0, 4) >= 30);
    let mut calm = ReceiverAdapt::new();
    let mut target = 20;
    for _ in 0..3 {
        target = calm.observe(0, 0);
    }
    assert_eq!(
        target, 20,
        "stable links retain the 20 ms callback-safe minimum"
    );
}

#[tokio::test]
async fn udp_roundtrip_feedback_and_daemon_rejection() {
    use std::sync::Arc;
    use tokio::net::UdpSocket;
    use tokio::time::timeout;

    let invitation = parse_invitation(&generate_invitation_token()).unwrap();
    let epoch = random_id();
    let mixer = Arc::new(Mixer::new(AudioFormat::canonical()));
    mixer.set_target_packets(1);
    let mut receiver = ReceiverEngine::new(invitation, epoch);
    let socket = UdpSocket::bind("127.0.0.1:0").await.unwrap();
    let peer = socket.local_addr().unwrap();
    let client = UdpSocket::bind("127.0.0.1:0").await.unwrap();
    let mut sender = SenderEngine::new(invitation, "mic".into());
    sender.bind_peer(epoch, peer, "rx".into(), [5u8; 16]);
    client
        .send_to(&sender.hello().unwrap(), peer)
        .await
        .unwrap();
    client
        .send_to(&sender.audio(&pcm_constant(12_000), 50_000).unwrap(), peer)
        .await
        .unwrap();
    let mut buf = [0u8; 2048];
    for _ in 0..2 {
        let (len, src) = timeout(Duration::from_secs(1), socket.recv_from(&mut buf))
            .await
            .unwrap()
            .unwrap();
        receiver.handle_datagram(&buf[..len], src, &mixer);
    }
    assert_eq!(receiver.peer_count(), 1);
    let mut out = vec![0u8; MAX_PCM_CHUNK];
    mixer.pull(&mut out, 240);
    assert!(out.iter().any(|byte| *byte != 0));
    let feedback = receiver.feedback_due(Instant::now(), &mixer);
    assert_eq!(feedback.len(), 1);
    assert!(sender.handle_feedback(&feedback[0].1, 80_000));
    assert!(!sender.handle_feedback(&feedback[0].1, 80_000));

    let dir = tempfile::tempdir().unwrap();
    let paths = ConfigPaths {
        root: dir.path().to_path_buf(),
    };
    let bad = AudioFormat {
        sample_rate: 44_100,
        channels: 1,
        frame_samples: 240,
    };
    let err = run_server(ServerOptions {
        paths: paths.clone(),
        format: bad,
        listen_addr: "127.0.0.1:0".parse().unwrap(),
        name: "rx".into(),
        device_index: None,
        playback_backend: PlaybackBackend::Null,
        discovery: false,
    })
    .await
    .unwrap_err();
    assert!(err.contains("48000"), "{err}");
    let err = run_sender(SenderOptions {
        paths,
        format: AudioFormat::canonical(),
        to: None,
        peer: None,
        source: CaptureSource::Null,
        name: "tx".into(),
        device_index: None,
    })
    .await
    .unwrap_err();
    assert!(err.contains("paired"), "{err}");
}
