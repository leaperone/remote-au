//! PCM16 and packet-local IMA ADPCM.
//!
//! ADPCM resets the predictor on every packet so a loss cannot poison the next
//! packet. 48 kHz stereo is about 384 kbps and is noticeably noisier than PCM.
//! 24 kHz mono is about 96 kbps: a cheap average downsample, narrow and not hi-fi.

use super::format::{MAX_PCM_CHUNK, WIRE_FRAME_SAMPLES};

pub const CODEC_PCM: u8 = 0;
pub const CODEC_ADPCM48: u8 = 1;
pub const CODEC_ADPCM24: u8 = 2;

const ADPCM48_BYTES: usize = 246;
const ADPCM24_BYTES: usize = 64;
const MONO_24K_SAMPLES: usize = 120;

const INDEX_TABLE: [i32; 16] = [-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8];

const STEP_TABLE: [i32; 89] = [
    7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45, 50, 55, 60, 66,
    73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230, 253, 279, 307, 337, 371, 408, 449,
    494, 544, 598, 658, 724, 796, 876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272,
    2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493,
    10442, 11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
];

pub fn encode_canonical(codec: u8, pcm960: &[u8]) -> Option<Vec<u8>> {
    if pcm960.len() != MAX_PCM_CHUNK {
        return None;
    }
    match codec {
        CODEC_PCM => Some(pcm960.to_vec()),
        CODEC_ADPCM48 => Some(encode_adpcm_stereo(pcm960)),
        CODEC_ADPCM24 => Some(encode_adpcm_mono24(pcm960)),
        _ => None,
    }
}

pub fn decode_canonical(codec: u8, payload: &[u8]) -> Option<Vec<u8>> {
    match codec {
        CODEC_PCM if payload.len() == MAX_PCM_CHUNK => Some(payload.to_vec()),
        CODEC_ADPCM48 if payload.len() == ADPCM48_BYTES => decode_adpcm_stereo(payload),
        CODEC_ADPCM24 if payload.len() == ADPCM24_BYTES => decode_adpcm_mono24(payload),
        _ => None,
    }
}

pub fn codec_name(codec: u8) -> &'static str {
    match codec {
        CODEC_PCM => "pcm16-48k-stereo",
        CODEC_ADPCM48 => "adpcm-48k-stereo",
        CODEC_ADPCM24 => "adpcm-24k-mono",
        _ => "unknown",
    }
}

fn read_i16(pcm: &[u8], frame: usize, channel: usize) -> i16 {
    let i = (frame * 2 + channel) * 2;
    i16::from_le_bytes([pcm[i], pcm[i + 1]])
}

fn write_i16(pcm: &mut [u8], frame: usize, channel: usize, sample: i16) {
    let i = (frame * 2 + channel) * 2;
    pcm[i..i + 2].copy_from_slice(&sample.to_le_bytes());
}

fn encode_nibble(sample: i16, pred: &mut i32, index: &mut i32) -> u8 {
    let step = STEP_TABLE[*index as usize];
    let mut delta = i32::from(sample) - *pred;
    let mut nibble = 0u8;
    if delta < 0 {
        nibble = 8;
        delta = -delta;
    }
    let mut vpdiff = step >> 3;
    if delta >= step {
        nibble |= 4;
        delta -= step;
        vpdiff += step;
    }
    if delta >= step >> 1 {
        nibble |= 2;
        delta -= step >> 1;
        vpdiff += step >> 1;
    }
    if delta >= step >> 2 {
        nibble |= 1;
        vpdiff += step >> 2;
    }
    if nibble & 8 != 0 {
        *pred -= vpdiff;
    } else {
        *pred += vpdiff;
    }
    *pred = (*pred).clamp(-32768, 32767);
    *index = (*index + INDEX_TABLE[nibble as usize]).clamp(0, 88);
    nibble
}

fn decode_nibble(nibble: u8, pred: &mut i32, index: &mut i32) -> i16 {
    let step = STEP_TABLE[*index as usize];
    let n = nibble & 0x0f;
    let mut diff = step >> 3;
    if n & 1 != 0 {
        diff += step >> 2;
    }
    if n & 2 != 0 {
        diff += step >> 1;
    }
    if n & 4 != 0 {
        diff += step;
    }
    if n & 8 != 0 {
        *pred -= diff;
    } else {
        *pred += diff;
    }
    *pred = (*pred).clamp(-32768, 32767);
    *index = (*index + INDEX_TABLE[n as usize]).clamp(0, 88);
    *pred as i16
}

