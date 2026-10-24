use std::collections::BTreeMap;

pub const MAX_SLOTS: usize = 32;

#[derive(Debug)]
pub struct JitterBuffer {
    bytes_per_frame: usize,
    packet_frames: usize,
    packet_bytes: usize,
    target_packets: usize,
    slots: BTreeMap<u64, Vec<u8>>,
    primed: bool,
    starved: bool,
    starve_packets: usize,
    play_seq: Option<u64>,
    play_off: usize,
    pub underruns: u64,
    pub losses: u64,
    pub discarded: u64,
    pub arrived: u64,
}

impl JitterBuffer {
    pub fn new(bytes_per_frame: usize, packet_frames: usize) -> Self {
        let packet_frames = packet_frames.max(1);
        Self {
            bytes_per_frame: bytes_per_frame.max(1),
            packet_frames,
            packet_bytes: bytes_per_frame.max(1) * packet_frames,
            target_packets: 4,
            slots: BTreeMap::new(),
            primed: false,
            starved: false,
            starve_packets: 0,
            play_seq: None,
            play_off: 0,
            underruns: 0,
            losses: 0,
            discarded: 0,
            arrived: 0,
        }
    }

    pub fn set_target_packets(&mut self, packets: usize) {
        let new_target = packets.clamp(1, MAX_SLOTS);
        if new_target == self.target_packets {
            return;
        }
        if self.primed && new_target > self.target_packets && self.slots.len() < new_target {
            self.primed = false;
        }
        self.target_packets = new_target;
        while self.slots.len() > new_target + 2 {
            if let Some(seq) = self.slots.keys().next().copied() {
                self.slots.remove(&seq);
                self.discarded += 1;
                self.play_seq = self.slots.keys().next().copied();
                self.play_off = 0;
            } else {
                break;
            }
        }
    }

    pub fn set_target_ms(&mut self, ms: u32) {
        let ms = ms.clamp(15, 150);
        let packets = (usize::try_from(ms).unwrap_or(15) + 4) / 5;
        self.set_target_packets(packets.clamp(3, 30));
    }

    pub fn target_ms(&self) -> u32 {
        (self.target_packets as u32) * 5
    }

    pub fn buffered_packets(&self) -> usize {
        self.slots.len()
    }

    pub fn push(&mut self, seq: u64, pcm: &[u8]) -> bool {
        if pcm.len() != self.packet_bytes || self.bytes_per_frame == 0 {
            return false;
        }
        if self.starved {
            self.slots.clear();
            self.play_seq = None;
            self.play_off = 0;
            self.primed = false;
            self.starved = false;
            self.starve_packets = 0;
        }
        if let Some(play) = self.play_seq {
            if seq < play {
                self.discarded += 1;
                return false;
            }
            if seq > play.saturating_add(MAX_SLOTS as u64) {
                let new_play = seq.saturating_sub(self.target_packets as u64);
                self.losses += new_play.saturating_sub(play);
                self.play_seq = Some(new_play);
                self.play_off = 0;
                self.slots.retain(|&s, _| s >= new_play);
            }
        }
        if self.slots.contains_key(&seq) {
            return false;
        }
        if self.slots.len() >= MAX_SLOTS {
            let Some(oldest) = self.slots.keys().next().copied() else {
                return false;
            };
            if seq < oldest {
                self.discarded += 1;
                return false;
            }
            self.slots.remove(&oldest);
            self.discarded += 1;
        }
        self.slots.insert(seq, pcm.to_vec());
        self.arrived += 1;
        if !self.primed && self.slots.len() >= self.target_packets {
            self.primed = true;
            if self.play_seq.is_none() {
                self.play_seq = self.slots.keys().next().copied();
                self.play_off = 0;
            }
        }
        true
    }

    /// Always fills `frames` with audio or silence. Empty and gapped input does not underflow.
    pub fn pull(&mut self, dst: &mut [u8], frames: usize) -> usize {
        let bpf = self.bytes_per_frame;
        if bpf == 0 || frames == 0 || dst.len() < bpf {
            return 0;
        }
        let frames = frames.min(dst.len() / bpf);
        dst[..frames * bpf].fill(0);
        if !self.primed {
            return frames;
        }
        let mut done = 0;
        while done < frames {
            let Some(seq) = self.play_seq else {
                self.underruns += (frames - done) as u64;
                break;
            };
            let off = self.play_off.min(self.packet_frames);
            let avail = self.packet_frames.saturating_sub(off);
            if avail == 0 {
                self.play_seq = Some(seq.saturating_add(1));
                self.play_off = 0;
                continue;
            }
            let n = avail.min(frames - done);
            if self.slots.contains_key(&seq) {
                let pkt = &self.slots[&seq];
                let src = off * bpf;
                let dst_off = done * bpf;
                let nbytes = n * bpf;
                if src + nbytes <= pkt.len() && dst_off + nbytes <= dst.len() {
                    dst[dst_off..dst_off + nbytes].copy_from_slice(&pkt[src..src + nbytes]);
                }
                done += n;
                self.starve_packets = 0;
                self.advance(seq, n);
            } else {
                self.underruns += n as u64;
                done += n;
                let empty = self.slots.is_empty();
                self.advance(seq, n);
                if empty {
                    self.starve_packets = self.starve_packets.saturating_add(1);
                    if self.starve_packets >= self.target_packets {
                        self.starved = true;
                    }
                } else {
                    self.starve_packets = 0;
                }
            }
        }
        frames
    }

    fn advance(&mut self, seq: u64, frames: usize) {
        self.play_off += frames;
        if self.play_off >= self.packet_frames {
            if !self.slots.contains_key(&seq) {
                self.losses += 1;
            }
            self.slots.remove(&seq);
            self.play_seq = Some(seq.saturating_add(1));
            self.play_off = 0;
        }
    }
}
