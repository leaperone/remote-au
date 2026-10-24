//! Link adaptation. `estimated_goodput_bps` is the highest paced probe step that
//! cleared the loss budget, never a claim about the physical maximum.

use std::time::{Duration, Instant};

pub const MAX_PROBE_BYTES: usize = 128 * 1024;
pub const PROBE_STEPS_BPS: [u64; 5] = [500_000, 1_000_000, 2_000_000, 4_000_000, 8_000_000];
/// Fixed probe payload size from [`plan_probe`].
pub const PROBE_PAYLOAD_BYTES: usize = 800;
/// Full on-wire datagram size for one probe (header + AEAD + probe body).
pub const PROBE_WIRE_BYTES: usize = 46 + 16 + 13 + PROBE_PAYLOAD_BYTES;
pub const MIN_BUFFER_MS: u32 = 15;
pub const MAX_BUFFER_MS: u32 = 150;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Profile {
    Pcm,
    Adpcm48,
    Adpcm24,
}

impl Profile {
    pub fn codec(self) -> u8 {
        match self {
            Self::Pcm => crate::protocol::CODEC_PCM,
            Self::Adpcm48 => crate::protocol::CODEC_ADPCM48,
            Self::Adpcm24 => crate::protocol::CODEC_ADPCM24,
        }
    }

