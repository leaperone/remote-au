mod jitter;

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

pub use jitter::{JitterBuffer, MAX_SLOTS};

use crate::protocol::{AudioFormat, MAX_STREAMS};

struct Stream {
    id: String,
    name: String,
    gain: f32,
    last_seen: Instant,
    buffer: JitterBuffer,
}

struct Inner {
    streams: Vec<Stream>,
    master_gain: f32,
    scratch: Vec<f32>,
    frame_buf: Vec<u8>,
    default_target_packets: usize,
}

#[derive(Clone)]
pub struct StreamStat {
    pub id: String,
    pub name: String,
    pub packets: u64,
    pub lost: u64,
    pub underruns: u64,
    pub buffer_ms: u32,
    pub buffered_packets: usize,
}

pub struct Mixer {
    format: AudioFormat,
    inner: Mutex<Inner>,
    callbacks: AtomicU64,
    last_callback_ms: AtomicU64,
}

impl Mixer {
    pub fn new(format: AudioFormat) -> Self {
        let packet_frames = usize::from(format.frame_samples.max(1));
        let default_target_packets = if packet_frames >= 240 { 4 } else { 1 };
        Self {
            format,
            inner: Mutex::new(Inner {
                streams: Vec::new(),
                master_gain: 1.0,
                scratch: vec![0.0; 4096 * usize::from(format.channels)],
                frame_buf: vec![0; 4096 * format.bytes_per_frame()],
                default_target_packets,
            }),
            callbacks: AtomicU64::new(0),
            last_callback_ms: AtomicU64::new(0),
        }
    }

    pub fn set_master_gain(&self, gain: f32) {
        if !gain.is_finite() {
            return;
        }
        let gain = gain.clamp(0.0, 2.0);
        if let Ok(mut inner) = self.inner.lock() {
            inner.master_gain = gain;
        }
    }

    pub fn master_gain(&self) -> f32 {
        self.inner.lock().map(|g| g.master_gain).unwrap_or(1.0)
    }

    pub fn ensure_stream(
        &self,
        id: impl Into<String>,
        name: impl Into<String>,
    ) -> Result<(), String> {
        let id = id.into();
        let name = name.into();
        let mut inner = self
            .inner
            .lock()
            .map_err(|_| "mixer lock poisoned".to_string())?;
        if let Some(stream) = inner.streams.iter_mut().find(|s| s.id == id) {
            stream.name = name;
            stream.last_seen = Instant::now();
            return Ok(());
        }
        if inner.streams.len() >= MAX_STREAMS {
            return Err("max streams reached".into());
        }
        let bpf = self.format.bytes_per_frame().max(1);
        let frames = usize::from(self.format.frame_samples.max(1));
        let mut buffer = JitterBuffer::new(bpf, frames);
        buffer.set_target_packets(inner.default_target_packets);
        inner.streams.push(Stream {
            id,
            name,
            gain: 1.0,
            last_seen: Instant::now(),
            buffer,
        });
        Ok(())
    }

    pub fn remove_stream(&self, id: &str) {
        if let Ok(mut inner) = self.inner.lock() {
            inner.streams.retain(|s| s.id != id);
        }
    }

    pub fn touch(&self, id: &str) {
        if let Ok(mut inner) = self.inner.lock() {
            if let Some(stream) = inner.streams.iter_mut().find(|s| s.id == id) {
                stream.last_seen = Instant::now();
            }
        }
    }

    pub fn stream_ids(&self) -> Vec<String> {
        self.inner
            .lock()
            .map(|inner| inner.streams.iter().map(|s| s.id.clone()).collect())
            .unwrap_or_default()
    }

    pub fn set_stream_gain(&self, id: &str, gain: f32) -> bool {
        if !gain.is_finite() {
            return false;
        }
        let gain = gain.clamp(0.0, 2.0);
        let Ok(mut inner) = self.inner.lock() else {
            return false;
        };
        if let Some(stream) = inner.streams.iter_mut().find(|s| s.id == id) {
            stream.gain = gain;
            true
        } else {
            false
        }
    }

    pub fn set_stream_target_ms(&self, id: &str, ms: u32) -> bool {
        let Ok(mut inner) = self.inner.lock() else {
            return false;
        };
        if let Some(stream) = inner.streams.iter_mut().find(|s| s.id == id) {
            stream.buffer.set_target_ms(ms);
            true
        } else {
            false
        }
    }

