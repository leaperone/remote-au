use std::net::SocketAddr;

use serde_json::{json, Value};

use crate::crypto::{random_id, DirectionKeys, ReplayWindow};
use crate::protocol::{
    decode_feedback, encode_audio, encode_canonical, encode_hello, encode_probe, AudioBody,
    ProbeBody, KIND_AUDIO, KIND_FEEDBACK, KIND_HELLO, KIND_PROBE,
};
use crate::transport::adapt::{AdaptState, PathSample, Profile};
use crate::transport::packet::{open_packet, seal_packet};

#[derive(Clone, Debug)]
pub struct SenderSnapshot {
    pub peers: Vec<SenderPeer>,
    pub codec: String,
    pub rtt_us: u32,
    pub loss: f32,
    pub jitter_us: u32,
    pub estimated_goodput_bps: u64,
    pub target_buffer_ms: u32,
}

#[derive(Clone, Debug)]
pub struct SenderPeer {
    pub addr: String,
    pub name: String,
    pub epoch: String,
}

impl SenderSnapshot {
    pub fn to_json(&self) -> Value {
        json!({
            "role": "sender",
            "peers": self.peers.iter().map(|p| json!({
                "addr": p.addr,
                "name": p.name,
                "epoch": p.epoch,
            })).collect::<Vec<_>>(),
            "codec": self.codec,
            "rtt_us": self.rtt_us,
            "loss": self.loss,
            "jitter_us": self.jitter_us,
            "estimated_goodput_bps": self.estimated_goodput_bps,
            "goodput_basis": "paced_probe_floor",
            "target_buffer_ms": self.target_buffer_ms,
        })
    }
}

pub struct SenderEngine {
    token: [u8; 32],
    session: [u8; 16],
    name: String,
    keys: Option<DirectionKeys>,
    epoch: Option<[u8; 16]>,
    addr: Option<SocketAddr>,
    peer_name: String,
    instance: Option<[u8; 16]>,
    counter: u64,
    replay: ReplayWindow,
    audio_seq: u64,
    adapt: AdaptState,
    last_recv: u32,
    last_lost: u32,
    last_underruns: u32,
    rtt_us: u32,
    jitter_us: u32,
    loss: f32,
    goodput_bps: u64,
    probe_bytes: [u32; 5],
    probe_recv_span_us: [u32; 5],
    probe_send_first_us: [u64; 5],
    probe_send_last_us: [u64; 5],
    pub probe_gen: u32,
}

impl SenderEngine {
    pub fn new(token: [u8; 32], name: String) -> Self {
        Self {
            token,
            session: random_id(),
            name,
            keys: None,
            epoch: None,
            addr: None,
            peer_name: String::new(),
            instance: None,
            counter: 0,
            replay: ReplayWindow::new(2048),
            audio_seq: 0,
            adapt: AdaptState::start(),
            last_recv: 0,
            last_lost: 0,
            last_underruns: 0,
            rtt_us: 0,
            jitter_us: 0,
            loss: 0.0,
            goodput_bps: 0,
            probe_bytes: [0; 5],
            probe_recv_span_us: [0; 5],
            probe_send_first_us: [0; 5],
            probe_send_last_us: [0; 5],
            probe_gen: 1,
        }
    }

    pub fn session(&self) -> [u8; 16] {
        self.session
    }

    pub fn profile(&self) -> Profile {
        self.adapt.profile
    }

    pub fn addr(&self) -> Option<SocketAddr> {
        self.addr
    }

    pub fn instance(&self) -> Option<[u8; 16]> {
        self.instance
    }

    pub fn epoch(&self) -> Option<[u8; 16]> {
        self.epoch
    }

    /// Returns true when the receiver epoch changed and counters were reset.
    pub fn bind_peer(
        &mut self,
        epoch: [u8; 16],
        addr: SocketAddr,
        name: String,
        instance: [u8; 16],
    ) -> bool {
        let rekeyed = self.epoch != Some(epoch);
        self.addr = Some(addr);
        self.peer_name = name;
        self.instance = Some(instance);
        if rekeyed {
            self.epoch = Some(epoch);
            self.keys = Some(DirectionKeys::derive(&self.token, &epoch, &self.session));
            self.counter = 0;
            self.replay = ReplayWindow::new(2048);
            self.audio_seq = 0;
            self.last_recv = 0;
            self.last_lost = 0;
            self.last_underruns = 0;
            self.probe_bytes = [0; 5];
            self.probe_recv_span_us = [0; 5];
            self.probe_send_first_us = [0; 5];
            self.probe_send_last_us = [0; 5];
        }
        rekeyed
    }

    pub fn begin_probe(&mut self) {
        self.probe_gen = self.probe_gen.wrapping_add(1).max(1);
        self.probe_bytes = [0; 5];
        self.probe_recv_span_us = [0; 5];
        self.probe_send_first_us = [0; 5];
        self.probe_send_last_us = [0; 5];
    }

    pub fn hello(&mut self) -> Option<Vec<u8>> {
        let body = encode_hello(&self.name)?;
        self.seal(KIND_HELLO, &body)
    }

