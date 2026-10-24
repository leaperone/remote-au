use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Mutex,
};
use std::{
    thread::{self, JoinHandle},
    time::Duration,
};

use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use serde::Serialize;

use super::pcm::{append_f32_interleaved, append_i16_interleaved, TARGET_RATE};
use super::ring::ByteRing;
use super::AudioFormat;

const RING_BYTES: usize = TARGET_RATE as usize * 2 * 2 / 10; // 100 ms stereo i16

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CaptureSource {
    Microphone,
    Loopback,
    Synthetic,
    Null,
}

#[derive(Debug, Clone, Serialize)]
pub struct CaptureDeviceInfo {
    pub index: usize,
    pub name: String,
}

struct CaptureWorker {
    stop: Arc<AtomicBool>,
    join: Mutex<Option<JoinHandle<()>>>,
}

impl CaptureWorker {
    fn new(stop: Arc<AtomicBool>) -> Self {
        Self {
            stop,
            join: Mutex::new(None),
        }
    }
    fn set_join(&self, handle: JoinHandle<()>) {
        *self.join.lock().unwrap() = Some(handle);
    }
}

impl Drop for CaptureWorker {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.join.lock().unwrap().take() {
            let _ = h.join();
        }
    }
}

pub struct CaptureHandle {
    worker: Option<CaptureWorker>,
    ring: Arc<Mutex<ByteRing>>,
    error: Arc<Mutex<Option<String>>>,
    synth_phase: f32,
    format: AudioFormat,
    source: CaptureSource,
}

impl CaptureHandle {
    pub fn capture_error(&self) -> Option<String> {
        self.error.lock().unwrap().clone()
    }

    pub fn try_read(&mut self, out: &mut [u8]) -> usize {
        if let Some(msg) = self.error.lock().unwrap().clone() {
            let _ = msg; // The worker reports changes, rather than logging each 5 ms poll.
            out.fill(0);
            return 0;
        }
        if self.source == CaptureSource::Synthetic {
            return self.fill_synth(out);
        }
        if self.source == CaptureSource::Null {
            out.fill(0);
            return out.len();
        }
        let mut ring = match self.ring.try_lock() {
            Ok(g) => g,
            Err(_) => {
                out.fill(0);
                return out.len();
            }
        };
        let n = ring.pop_into(out);
        if n < out.len() {
            out[n..].fill(0);
        }
        out.len()
    }

    fn fill_synth(&mut self, out: &mut [u8]) -> usize {
        let step = std::f32::consts::TAU * 440.0 / self.format.sample_rate as f32;
        for frame in out.chunks_exact_mut(self.format.bytes_per_frame()) {
            let sample = (self.synth_phase.sin() * 0.2 * 32767.0) as i16;
            for channel in frame.chunks_exact_mut(2) {
                channel.copy_from_slice(&sample.to_le_bytes());
            }
            self.synth_phase = (self.synth_phase + step) % std::f32::consts::TAU;
        }
        out.len()
    }

    pub fn stop(&mut self) {
        self.worker = None;
    }
}

impl Drop for CaptureHandle {
    fn drop(&mut self) {
        self.stop();
    }
}

pub fn list_capture_devices() -> Vec<CaptureDeviceInfo> {
    let host = cpal::default_host();
    let mut out = Vec::new();
    if let Ok(devices) = host.input_devices() {
        for (index, device) in devices.enumerate() {
            let name = device.name().unwrap_or_else(|_| format!("input-{}", index));
            out.push(CaptureDeviceInfo { index, name });
        }
    }
    out
}

pub fn open_capture(
    format: AudioFormat,
    source: CaptureSource,
    device_index: Option<usize>,
) -> Result<CaptureHandle, String> {
    if format.sample_rate != 48_000 || format.channels != 2 {
        return Err("audio backend requires 48000 Hz stereo".into());
    }

    if source == CaptureSource::Synthetic || source == CaptureSource::Null {
        let ring = Arc::new(Mutex::new(ByteRing::with_capacity(RING_BYTES)));
        let error = Arc::new(Mutex::new(None));
        return Ok(CaptureHandle {
            worker: None,
            ring,
            error,
            synth_phase: 0.0,
            format,
            source,
        });
    }

    #[cfg(windows)]
    if source == CaptureSource::Loopback {
        return open_windows_loopback(format, device_index);
    }

    #[cfg(not(windows))]
    if source == CaptureSource::Loopback {
        return Err(
            "loopback capture is not supported on this platform; on macOS use microphone with a virtual device (e.g. BlackHole) or --source synth for tests".into(),
        );
    }

    open_cpal_microphone(format, device_index)
}