    pub fn set_target_packets(&self, packets: usize) {
        if let Ok(mut inner) = self.inner.lock() {
            inner.default_target_packets = packets.clamp(1, MAX_SLOTS);
            for stream in &mut inner.streams {
                stream.buffer.set_target_packets(packets);
            }
        }
    }

    pub fn push_packet(&self, id: &str, seq: u64, pcm: &[u8]) -> bool {
        let Ok(mut inner) = self.inner.lock() else {
            return false;
        };
        let Some(stream) = inner.streams.iter_mut().find(|s| s.id == id) else {
            return false;
        };
        stream.last_seen = Instant::now();
        stream.buffer.push(seq, pcm)
    }

    pub fn expire_idle(&self, max_idle: Duration, now: Instant) -> Vec<String> {
        let Ok(mut inner) = self.inner.lock() else {
            return Vec::new();
        };
        let mut removed = Vec::new();
        inner.streams.retain(|s| {
            let keep = now.saturating_duration_since(s.last_seen) < max_idle;
            if !keep {
                removed.push(s.id.clone());
            }
            keep
        });
        removed
    }

    pub fn stats(&self) -> Vec<StreamStat> {
        let Ok(inner) = self.inner.lock() else {
            return Vec::new();
        };
        inner
            .streams
            .iter()
            .map(|s| StreamStat {
                id: s.id.clone(),
                name: s.name.clone(),
                packets: s.buffer.arrived,
                lost: s.buffer.losses,
                underruns: s.buffer.underruns,
                buffer_ms: s.buffer.target_ms(),
                buffered_packets: s.buffer.buffered_packets(),
            })
            .collect()
    }

    pub fn callbacks(&self) -> u64 {
        self.callbacks.load(Ordering::Relaxed)
    }

    pub fn last_callback_ms(&self) -> u64 {
        self.last_callback_ms.load(Ordering::Relaxed)
    }

    pub fn pull(&self, out: &mut [u8], frame_count: usize) {
        let bpf = self.format.bytes_per_frame();
        let channels = usize::from(self.format.channels);
        if bpf == 0 || channels == 0 || frame_count == 0 || out.len() < bpf {
            return;
        }
        let asked = frame_count.min(out.len() / bpf);
        out[..asked * bpf].fill(0);
        let frames = asked.min(4096);
        let Ok(mut inner) = self.inner.try_lock() else {
            return;
        };
        let samples = frames * channels;
        let nbytes = frames * bpf;
        let mut scratch = std::mem::take(&mut inner.scratch);
        let mut frame_buf = std::mem::take(&mut inner.frame_buf);
        if scratch.len() < samples {
            scratch.resize(samples, 0.0);
        }
        scratch[..samples].fill(0.0);
        if frame_buf.len() < nbytes {
            frame_buf.resize(nbytes, 0);
        }
        let master = inner.master_gain;
        for stream in &mut inner.streams {
            let got = stream.buffer.pull(&mut frame_buf[..nbytes], frames);
            if got == 0 {
                continue;
            }
            let gain = stream.gain;
            for frame in 0..got.min(frames) {
                for ch in 0..channels {
                    let byte = (frame * channels + ch) * 2;
                    if byte + 1 >= frame_buf.len() {
                        break;
                    }
                    let sample = i16::from_le_bytes([frame_buf[byte], frame_buf[byte + 1]]);
                    scratch[frame * channels + ch] += (sample as f32 / 32768.0) * gain;
                }
            }
        }
        let out_samples = frames * channels;
        for i in 0..out_samples {
            if i * 2 + 1 >= out.len() {
                break;
            }
            let mixed = (scratch[i] * master).clamp(-1.0, 1.0);
            let sample = (mixed * 32767.0) as i16;
            out[i * 2..i * 2 + 2].copy_from_slice(&sample.to_le_bytes());
        }
        inner.scratch = scratch;
        inner.frame_buf = frame_buf;
        self.callbacks.fetch_add(1, Ordering::Relaxed);
        self.last_callback_ms.store(mono_ms(), Ordering::Relaxed);
    }
}

pub fn mono_ms() -> u64 {
    static ORIGIN: std::sync::OnceLock<Instant> = std::sync::OnceLock::new();
    ORIGIN.get_or_init(Instant::now).elapsed().as_millis() as u64
}
