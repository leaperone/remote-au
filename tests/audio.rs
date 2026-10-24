use remote_au::audio::{open_capture, open_playback, AudioFormat, CaptureSource, PlaybackBackend};
use remote_au::protocol::AudioFormat as ProtoFormat;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

fn test_format() -> AudioFormat {
    ProtoFormat {
        sample_rate: 48_000,
        channels: 2,
        frame_samples: 240,
    }
}

#[test]
fn synth_advances_phase_and_fills_buffer() {
    let mut cap = open_capture(test_format(), CaptureSource::Synthetic, None).unwrap();
    let mut a = [0u8; 960];
    let mut b = [0u8; 960];
    cap.try_read(&mut a);
    cap.try_read(&mut b);
    assert_ne!(a, b, "synthetic tone should change between reads");
}

#[test]
fn null_capture_zeros() {
    let mut cap = open_capture(test_format(), CaptureSource::Null, None).unwrap();
    let mut buf = [1u8; 64];
    cap.try_read(&mut buf);
    assert!(buf.iter().all(|&b| b == 0));
}

#[test]
fn null_playback_pulls() {
    let count = Arc::new(AtomicUsize::new(0));
    let count_cb = Arc::clone(&count);
    let pull = Arc::new(move |_buf: &mut [u8], frames: usize| {
        count_cb.fetch_add(frames, Ordering::Relaxed);
    });
    let handle = open_playback(test_format(), PlaybackBackend::Null, None, pull).unwrap();
    std::thread::sleep(std::time::Duration::from_millis(30));
    drop(handle);
    assert!(
        count.load(Ordering::Relaxed) > 0,
        "null playback must invoke pull"
    );
}

#[test]
fn synth_is_440hz_with_identical_stereo_channels() {
    let mut cap = open_capture(test_format(), CaptureSource::Synthetic, None).unwrap();
    let mut pcm = vec![0; 48000 * 4];
    cap.try_read(&mut pcm);
    let samples: Vec<i16> = pcm
        .chunks_exact(4)
        .map(|f| {
            assert_eq!(&f[..2], &f[2..]);
            i16::from_le_bytes([f[0], f[1]])
        })
        .collect();
    assert_eq!(samples[0], 0);
    assert!((samples[1] - 377).abs() <= 1);
    let crossings = samples.windows(2).filter(|s| s[0] <= 0 && s[1] > 0).count();
    assert!(
        (439..=441).contains(&crossings),
        "one second should contain 440 periods, got {crossings}"
    );
}