fn open_cpal_microphone(
    format: AudioFormat,
    device_index: Option<usize>,
) -> Result<CaptureHandle, String> {
    let ring = Arc::new(Mutex::new(ByteRing::with_capacity(RING_BYTES)));
    let error = Arc::new(Mutex::new(None));
    let stop = Arc::new(AtomicBool::new(false));
    let worker = CaptureWorker::new(stop.clone());
    let (ring_cb, error_cb) = (ring.clone(), error.clone());
    let handle = thread::Builder::new()
        .name("microphone-capture".into())
        .spawn(move || {
            let mut last_warning = std::time::Instant::now() - Duration::from_secs(31);
            while !stop.load(Ordering::Relaxed) {
                *error_cb.lock().unwrap() = None;
                match open_microphone_stream(device_index, ring_cb.clone(), error_cb.clone()) {
                    Ok(stream) => {
                        while !stop.load(Ordering::Relaxed) && error_cb.lock().unwrap().is_none() {
                            thread::sleep(Duration::from_millis(100));
                        }
                        drop(stream);
                    }
                    Err(e) => {
                        *error_cb.lock().unwrap() = Some(e.clone());
                        if last_warning.elapsed() >= Duration::from_secs(30) {
                            tracing::warn!("microphone unavailable: {}; retrying", e);
                            last_warning = std::time::Instant::now();
                        }
                    }
                }
                for _ in 0..10 {
                    if stop.load(Ordering::Relaxed) {
                        break;
                    }
                    thread::sleep(Duration::from_millis(100));
                }
            }
        })
        .map_err(|e| e.to_string())?;
    worker.set_join(handle);
    Ok(CaptureHandle {
        worker: Some(worker),
        ring,
        error,
        synth_phase: 0.0,
        format,
        source: CaptureSource::Microphone,
    })
}

fn open_microphone_stream(
    device_index: Option<usize>,
    ring: Arc<Mutex<ByteRing>>,
    error: Arc<Mutex<Option<String>>>,
) -> Result<cpal::Stream, String> {
    let host = cpal::default_host();
    let device = match device_index {
        Some(i) => host
            .input_devices()
            .map_err(|e| e.to_string())?
            .nth(i)
            .ok_or_else(|| format!("input device index {} not found", i))?,
        None => host
            .default_input_device()
            .ok_or_else(|| "no default input device".to_string())?,
    };
    let config = device
        .supported_input_configs()
        .map_err(|e| e.to_string())?
        .find(|cfg| {
            cfg.min_sample_rate().0 <= TARGET_RATE
                && cfg.max_sample_rate().0 >= TARGET_RATE
                && matches!(
                    cfg.sample_format(),
                    cpal::SampleFormat::F32 | cpal::SampleFormat::I16
                )
        })
        .ok_or("microphone endpoint must support 48000 Hz F32 or I16")?
        .with_sample_rate(cpal::SampleRate(TARGET_RATE));
    let sample_format = config.sample_format();
    let in_channels = config.channels() as usize;
    let in_rate = config.sample_rate().0;

    let ring_cb = Arc::clone(&ring);

    let make_err = || {
        let error = Arc::clone(&error);
        move |e: cpal::StreamError| {
            tracing::error!("cpal capture stream error: {}", e);
            *error.lock().unwrap() = Some(e.to_string());
        }
    };

    let stream = match sample_format {
        cpal::SampleFormat::F32 => device.build_input_stream(
            &config.into(),
            move |data: &[f32], _| {
                let mut chunk = Vec::new();
                append_f32_interleaved(&mut chunk, data, in_channels, in_rate);
                if let Ok(mut r) = ring_cb.try_lock() {
                    r.push(&chunk);
                }
            },
            make_err(),
            None,
        ),
        cpal::SampleFormat::I16 => device.build_input_stream(
            &config.into(),
            move |data: &[i16], _| {
                let mut chunk = Vec::new();
                append_i16_interleaved(&mut chunk, data, in_channels, in_rate);
                if let Ok(mut r) = ring_cb.try_lock() {
                    r.push(&chunk);
                }
            },
            make_err(),
            None,
        ),
        other => {
            return Err(format!(
                "unsupported microphone sample format {:?}; need F32 or I16",
                other
            ));
        }
    }
    .map_err(|e| e.to_string())?;

    stream.play().map_err(|e| e.to_string())?;

    Ok(stream)
}