fn encode_adpcm_stereo(pcm: &[u8]) -> Vec<u8> {
    let mut pred_l = i32::from(read_i16(pcm, 0, 0));
    let mut pred_r = i32::from(read_i16(pcm, 0, 1));
    let mut idx_l = 0i32;
    let mut idx_r = 0i32;
    let mut out = Vec::with_capacity(ADPCM48_BYTES);
    out.extend_from_slice(&(pred_l as i16).to_le_bytes());
    out.push(0);
    out.extend_from_slice(&(pred_r as i16).to_le_bytes());
    out.push(0);
    for frame in 0..usize::from(WIRE_FRAME_SAMPLES) {
        let nl = encode_nibble(read_i16(pcm, frame, 0), &mut pred_l, &mut idx_l);
        let nr = encode_nibble(read_i16(pcm, frame, 1), &mut pred_r, &mut idx_r);
        out.push(nl | (nr << 4));
    }
    out
}

fn decode_adpcm_stereo(payload: &[u8]) -> Option<Vec<u8>> {
    if payload.len() != ADPCM48_BYTES {
        return None;
    }
    let mut pred_l = i32::from(i16::from_le_bytes([payload[0], payload[1]]));
    let mut idx_l = i32::from(payload[2]).clamp(0, 88);
    let mut pred_r = i32::from(i16::from_le_bytes([payload[3], payload[4]]));
    let mut idx_r = i32::from(payload[5]).clamp(0, 88);
    let mut pcm = vec![0u8; MAX_PCM_CHUNK];
    for (frame, byte) in payload[6..].iter().copied().enumerate() {
        if frame >= usize::from(WIRE_FRAME_SAMPLES) {
            return None;
        }
        let l = decode_nibble(byte & 0x0f, &mut pred_l, &mut idx_l);
        let r = decode_nibble(byte >> 4, &mut pred_r, &mut idx_r);
        write_i16(&mut pcm, frame, 0, l);
        write_i16(&mut pcm, frame, 1, r);
    }
    Some(pcm)
}

fn downsample_24k_mono(pcm: &[u8]) -> [i16; MONO_24K_SAMPLES] {
    let mut out = [0i16; MONO_24K_SAMPLES];
    for i in 0..MONO_24K_SAMPLES {
        let a = i32::from(read_i16(pcm, i * 2, 0)) + i32::from(read_i16(pcm, i * 2, 1));
        let b = i32::from(read_i16(pcm, i * 2 + 1, 0)) + i32::from(read_i16(pcm, i * 2 + 1, 1));
        out[i] = ((a + b) / 4) as i16;
    }
    out
}

fn upsample_24k_mono(mono: &[i16], pcm: &mut [u8]) {
    for (i, sample) in mono.iter().copied().enumerate() {
        write_i16(pcm, i * 2, 0, sample);
        write_i16(pcm, i * 2, 1, sample);
        write_i16(pcm, i * 2 + 1, 0, sample);
        write_i16(pcm, i * 2 + 1, 1, sample);
    }
}

fn encode_adpcm_mono24(pcm: &[u8]) -> Vec<u8> {
    let mono = downsample_24k_mono(pcm);
    let mut pred = i32::from(mono[0]);
    let mut index = 0i32;
    let mut out = Vec::with_capacity(ADPCM24_BYTES);
    out.extend_from_slice(&(pred as i16).to_le_bytes());
    out.push(0);
    out.push(0);
    let mut byte = 0u8;
    let mut which = 0;
    for sample in mono {
        let n = encode_nibble(sample, &mut pred, &mut index);
        if which == 0 {
            byte = n;
            which = 1;
        } else {
            out.push(byte | (n << 4));
            which = 0;
        }
    }
    out
}

fn decode_adpcm_mono24(payload: &[u8]) -> Option<Vec<u8>> {
    if payload.len() != ADPCM24_BYTES {
        return None;
    }
    let mut pred = i32::from(i16::from_le_bytes([payload[0], payload[1]]));
    let mut index = i32::from(payload[2]).clamp(0, 88);
    let mut mono = [0i16; MONO_24K_SAMPLES];
    let mut n = 0;
    for byte in payload[4..].iter().copied() {
        if n + 1 >= MONO_24K_SAMPLES {
            return None;
        }
        mono[n] = decode_nibble(byte & 0x0f, &mut pred, &mut index);
        mono[n + 1] = decode_nibble(byte >> 4, &mut pred, &mut index);
        n += 2;
    }
    if n != MONO_24K_SAMPLES {
        return None;
    }
    let mut pcm = vec![0u8; MAX_PCM_CHUNK];
    upsample_24k_mono(&mono, &mut pcm);
    Some(pcm)
}
