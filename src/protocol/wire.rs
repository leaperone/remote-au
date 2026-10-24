use super::format::MAX_DATAGRAM;

pub const MAGIC: &[u8; 4] = b"RAU2";
pub const VERSION: u8 = 2;
pub const HEADER_LEN: usize = 46;

pub const KIND_HELLO: u8 = 1;
pub const KIND_AUDIO: u8 = 2;
pub const KIND_FEEDBACK: u8 = 3;
pub const KIND_PROBE: u8 = 4;

pub const AUDIO_PREFIX: usize = 19;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AudioBody {
    pub seq: u64,
    pub send_ts_us: u64,
    pub codec: u8,
    pub buffer_ms: u16,
    pub payload: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FeedbackBody {
    pub echo_send_ts_us: u64,
    pub delay_us: u32,
    pub echo_seq: u64,
    pub received: u32,
    pub lost: u32,
    pub jitter_us: u32,
    pub underruns: u32,
    pub buffer_ms: u16,
    pub probe_gen: u32,
    pub probe_bytes: [u32; 5],
    /// Per-step receive span (last−first arrival), microseconds; 0 when no probe bytes for that step.
    pub probe_recv_span_us: [u32; 5],
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProbeBody {
    pub gen: u32,
    pub step: u8,
    pub send_ts_us: u64,
    pub payload: Vec<u8>,
}

pub struct HeaderView<'a> {
    pub kind: u8,
    pub epoch: [u8; 16],
    pub session: [u8; 16],
    pub counter: u64,
    pub aad: &'a [u8],
    pub ciphertext: &'a [u8],
}

pub fn parse_packet(pkt: &[u8]) -> Option<HeaderView<'_>> {
    if pkt.len() < HEADER_LEN + 16 || pkt.len() > MAX_DATAGRAM {
        return None;
    }
    if pkt.get(..4)? != MAGIC.as_slice() || pkt.get(4)? != &VERSION {
        return None;
    }
    let kind = *pkt.get(5)?;
    if !(KIND_HELLO..=KIND_PROBE).contains(&kind) {
        return None;
    }
    let mut epoch = [0u8; 16];
    epoch.copy_from_slice(pkt.get(6..22)?);
    let mut session = [0u8; 16];
    session.copy_from_slice(pkt.get(22..38)?);
    let counter = u64::from_be_bytes(pkt.get(38..46)?.try_into().ok()?);
    if counter == 0 {
        return None;
    }
    Some(HeaderView {
        kind,
        epoch,
        session,
        counter,
        aad: &pkt[..HEADER_LEN],
        ciphertext: &pkt[HEADER_LEN..],
    })
}

pub fn assemble(
    kind: u8,
    epoch: &[u8; 16],
    session: &[u8; 16],
    counter: u64,
    ct: &[u8],
) -> Option<Vec<u8>> {
    if counter == 0 || HEADER_LEN + ct.len() > MAX_DATAGRAM {
        return None;
    }
    let mut out = vec![0u8; HEADER_LEN + ct.len()];
    out[..4].copy_from_slice(MAGIC);
    out[4] = VERSION;
    out[5] = kind;
    out[6..22].copy_from_slice(epoch);
    out[22..38].copy_from_slice(session);
    out[38..46].copy_from_slice(&counter.to_be_bytes());
    out[HEADER_LEN..].copy_from_slice(ct);
    Some(out)
}

pub fn encode_hello(name: &str) -> Option<Vec<u8>> {
    let name = clip_name(name);
    if name.len() > 64 {
        return None;
    }
    let mut out = Vec::with_capacity(1 + name.len());
    out.push(u8::try_from(name.len()).ok()?);
    out.extend_from_slice(name.as_bytes());
    Some(out)
}

pub fn decode_hello(body: &[u8]) -> Option<String> {
    let n = usize::from(*body.first()?);
    if body.len() != 1 + n || n > 64 {
        return None;
    }
    let name = std::str::from_utf8(body.get(1..1 + n)?).ok()?;
    Some(name.to_string())
}

pub fn encode_audio(body: &AudioBody) -> Option<Vec<u8>> {
    if body.payload.len() > MAX_DATAGRAM {
        return None;
    }
    let mut out = Vec::with_capacity(AUDIO_PREFIX + body.payload.len());
    out.extend_from_slice(&body.seq.to_be_bytes());
    out.extend_from_slice(&body.send_ts_us.to_be_bytes());
    out.push(body.codec);
    out.extend_from_slice(&body.buffer_ms.to_be_bytes());
    out.extend_from_slice(&body.payload);
    Some(out)
}