    pub fn audio(&mut self, pcm960: &[u8], now_us: u64) -> Option<Vec<u8>> {
        let payload = encode_canonical(self.adapt.profile.codec(), pcm960)?;
        self.audio_seq = self.audio_seq.saturating_add(1);
        let body = encode_audio(&AudioBody {
            seq: self.audio_seq,
            send_ts_us: now_us,
            codec: self.adapt.profile.codec(),
            buffer_ms: u16::try_from(self.adapt.target_buffer_ms).unwrap_or(u16::MAX),
            payload,
        })?;
        self.seal(KIND_AUDIO, &body)
    }

    pub fn probe(&mut self, step: u8, payload: usize, now_us: u64) -> Option<Vec<u8>> {
        let idx = usize::from(step);
        if idx < self.probe_send_first_us.len() {
            if self.probe_send_first_us[idx] == 0 {
                self.probe_send_first_us[idx] = now_us;
            }
            self.probe_send_last_us[idx] = now_us;
        }
        let body = encode_probe(&ProbeBody {
            gen: self.probe_gen,
            step,
            send_ts_us: now_us,
            payload: vec![0u8; payload],
        })?;
        self.seal(KIND_PROBE, &body)
    }

    pub fn handle_feedback(&mut self, packet: &[u8], now_us: u64) -> bool {
        let Some(keys) = self.keys.clone() else {
            return false;
        };
        let Some(epoch) = self.epoch else {
            return false;
        };
        let Some(opened) = open_packet(&keys.receiver_to_sender, packet) else {
            return false;
        };
        if opened.epoch != epoch || opened.session != self.session || opened.kind != KIND_FEEDBACK {
            return false;
        }
        if !self.replay.commit(opened.counter) {
            return false;
        }
        let Some(feedback) = decode_feedback(&opened.body) else {
            return false;
        };
        let rtt = now_us
            .saturating_sub(feedback.echo_send_ts_us)
            .saturating_sub(u64::from(feedback.delay_us));
        self.rtt_us = u32::try_from(rtt).unwrap_or(u32::MAX);
        self.jitter_us = feedback.jitter_us;
        let recv_delta = feedback.received.saturating_sub(self.last_recv);
        let lost_delta = feedback.lost.saturating_sub(self.last_lost);
        self.last_recv = feedback.received;
        self.last_lost = feedback.lost;
        let denom = recv_delta.saturating_add(lost_delta);
        self.loss = if denom == 0 {
            0.0
        } else {
            lost_delta as f32 / denom as f32
        };
        let underrun_delta = feedback.underruns.saturating_sub(self.last_underruns);
        self.last_underruns = feedback.underruns;
        if feedback.probe_gen == self.probe_gen {
            self.probe_bytes = feedback.probe_bytes;
            self.probe_recv_span_us = feedback.probe_recv_span_us;
        }
        self.adapt.observe(PathSample {
            loss: self.loss,
            rtt_us: self.rtt_us,
            jitter_us: self.jitter_us,
            underruns: underrun_delta,
        });
        true
    }

    pub fn note_probe(&mut self, sent: [u32; 5], gradual: bool) {
        let steps: Vec<(u64, u32, u32, u32, u32)> = crate::transport::adapt::PROBE_STEPS_BPS
            .iter()
            .enumerate()
            .map(|(i, rate)| {
                let send_us = span_us(self.probe_send_first_us[i], self.probe_send_last_us[i]);
                (
                    *rate,
                    sent[i],
                    self.probe_bytes[i],
                    send_us,
                    self.probe_recv_span_us[i],
                )
            })
            .collect();
        let floor = crate::transport::adapt::highest_sustainable_step_timed(&steps);
        self.goodput_bps = floor.unwrap_or(0);
        self.adapt.apply_floor(floor, gradual);
    }

    pub fn snapshot(&self) -> SenderSnapshot {
        let peers = match (self.addr, self.epoch) {
            (Some(addr), Some(epoch)) => vec![SenderPeer {
                addr: addr.to_string(),
                name: self.peer_name.clone(),
                epoch: hex::encode(epoch),
            }],
            _ => Vec::new(),
        };
        SenderSnapshot {
            peers,
            codec: self.adapt.profile.name().to_string(),
            rtt_us: self.rtt_us,
            loss: self.loss,
            jitter_us: self.jitter_us,
            estimated_goodput_bps: self.goodput_bps,
            target_buffer_ms: self.adapt.target_buffer_ms,
        }
    }

    fn seal(&mut self, kind: u8, body: &[u8]) -> Option<Vec<u8>> {
        let epoch = self.epoch?;
        self.counter = self.counter.saturating_add(1);
        if self.counter == 0 {
            return None;
        }
        let counter = self.counter;
        let session = self.session;
        let keys = self.keys.as_ref()?;
        seal_packet(
            &keys.sender_to_receiver,
            &epoch,
            &session,
            counter,
            kind,
            body,
        )
    }
}

fn span_us(first: u64, last: u64) -> u32 {
    if first == 0 {
        return 0;
    }
    if last <= first {
        return u32::MAX;
    }
    u32::try_from(last - first).unwrap_or(u32::MAX).max(1)
}
