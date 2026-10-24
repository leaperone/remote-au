use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Mutex,
};
use std::thread::{self, JoinHandle};
use std::time::Duration;

use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use serde::Serialize;

use super::pcm::{fill_device_f32_from_session_i16, fill_device_i16_from_session_i16};
use super::AudioFormat;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum DeviceDirection {
    Output,
    Input,
}

#[derive(Debug, Clone, Serialize)]
pub struct DeviceInfo {
    pub index: usize,
    pub name: String,
    pub is_default: bool,
    pub direction: DeviceDirection,
}

pub fn list_devices() -> Vec<DeviceInfo> {
    list_output_devices()
}

pub fn list_output_devices() -> Vec<DeviceInfo> {
    let host = cpal::default_host();
    let default_name = host.default_output_device().and_then(|d| d.name().ok());
    let mut out = Vec::new();
    if let Ok(devices) = host.output_devices() {
        for (index, device) in devices.enumerate() {
            let name = device
                .name()
                .unwrap_or_else(|_| format!("device-{}", index));
            let is_default = default_name.as_deref() == Some(name.as_str());
            out.push(DeviceInfo {
                index,
                name,
                is_default,
                direction: DeviceDirection::Output,
            });
        }
    }
    out
}

#[derive(Clone, Copy)]
pub enum PlaybackBackend {
    Cpal,
    Null,
}

static CALLBACKS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
static FRAMES: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
static NONZERO: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
static SILENT_RUN: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
static MAX_SILENT_RUN: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
static STATE: Mutex<&str> = Mutex::new("closed");
static DEVICE: Mutex<String> = Mutex::new(String::new());

pub fn playback_status() -> serde_json::Value {
    serde_json::json!({
        "state": *STATE.lock().unwrap(), "device": DEVICE.lock().unwrap().clone(),
        "callbacks": CALLBACKS.load(Ordering::Relaxed),
        "frames": FRAMES.load(Ordering::Relaxed),
        "nonzero_samples": NONZERO.load(Ordering::Relaxed),
        "silent_run_frames": SILENT_RUN.load(Ordering::Relaxed),
        "max_silent_run_frames": MAX_SILENT_RUN.load(Ordering::Relaxed),
    })
}

fn counted_pull(pull: &Arc<dyn Fn(&mut [u8], usize) + Send + Sync>, raw: &mut [u8], frames: usize) {
    pull(raw, frames);
    CALLBACKS.fetch_add(1, Ordering::Relaxed);
    FRAMES.fetch_add(frames as u64, Ordering::Relaxed);
    let count = raw
        .chunks_exact(2)
        .filter(|s| s[0] != 0 || s[1] != 0)
        .count() as u64;
    if count > 0 {
        NONZERO.fetch_add(count, Ordering::Relaxed);
        SILENT_RUN.store(0, Ordering::Relaxed);
    } else if NONZERO.load(Ordering::Relaxed) > 0 {
        let run = SILENT_RUN.fetch_add(frames as u64, Ordering::Relaxed) + frames as u64;
        MAX_SILENT_RUN.fetch_max(run, Ordering::Relaxed);
    }
}

pub struct PlaybackHandle {
    stop: Arc<AtomicBool>,
    join: Option<JoinHandle<()>>,
}
impl PlaybackHandle {
    pub fn stop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(h) = self.join.take() {
            let _ = h.join();
        }
    }
}
impl Drop for PlaybackHandle {
    fn drop(&mut self) {
        self.stop();
    }
}

