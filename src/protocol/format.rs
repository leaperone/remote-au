use std::time::Duration;

use thiserror::Error;

pub const WIRE_RATE: u32 = 48_000;
pub const WIRE_CHANNELS: u8 = 2;
pub const WIRE_FRAME_SAMPLES: u16 = 240;
pub const WIRE_PACE: Duration = Duration::from_millis(5);
pub const MAX_DATAGRAM: usize = 1200;
pub const MAX_PCM_CHUNK: usize = 960;
pub const MAX_STREAMS: usize = 8;
pub const STREAM_IDLE: Duration = Duration::from_secs(10);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AudioFormat {
    pub sample_rate: u32,
    pub channels: u8,
    pub frame_samples: u16,
}

impl AudioFormat {
    pub fn canonical() -> Self {
        Self {
            sample_rate: WIRE_RATE,
            channels: WIRE_CHANNELS,
            frame_samples: WIRE_FRAME_SAMPLES,
        }
    }

    pub fn bytes_per_frame(&self) -> usize {
        usize::from(self.channels) * 2
    }

    pub fn expected_payload_bytes(&self) -> usize {
        usize::from(self.frame_samples) * self.bytes_per_frame()
    }

    pub fn validate(&self) -> Result<(), ProtocolError> {
        if !(8_000..=192_000).contains(&self.sample_rate) {
            return Err(ProtocolError::InvalidSampleRate);
        }
        if !(1..=8).contains(&self.channels) {
            return Err(ProtocolError::InvalidChannels);
        }
        if !(1..=4096).contains(&self.frame_samples) {
            return Err(ProtocolError::InvalidFrameSamples);
        }
        Ok(())
    }

    pub fn require_wire(&self) -> Result<(), String> {
        if self.sample_rate != WIRE_RATE
            || self.channels != WIRE_CHANNELS
            || self.frame_samples != WIRE_FRAME_SAMPLES
        {
            return Err(format!(
                "incompatible audio format: wire is fixed {WIRE_RATE} Hz stereo {WIRE_FRAME_SAMPLES} frames (5 ms), got {} Hz {} ch {} frames",
                self.sample_rate, self.channels, self.frame_samples
            ));
        }
        Ok(())
    }
}

#[derive(Debug, Error, PartialEq, Eq)]
pub enum ProtocolError {
    #[error("invalid sample rate")]
    InvalidSampleRate,
    #[error("invalid channels")]
    InvalidChannels,
    #[error("invalid frame samples")]
    InvalidFrameSamples,
    #[error("invalid payload length")]
    InvalidPayload,
}

/// Split interleaved PCM into chunks of at most 960 bytes, on 4-byte frames.
pub fn split_pcm(pcm: &[u8]) -> Vec<&[u8]> {
    let usable = pcm.len() - (pcm.len() % 4);
    if usable == 0 {
        return Vec::new();
    }
    pcm[..usable].chunks(MAX_PCM_CHUNK).collect()
}