#[cfg(windows)]
fn open_windows_loopback(
    format: AudioFormat,
    device_index: Option<usize>,
) -> Result<CaptureHandle, String> {
    use wasapi::{initialize_mta, SampleType, WaveFormat};

    let ring = Arc::new(Mutex::new(ByteRing::with_capacity(RING_BYTES)));
    let stop = Arc::new(AtomicBool::new(false));
    let error = Arc::new(Mutex::new(None));
    let worker = CaptureWorker::new(Arc::clone(&stop));

    let channels = format.channels.max(1);
    let rate = format.sample_rate.max(8000);
    let idx = device_index;
    let ring_handle = Arc::clone(&ring);
    let error_handle = Arc::clone(&error);

    let handle = thread::Builder::new()
        .name("wasapi-loopback".into())
        .spawn(move || {
            if initialize_mta().is_err() {
                *error.lock().unwrap() = Some("failed to initialize COM (MTA)".into());
                return;
            }
            let desired = WaveFormat::new(
                32,
                32,
                &SampleType::Float,
                rate as usize,
                channels as usize,
                None,
            );
            let in_ch = channels as usize;

            let mut last_warning = std::time::Instant::now() - Duration::from_secs(31);
            while !stop.load(Ordering::SeqCst) {
                match run_loopback_once(&desired, idx, &stop, &ring, in_ch, rate) {
                    Ok(()) => break,
                    Err(e) => {
                        if last_warning.elapsed() >= Duration::from_secs(30) {
                            tracing::warn!("loopback unavailable: {}; retrying until an active render endpoint exists", e);
                            last_warning = std::time::Instant::now();
                        }
                        *error.lock().unwrap() = Some(e);
                        thread::sleep(Duration::from_millis(500));
                        if stop.load(Ordering::SeqCst) {
                            break;
                        }
                        error.lock().unwrap().take();
                    }
                }
            }
        })
        .map_err(|e| e.to_string())?;

    worker.set_join(handle);

    Ok(CaptureHandle {
        worker: Some(worker),
        ring: ring_handle,
        error: error_handle,
        synth_phase: 0.0,
        format,
        source: CaptureSource::Loopback,
    })
}

#[cfg(windows)]
fn run_loopback_once(
    desired: &wasapi::WaveFormat,
    device_index: Option<usize>,
    stop: &AtomicBool,
    ring: &Arc<Mutex<ByteRing>>,
    in_channels: usize,
    in_rate: u32,
) -> Result<(), String> {
    use wasapi::{DeviceCollection, Direction, StreamMode};

    let device = match device_index {
        Some(i) => {
            let coll = DeviceCollection::new(&Direction::Render).map_err(|e| e.to_string())?;
            coll.get_device_at_index(i as u32)
                .map_err(|e| format!("render device index {}: {}", i, e))?
        }
        None => wasapi::get_default_device(&Direction::Render).map_err(|e| e.to_string())?,
    };

    let mut audio_client = device.get_iaudioclient().map_err(|e| e.to_string())?;
    let (default_period, _) = audio_client
        .get_device_period()
        .map_err(|e| e.to_string())?;
    let mode = StreamMode::EventsShared {
        autoconvert: true,
        buffer_duration_hns: default_period,
    };
    audio_client
        .initialize_client(desired, &Direction::Capture, &mode)
        .map_err(|e| e.to_string())?;
    let h_event = audio_client
        .set_get_eventhandle()
        .map_err(|e| e.to_string())?;
    let capture_client = audio_client
        .get_audiocaptureclient()
        .map_err(|e| e.to_string())?;
    audio_client.start_stream().map_err(|e| e.to_string())?;

    let bytes_per_frame = (in_channels * 4).max(4);
    let mut raw_deque = std::collections::VecDeque::with_capacity(bytes_per_frame * 4096);
    let endpoint_id = device.get_id().map_err(|e| e.to_string())?;
    let mut last_endpoint_check = std::time::Instant::now();
    while !stop.load(Ordering::SeqCst) {
        capture_client
            .read_from_device_to_deque(&mut raw_deque)
            .map_err(|e| e.to_string())?;
        let frames = raw_deque.len() / bytes_per_frame;
        if frames > 0 {
            let raw: Vec<u8> = raw_deque.drain(..frames * bytes_per_frame).collect();
            let samples: Vec<f32> = raw
                .chunks_exact(4)
                .map(|c| f32::from_le_bytes([c[0], c[1], c[2], c[3]]))
                .collect();
            let mut chunk = Vec::with_capacity(frames * 4);
            append_f32_interleaved(&mut chunk, &samples, in_channels, in_rate);
            if let Ok(mut r) = ring.try_lock() {
                r.push(&chunk);
            }
        }
        if last_endpoint_check.elapsed() >= Duration::from_secs(2) {
            if device.get_state().map_err(|e| e.to_string())? != wasapi::DeviceState::Active {
                return Err("render endpoint is no longer active".into());
            }
            if device_index.is_none() {
                let current =
                    wasapi::get_default_device(&Direction::Render).map_err(|e| e.to_string())?;
                if current.get_id().map_err(|e| e.to_string())? != endpoint_id {
                    return Err("default render endpoint changed; reopening".into());
                }
            }
            last_endpoint_check = std::time::Instant::now();
        }
        // An idle audio engine need not signal. A timeout alone is not device failure.
        let _ = h_event.wait_for_event(200);
    }
    let _ = audio_client.stop_stream();
    Ok(())
}