pub fn open_playback(
    format: AudioFormat,
    backend: PlaybackBackend,
    device_index: Option<usize>,
    pull: Arc<dyn Fn(&mut [u8], usize) + Send + Sync>,
) -> Result<PlaybackHandle, String> {
    if format.sample_rate != 48_000 || format.channels != 2 {
        return Err("audio backend requires 48000 Hz stereo".into());
    }

    let stop = Arc::new(AtomicBool::new(false));
    let stopping = stop.clone();
    let join = thread::Builder::new()
        .name("audio-playback".into())
        .spawn(move || {
            if matches!(backend, PlaybackBackend::Null) {
                *STATE.lock().unwrap() = "null";
                let mut buffer = vec![0; format.expected_payload_bytes()];
                let period = Duration::from_secs_f64(
                    format.frame_samples as f64 / format.sample_rate as f64,
                );
                let mut deadline = std::time::Instant::now();
                while !stopping.load(Ordering::Relaxed) {
                    counted_pull(&pull, &mut buffer, format.frame_samples as usize);
                    deadline += period;
                    if let Some(wait) = deadline.checked_duration_since(std::time::Instant::now()) {
                        thread::sleep(wait);
                    } else {
                        deadline = std::time::Instant::now();
                    }
                }
            } else {
                let mut last_warning = std::time::Instant::now() - Duration::from_secs(31);
                while !stopping.load(Ordering::Relaxed) {
                    *STATE.lock().unwrap() = "waiting_for_output";
                    let failed = Arc::new(AtomicBool::new(false));
                    match open_cpal_playback(format, device_index, pull.clone(), failed.clone()) {
                        Ok(stream) => {
                            *STATE.lock().unwrap() = "playing";
                            let opened_name = DEVICE.lock().unwrap().clone();
                            let mut last_check = std::time::Instant::now();
                            while !stopping.load(Ordering::Relaxed)
                                && !failed.load(Ordering::Relaxed)
                            {
                                thread::sleep(Duration::from_millis(100));
                                if device_index.is_none()
                                    && last_check.elapsed() > Duration::from_secs(2)
                                {
                                    let name = cpal::default_host()
                                        .default_output_device()
                                        .and_then(|d| d.name().ok())
                                        .unwrap_or_default();
                                    if name != opened_name {
                                        break;
                                    }
                                    last_check = std::time::Instant::now();
                                }
                            }
                            drop(stream);
                        }
                        Err(e) => {
                            if last_warning.elapsed() >= Duration::from_secs(30) {
                                tracing::warn!(
                                    "playback unavailable: {}; waiting for output device",
                                    e
                                );
                                last_warning = std::time::Instant::now();
                            }
                            for _ in 0..20 {
                                if stopping.load(Ordering::Relaxed) {
                                    break;
                                }
                                thread::sleep(Duration::from_millis(100));
                            }
                        }
                    }
                }
            }
            *STATE.lock().unwrap() = "stopped";
        })
        .map_err(|e| e.to_string())?;
    Ok(PlaybackHandle {
        stop,
        join: Some(join),
    })
}

fn open_cpal_playback(
    format: AudioFormat,
    device_index: Option<usize>,
    pull: Arc<dyn Fn(&mut [u8], usize) + Send + Sync>,
    failed: Arc<AtomicBool>,
) -> Result<cpal::Stream, String> {
    let host = cpal::default_host();
    let device = match device_index {
        Some(i) => host
            .output_devices()
            .map_err(|e| e.to_string())?
            .nth(i)
            .ok_or_else(|| format!("output device index {} not found", i))?,
        None => host
            .default_output_device()
            .ok_or_else(|| "no default output device".to_string())?,
    };

    *DEVICE.lock().unwrap() = device.name().unwrap_or_default();
    let supported = device.default_output_config().map_err(|e| e.to_string())?;
    let sample_format = supported.sample_format();
    let mut stream_config: cpal::StreamConfig = supported.into();
    stream_config.channels = format.channels as u16;
    stream_config.sample_rate = cpal::SampleRate(format.sample_rate);

    let device_channels = stream_config.channels as usize;
    let session_channels = format.channels as usize;
    let session_bpf = format.bytes_per_frame();
    let pull_cb = pull;

    let err_fn = move |e| {
        tracing::warn!("playback stream lost: {}", e);
        failed.store(true, Ordering::Relaxed);
    };

    let stream = match sample_format {
        cpal::SampleFormat::I16 => device
            .build_output_stream(
                &stream_config,
                move |data: &mut [i16], _| {
                    for chunk in data.chunks_mut(device_channels * 2048) {
                        let frames = chunk.len() / device_channels;
                        let mut raw = [0u8; 8192];
                        let raw = &mut raw[..frames * session_bpf];
                        counted_pull(&pull_cb, raw, frames);
                        fill_device_i16_from_session_i16(
                            chunk,
                            raw,
                            device_channels,
                            session_channels,
                        );
                    }
                },
                err_fn,
                None,
            )
            .map_err(|e| e.to_string())?,
        cpal::SampleFormat::F32 => device
            .build_output_stream(
                &stream_config,
                move |data: &mut [f32], _| {
                    for chunk in data.chunks_mut(device_channels * 2048) {
                        let frames = chunk.len() / device_channels;
                        let mut raw = [0u8; 8192];
                        let raw = &mut raw[..frames * session_bpf];
                        counted_pull(&pull_cb, raw, frames);
                        fill_device_f32_from_session_i16(
                            chunk,
                            raw,
                            device_channels,
                            session_channels,
                        );
                    }
                },
                err_fn,
                None,
            )
            .map_err(|e| e.to_string())?,
        other => {
            return Err(format!(
                "unsupported playback sample format {:?}; need I16 or F32",
                other
            ));
        }
    };

    stream.play().map_err(|e| e.to_string())?;
    Ok(stream)
}
