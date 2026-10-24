use std::{collections::BTreeMap, time::Instant};

pub const MAX_SLOTS: usize = 32;
#[derive(Debug)]
struct Packet {
    pcm: Vec<u8>,
    arrived: Instant,
}

#[derive(Debug)]
pub struct JitterBuffer {
    bytes_per_frame: usize,
    packet_frames: usize,
    packet_bytes: usize,
    target_packets: usize,
    slots: BTreeMap<u64, Packet>,
    primed: bool,
    starved: bool,
    started: bool,
    play_seq: Option<u64>,
    play_off: usize,
    pub underruns: u64,
    pub starvations: u64,
    pub losses: u64,
    pub discarded: u64,
    pub arrived: u64,
    pub residence_us: u64,
}
impl JitterBuffer {
    pub fn new(bytes_per_frame: usize, packet_frames: usize) -> Self {
        let bpf = bytes_per_frame.max(1);
        let frames = packet_frames.max(1);
        Self {
            bytes_per_frame: bpf,
            packet_frames: frames,
            packet_bytes: bpf * frames,
            target_packets: 4,
            slots: BTreeMap::new(),
            primed: false,
            starved: false,
            started: false,
            play_seq: None,
            play_off: 0,
            underruns: 0,
            starvations: 0,
            losses: 0,
            discarded: 0,
            arrived: 0,
            residence_us: 0,
        }
    }
    pub fn set_target_packets(&mut self, packets: usize) {
        let target = packets.clamp(1, MAX_SLOTS);
        if target == self.target_packets {
            return;
        }
        if target > self.target_packets
            && self.primed
            && self.queued_frames() < target * self.packet_frames
        {
            self.primed = false;
        }
        self.target_packets = target;
        self.trim_latency();
    }
    pub fn set_target_ms(&mut self, ms: u32) {
        self.set_target_packets(((ms.clamp(20, 150) as usize + 4) / 5).clamp(4, 30));
    }
    pub fn target_ms(&self) -> u32 {
        (self.target_packets as u32) * 5
    }
    pub fn buffered_packets(&self) -> usize {
        self.slots.len()
    }
    pub fn queued_frames(&self) -> usize {
        let Some(&last) = self.slots.keys().next_back() else {
            return 0;
        };
        let first = self
            .play_seq
            .or_else(|| self.slots.keys().next().copied())
            .unwrap_or(last);
        (last.saturating_sub(first).saturating_add(1) as usize * self.packet_frames)
            .saturating_sub(self.play_off)
    }
    fn trim_latency(&mut self) {
        let Some(&last) = self.slots.keys().next_back() else {
            return;
        };
        let Some(play) = self.play_seq else { return };
        if last.saturating_sub(play) as usize > self.target_packets + 2 {
            let new_play = last.saturating_sub(self.target_packets as u64 - 1);
            let old = self.slots.len();
            self.slots.retain(|&seq, _| seq >= new_play);
            self.discarded += (old - self.slots.len()) as u64;
            self.play_seq = Some(new_play);
            self.play_off = 0;
        }
    }
    pub fn push(&mut self, seq: u64, pcm: &[u8]) -> bool {
        if seq == 0 || pcm.len() != self.packet_bytes {
            return false;
        }
        if self.starved {
            self.slots.clear();
            self.play_seq = None;
            self.play_off = 0;
            self.primed = false;
            self.starved = false;
            self.started = false;
        }
        if let Some(play) = self.play_seq {
            if seq < play && self.started {
                self.discarded += 1;
                return false;
            }
            if seq > play.saturating_add((MAX_SLOTS * 2) as u64) {
                self.slots.clear();
                self.play_seq = None;
                self.play_off = 0;
                self.primed = false;
            }
        }
        if self.slots.contains_key(&seq) {
            return false;
        }
        self.slots.insert(
            seq,
            Packet {
                pcm: pcm.to_vec(),
                arrived: Instant::now(),
            },
        );
        self.arrived += 1;
        while self.slots.len() > MAX_SLOTS {
            if let Some(first) = self.slots.keys().next().copied() {
                self.slots.remove(&first);
                self.discarded += 1;
                if self.play_seq.is_some_and(|play| play <= first) {
                    self.play_seq = self.slots.keys().next().copied();
                    self.play_off = 0;
                }
            }
        }
        if self.play_seq.is_none() || !self.started {
            self.play_seq = self.slots.keys().next().copied();
        }
        if !self.primed && self.queued_frames() >= self.target_packets * self.packet_frames {
            self.primed = true;
        }
        self.trim_latency();
        true
    }
    /// Missing packets are concealed; an empty source re-primes instead of advancing forever.
    pub fn pull(&mut self, dst: &mut [u8], frames: usize) -> usize {
        let bpf = self.bytes_per_frame;
        let frames = frames.min(dst.len() / bpf);
        dst[..frames * bpf].fill(0);
        if !self.primed {
            return frames;
        }
        self.started = true;
        let mut done = 0;
        while done < frames {
            let Some(seq) = self.play_seq else { break };
            let off = self.play_off.min(self.packet_frames);
            let n = (self.packet_frames - off).min(frames - done);
            if n == 0 {
                self.play_seq = Some(seq.saturating_add(1));
                self.play_off = 0;
                continue;
            }
            if let Some(packet) = self.slots.get(&seq) {
                dst[done * bpf..(done + n) * bpf]
                    .copy_from_slice(&packet.pcm[off * bpf..(off + n) * bpf]);
                self.residence_us = packet
                    .arrived
                    .elapsed()
                    .as_micros()
                    .min(u128::from(u64::MAX)) as u64;
            } else if self.slots.range(seq..).next().is_none() {
                self.underruns += (frames - done) as u64;
                self.starvations += 1;
                self.primed = false;
                self.starved = true;
                self.play_seq = None;
                self.play_off = 0;
                break;
            } else {
                self.underruns += n as u64;
            }
            done += n;
            self.play_off += n;
            if self.play_off >= self.packet_frames {
                if self.slots.remove(&seq).is_none() {
                    self.losses += 1;
                }
                self.play_seq = Some(seq.saturating_add(1));
                self.play_off = 0;
            }
        }
        frames
    }
}