    pub fn name(self) -> &'static str {
        crate::protocol::codec_name(self.codec())
    }

    pub fn default_buffer_ms(self) -> u32 {
        match self {
            Self::Pcm => 20,
            Self::Adpcm48 => 40,
            Self::Adpcm24 => 80,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ProbePacketSpec {
    pub step: u8,
    pub rate_bps: u64,
    pub payload: usize,
    pub datagram_len: usize,
    pub delay_after: Duration,
}

pub fn plan_probe(max_bytes: usize) -> Vec<ProbePacketSpec> {
    let max_bytes = max_bytes.min(MAX_PROBE_BYTES);
    let payload = 800usize;
    let body = 13 + payload;
    let datagram_len = 46 + 16 + body;
    let mut plan = Vec::new();
    let mut used = 0usize;
    for (step, rate) in PROBE_STEPS_BPS.iter().copied().enumerate() {
        let mut packets = 0;
        while packets < 20 {
            if used + datagram_len > max_bytes {
                return plan;
            }
            let bits = (datagram_len as u128) * 8;
            let micros = (bits * 1_000_000 / u128::from(rate.max(1))).max(1) as u64;
            plan.push(ProbePacketSpec {
                step: step as u8,
                rate_bps: rate,
                payload,
                datagram_len,
                delay_after: Duration::from_micros(micros),
            });
            used += datagram_len;
            packets += 1;
        }
    }
    plan
}

/// Payload bytes summed on the wire for a probe step (includes header/AEAD overhead per datagram).
pub fn probe_wire_bytes(payload_bytes: u32) -> u64 {
    if payload_bytes == 0 {
        return 0;
    }
    let packets = (u64::from(payload_bytes) + u64::from(PROBE_PAYLOAD_BYTES as u32) - 1)
        / u64::from(PROBE_PAYLOAD_BYTES as u32);
    packets * u64::from(PROBE_WIRE_BYTES as u32)
}

pub fn effective_goodput_bps(wire_bytes: u64, duration_us: u32) -> u64 {
    if duration_us == 0 || wire_bytes == 0 {
        return 0;
    }
    wire_bytes * 8 * 1_000_000 / u64::from(duration_us)
}

/// Highest paced step with ≥90% payload delivery and sustainable wire goodput on send and receive.
///
/// Each tuple is `(paced_rate_bps, sent_payload, recv_payload, send_span_us, recv_span_us)`.
/// Goodput uses [`PROBE_WIRE_BYTES`] per 800-byte payload chunk (not payload bits alone).
pub fn highest_sustainable_step_timed(steps: &[(u64, u32, u32, u32, u32)]) -> Option<u64> {
    let mut best = None;
    for &(rate, sent, recv, send_us, recv_us) in steps {
        if sent == 0 {
            continue;
        }
        if u64::from(recv) * 100 < u64::from(sent) * 90 {
            continue;
        }
        let mut sustainable = rate;
        if send_us > 0 && recv_us > 0 {
            let send_bps = effective_goodput_bps(probe_wire_bytes(sent), send_us);
            let recv_bps = effective_goodput_bps(probe_wire_bytes(recv), recv_us);
            sustainable = rate.min(send_bps).min(recv_bps);
        }
        if sustainable == 0 {
            continue;
        }
        sustainable = sustainable.min(PROBE_STEPS_BPS[4]);
        best = Some(best.map_or(sustainable, |b: u64| b.max(sustainable)));
    }
    best
}

/// Byte-delivery-only view for legacy callers; spans of zero skip timing caps.
pub fn highest_sustainable_step(steps: &[(u64, u32, u32)]) -> Option<u64> {
    let timed: Vec<(u64, u32, u32, u32, u32)> =
        steps.iter().map(|&(r, s, rv)| (r, s, rv, 0, 0)).collect();
    highest_sustainable_step_timed(&timed)
}

pub fn profile_for_floor(floor_bps: Option<u64>) -> Profile {
    match floor_bps {
        Some(rate) if rate >= 2_000_000 => Profile::Pcm,
        Some(rate) if rate >= 1_000_000 => Profile::Adpcm48,
        _ => Profile::Adpcm24,
    }
}

#[derive(Debug, Clone, Copy)]
pub struct PathSample {
    pub loss: f32,
    pub rtt_us: u32,
    pub jitter_us: u32,
    pub underruns: u32,
}

#[derive(Debug, Clone)]
pub struct AdaptState {
    pub profile: Profile,
    pub target_buffer_ms: u32,
    bad: u8,
}

impl AdaptState {
    pub fn start() -> Self {
        Self {
            profile: Profile::Pcm,
            target_buffer_ms: Profile::Pcm.default_buffer_ms(),
            bad: 0,
        }
    }

    /// `gradual` moves one quality step toward a better floor. A worse floor is applied immediately.
    pub fn apply_floor(&mut self, floor_bps: Option<u64>, gradual: bool) {
        let target = profile_for_floor(floor_bps);
        if rank(target) > rank(self.profile) {
            self.profile = target;
            self.target_buffer_ms = self.target_buffer_ms.max(self.profile.default_buffer_ms());
        } else if !gradual {
            self.profile = target;
            self.target_buffer_ms = self.profile.default_buffer_ms();
        } else if rank(target) < rank(self.profile) {
            self.profile = step_up(self.profile);
        }
        self.target_buffer_ms = self.target_buffer_ms.clamp(MIN_BUFFER_MS, MAX_BUFFER_MS);
        self.bad = 0;
    }

    pub fn observe(&mut self, sample: PathSample) {
        let mut want = self.profile.default_buffer_ms();
        want = want.saturating_add(sample.jitter_us / 1000);
        want = want.saturating_add(sample.rtt_us / 2000);
        if sample.underruns > 0 {
            want = want.saturating_add(20);
        }
        want = want.clamp(MIN_BUFFER_MS, MAX_BUFFER_MS);
        if want > self.target_buffer_ms {
            self.target_buffer_ms = (self.target_buffer_ms + 10).min(want);
        } else if want < self.target_buffer_ms {
            self.target_buffer_ms = self.target_buffer_ms.saturating_sub(10).max(want);
        }

        let loss = if sample.loss.is_finite() {
            sample.loss.clamp(0.0, 1.0)
        } else {
            1.0
        };
        if loss >= 0.10 || sample.underruns >= 3 {
            self.profile = step_down(self.profile);
            self.bad = 0;
        } else if loss >= 0.04 {
            self.bad = self.bad.saturating_add(1);
            if self.bad >= 2 {
                self.profile = step_down(self.profile);
                self.bad = 0;
            }
        } else {
            self.bad = 0;
        }
    }
}

fn rank(profile: Profile) -> u8 {
    match profile {
        Profile::Pcm => 0,
        Profile::Adpcm48 => 1,
        Profile::Adpcm24 => 2,
    }
}

pub fn step_down(profile: Profile) -> Profile {
    match profile {
        Profile::Pcm => Profile::Adpcm48,
        Profile::Adpcm48 => Profile::Adpcm24,
        Profile::Adpcm24 => Profile::Adpcm24,
    }
}

pub fn step_up(profile: Profile) -> Profile {
    match profile {
        Profile::Adpcm24 => Profile::Adpcm48,
        Profile::Adpcm48 => Profile::Pcm,
        Profile::Pcm => Profile::Pcm,
    }
}

#[derive(Debug, Clone, Copy)]
pub struct ReceiverAdapt {
    pub target_ms: u32,
    stable: u8,
}

impl ReceiverAdapt {
    pub fn new() -> Self {
        Self {
            target_ms: 20,
            stable: 0,
        }
    }

    pub fn observe(&mut self, jitter_us: u32, underrun_delta: u32) -> u32 {
        if underrun_delta > 0 {
            self.target_ms = self.target_ms.saturating_add(15).min(MAX_BUFFER_MS);
            self.stable = 0;
        } else if jitter_us / 1000 + MIN_BUFFER_MS > self.target_ms {
            self.target_ms = self.target_ms.saturating_add(5).min(MAX_BUFFER_MS);
            self.stable = 0;
        } else {
            self.stable = self.stable.saturating_add(1);
            if self.stable >= 3 && self.target_ms > MIN_BUFFER_MS {
                self.target_ms = self.target_ms.saturating_sub(5).max(MIN_BUFFER_MS);
                self.stable = 0;
            }
        }
        self.target_ms
    }
}

pub fn next_pace(scheduled_us: u64, now_us: u64) -> u64 {
    if now_us.saturating_sub(scheduled_us) > 15_000 {
        now_us.saturating_add(5_000)
    } else {
        scheduled_us.saturating_add(5_000)
    }
}

pub fn advance_pace(scheduled: Instant, now: Instant) -> Instant {
    if now.saturating_duration_since(scheduled) > Duration::from_millis(15) {
        now + Duration::from_millis(5)
    } else {
        scheduled + Duration::from_millis(5)
    }
}