pub fn decode_audio(body: &[u8]) -> Option<AudioBody> {
    if body.len() < AUDIO_PREFIX || body.len() > MAX_DATAGRAM {
        return None;
    }
    let payload = body.get(AUDIO_PREFIX..)?.to_vec();
    Some(AudioBody {
        seq: u64::from_be_bytes(body.get(..8)?.try_into().ok()?),
        send_ts_us: u64::from_be_bytes(body.get(8..16)?.try_into().ok()?),
        codec: *body.get(16)?,
        buffer_ms: u16::from_be_bytes(body.get(17..19)?.try_into().ok()?),
        payload,
    })
}

pub const FEEDBACK_BODY_LEN: usize = 82;

pub fn encode_feedback(body: &FeedbackBody) -> Vec<u8> {
    let mut out = Vec::with_capacity(FEEDBACK_BODY_LEN);
    out.extend_from_slice(&body.echo_send_ts_us.to_be_bytes());
    out.extend_from_slice(&body.delay_us.to_be_bytes());
    out.extend_from_slice(&body.echo_seq.to_be_bytes());
    out.extend_from_slice(&body.received.to_be_bytes());
    out.extend_from_slice(&body.lost.to_be_bytes());
    out.extend_from_slice(&body.jitter_us.to_be_bytes());
    out.extend_from_slice(&body.underruns.to_be_bytes());
    out.extend_from_slice(&body.buffer_ms.to_be_bytes());
    out.extend_from_slice(&body.probe_gen.to_be_bytes());
    for n in body.probe_bytes {
        out.extend_from_slice(&n.to_be_bytes());
    }
    for n in body.probe_recv_span_us {
        out.extend_from_slice(&n.to_be_bytes());
    }
    out
}

pub fn decode_feedback(body: &[u8]) -> Option<FeedbackBody> {
    if body.len() != FEEDBACK_BODY_LEN {
        return None;
    }
    let mut probe_bytes = [0u32; 5];
    for (i, slot) in probe_bytes.iter_mut().enumerate() {
        let o = 42 + i * 4;
        *slot = u32::from_be_bytes(body.get(o..o + 4)?.try_into().ok()?);
    }
    let mut probe_recv_span_us = [0u32; 5];
    for (i, slot) in probe_recv_span_us.iter_mut().enumerate() {
        let o = 62 + i * 4;
        *slot = u32::from_be_bytes(body.get(o..o + 4)?.try_into().ok()?);
    }
    Some(FeedbackBody {
        echo_send_ts_us: u64::from_be_bytes(body.get(..8)?.try_into().ok()?),
        delay_us: u32::from_be_bytes(body.get(8..12)?.try_into().ok()?),
        echo_seq: u64::from_be_bytes(body.get(12..20)?.try_into().ok()?),
        received: u32::from_be_bytes(body.get(20..24)?.try_into().ok()?),
        lost: u32::from_be_bytes(body.get(24..28)?.try_into().ok()?),
        jitter_us: u32::from_be_bytes(body.get(28..32)?.try_into().ok()?),
        underruns: u32::from_be_bytes(body.get(32..36)?.try_into().ok()?),
        buffer_ms: u16::from_be_bytes(body.get(36..38)?.try_into().ok()?),
        probe_gen: u32::from_be_bytes(body.get(38..42)?.try_into().ok()?),
        probe_bytes,
        probe_recv_span_us,
    })
}

pub fn encode_probe(body: &ProbeBody) -> Option<Vec<u8>> {
    if body.payload.len() > 1000 || body.step > 4 {
        return None;
    }
    let mut out = Vec::with_capacity(13 + body.payload.len());
    out.extend_from_slice(&body.gen.to_be_bytes());
    out.push(body.step);
    out.extend_from_slice(&body.send_ts_us.to_be_bytes());
    out.extend_from_slice(&body.payload);
    Some(out)
}

pub fn decode_probe(body: &[u8]) -> Option<ProbeBody> {
    if body.len() < 13 || body.len() > 13 + 1000 {
        return None;
    }
    let step = *body.get(4)?;
    if step > 4 {
        return None;
    }
    Some(ProbeBody {
        gen: u32::from_be_bytes(body.get(..4)?.try_into().ok()?),
        step,
        send_ts_us: u64::from_be_bytes(body.get(5..13)?.try_into().ok()?),
        payload: body.get(13..)?.to_vec(),
    })
}

pub fn clip_name(name: &str) -> String {
    let mut out = String::new();
    for ch in name.chars() {
        if out.len() + ch.len_utf8() > 64 {
            break;
        }
        out.push(ch);
    }
    out
}
