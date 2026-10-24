use remote_au::mixer::JitterBuffer;

#[test]
fn playout_recovers_after_source_temporarily_stops_producing_packets() {
    let mut buffer = JitterBuffer::new(4, 240);
    buffer.set_target_packets(3);
    let packet: Vec<u8> = (0..480).flat_map(|_| 0x1234_i16.to_le_bytes()).collect();
    for seq in 1..=3 {
        assert!(buffer.push(seq, &packet));
    }
    let mut one = vec![0; 960];
    buffer.pull(&mut one, 240);
    assert_eq!(one, packet);
    let mut silence_gap = vec![0; 960 * 10];
    buffer.pull(&mut silence_gap, 240 * 10);
    for seq in 4..=6 {
        assert!(
            buffer.push(seq, &packet),
            "fresh resumed audio must be allowed to re-prime after starvation"
        );
    }
    one.fill(0);
    buffer.pull(&mut one, 240);
    assert_eq!(
        one, packet,
        "audio must resume without restarting the receiver"
    );
}

#[test]
fn isolated_packet_loss_is_distinct_from_an_empty_source_queue() {
    let mut buffer = JitterBuffer::new(4, 240);
    buffer.set_target_packets(1);
    let packet = 0x1234_i16.to_le_bytes().repeat(480);
    assert!(buffer.push(1, &packet));
    assert!(buffer.push(3, &packet));
    let mut out = vec![0; 960 * 3];
    buffer.pull(&mut out, 240 * 3);
    assert_eq!(buffer.underruns, 240);
    assert_eq!(
        buffer.starvations, 0,
        "more buffering cannot recover a packet that was lost"
    );
    buffer.pull(&mut out[..960], 240);
    assert_eq!(buffer.starvations, 1);
}
