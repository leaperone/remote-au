use remote_au::crypto::random_id;
use remote_au::mixer::Mixer;
use remote_au::protocol::{decode_feedback, encode_feedback, AudioFormat, FEEDBACK_BODY_LEN};
use remote_au::transport::{
    effective_goodput_bps, highest_sustainable_step_timed, plan_probe, probe_wire_bytes,
    AdaptState, Profile, ReceiverEngine, SenderEngine, PROBE_PAYLOAD_BYTES, PROBE_STEPS_BPS,
};
use std::time::Instant;

const TOKEN: [u8; 32] = [4; 32];

#[test]
fn feedback_body_fits_datagram_budget() {
    let body = encode_feedback(&remote_au::protocol::FeedbackBody {
        echo_send_ts_us: 1,
        delay_us: 2,
        echo_seq: 3,
        received: 4,
        lost: 5,
        jitter_us: 6,
        underruns: 7,
        buffer_ms: 20,
        probe_gen: 1,
        probe_bytes: [800; 5],
        probe_recv_span_us: [50_000; 5],
    });
    assert_eq!(body.len(), FEEDBACK_BODY_LEN);
    assert!(46 + 16 + body.len() <= 1200);
    assert_eq!(
        decode_feedback(&body).unwrap().probe_recv_span_us,
        [50_000; 5]
    );
}

#[test]
fn delayed_probe_cannot_claim_eight_megabit_floor() {
    let payload = 20 * PROBE_PAYLOAD_BYTES;
    let wire = probe_wire_bytes(payload as u32);
    let slow_us = 200_000u32;
    let slow_bps = effective_goodput_bps(wire, slow_us);
    assert!(slow_bps < 2_000_000, "slow_bps={slow_bps}");
    let floor = highest_sustainable_step_timed(&[(
        8_000_000,
        payload as u32,
        payload as u32,
        slow_us,
        slow_us,
    )]);
    assert!(floor.unwrap_or(0) < 2_000_000);
}

#[test]
fn good_link_promotes_pcm_bad_link_degrades_and_recovers() {
    let mut adapt = AdaptState::start();
    adapt.apply_floor(Some(8_000_000), false);
    assert_eq!(adapt.profile, Profile::Pcm);

    adapt.apply_floor(Some(500_000), false);
    assert_eq!(adapt.profile, Profile::Adpcm24);

    adapt.apply_floor(Some(8_000_000), true);
    assert_eq!(adapt.profile, Profile::Adpcm48);
    adapt.apply_floor(Some(8_000_000), true);
    assert_eq!(adapt.profile, Profile::Pcm);
}

#[test]
fn probe_train_end_to_end_respects_actual_send_and_receive_rates() {
    let epoch = random_id();
    let addr = "127.0.0.1:40111".parse().unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::new(TOKEN, epoch);
    let mut sender = SenderEngine::new(TOKEN, "probe-tx".into());
    sender.bind_peer(epoch, addr, "rx".into(), [1; 16]);
    sender.begin_probe();

    receiver.handle_datagram(&sender.hello().unwrap(), addr, &mixer);
    assert_eq!(receiver.peer_count(), 1);

    let plan = plan_probe(128 * 1024);
    let mut sent = [0u32; 5];
    let mut now = 1_000_000u64;
    for spec in &plan {
        let step = usize::from(spec.step);
        if let Some(pkt) = sender.probe(spec.step, spec.payload, now) {
            receiver.handle_datagram(&pkt, addr, &mixer);
            sent[step] = sent[step].saturating_add(spec.payload as u32);
        }
        now += spec.delay_after.as_micros() as u64;
    }

    let feedback = receiver.feedback_due(Instant::now(), &mixer);
    assert_eq!(feedback.len(), 1);
    sender.handle_feedback(&feedback[0].1, now + 5_000);
    sender.note_probe(sent, false);
    let floor = sender.snapshot().estimated_goodput_bps;
    assert!(floor >= PROBE_STEPS_BPS[4], "fast local loop floor={floor}");

    let mut slow = SenderEngine::new(TOKEN, "slow".into());
    slow.bind_peer(epoch, addr, "rx".into(), [2; 16]);
    slow.begin_probe();
    receiver.handle_datagram(&slow.hello().unwrap(), addr, &mixer);
    let mut slow_sent = [0u32; 5];
    let base = 10_000_000u64;
    for i in 0..20 {
        let ts = base + i as u64 * 10_000;
        let pkt = slow.probe(4, PROBE_PAYLOAD_BYTES, ts).unwrap();
        receiver.handle_datagram(&pkt, addr, &mixer);
        slow_sent[4] = slow_sent[4].saturating_add(PROBE_PAYLOAD_BYTES as u32);
    }
    let fb = receiver.feedback_due(Instant::now(), &mixer);
    let peer_fb = fb
        .iter()
        .find(|(a, _)| *a == addr)
        .map(|(_, p)| p.clone())
        .unwrap();
    slow.handle_feedback(&peer_fb, base + 250_000);
    slow.note_probe(slow_sent, false);
    assert!(
        slow.snapshot().estimated_goodput_bps < 2_000_000,
        "buffered step-4 train must not claim 8M: {}",
        slow.snapshot().estimated_goodput_bps
    );
}

#[test]
fn probe_echo_pairs_send_ts_for_rtt() {
    let epoch = [6u8; 16];
    let addr = "127.0.0.1:40112".parse().unwrap();
    let mixer = Mixer::new(AudioFormat::canonical());
    let mut receiver = ReceiverEngine::new(TOKEN, epoch);
    let mut sender = SenderEngine::new(TOKEN, "rtt".into());
    sender.bind_peer(epoch, addr, "rx".into(), [3; 16]);
    receiver.handle_datagram(&sender.hello().unwrap(), addr, &mixer);

    let ts = 42_000_000u64;
    receiver.handle_datagram(&sender.probe(0, 800, ts).unwrap(), addr, &mixer);
    let now = ts + 80_000;
    let fb = receiver.feedback_due(Instant::now(), &mixer)[0].1.clone();
    assert!(sender.handle_feedback(&fb, now));
    assert!(sender.snapshot().rtt_us > 0 && sender.snapshot().rtt_us < 200_000);
}
