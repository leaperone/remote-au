//! Convert native PCM to interleaved i16 LE at a target rate/channel layout.

pub const TARGET_RATE: u32 = 48_000;
/// Linear resample mono `samples` from `in_rate` to `out_rate`, append to `out`.
fn resample_mono(samples: &[f32], in_rate: u32, out_rate: u32, out: &mut Vec<i16>) {
    if samples.is_empty() {
        return;
    }
    if in_rate == out_rate {
        for &s in samples {
            out.push(float_to_i16(s));
        }
        return;
    }
    let ratio = in_rate as f64 / out_rate as f64;
    let out_frames = ((samples.len() as f64) / ratio).floor() as usize;
    for i in 0..out_frames {
        let src = i as f64 * ratio;
        let idx = src.floor() as usize;
        let frac = (src - idx as f64) as f32;
        let a = samples.get(idx).copied().unwrap_or(0.0);
        let b = samples.get(idx + 1).copied().unwrap_or(a);
        out.push(float_to_i16(a + (b - a) * frac));
    }
}

fn float_to_i16(v: f32) -> i16 {
    let clamped = v.clamp(-1.0, 1.0);
    (clamped * 32767.0).round() as i16
}

fn downmix_to_stereo(frame: &[f32], channels: usize) -> [f32; 2] {
    match channels {
        0 => [0.0, 0.0],
        1 => [frame[0], frame[0]],
        _ => {
            let l = frame[0];
            let r = frame.get(1).copied().unwrap_or(frame[0]);
            [l, r]
        }
    }
}

/// Interleaved F32 frames → i16 LE stereo @ 48 kHz bytes appended to `out`.
pub fn append_f32_interleaved(out: &mut Vec<u8>, data: &[f32], in_channels: usize, in_rate: u32) {
    if in_channels == 0 || data.is_empty() {
        return;
    }
    let in_frames = data.len() / in_channels;
    let mut mono_l = Vec::with_capacity(in_frames);
    let mut mono_r = Vec::with_capacity(in_frames);
    for f in 0..in_frames {
        let base = f * in_channels;
        let frame = &data[base..base + in_channels];
        let [l, r] = downmix_to_stereo(frame, in_channels);
        mono_l.push(l);
        mono_r.push(r);
    }
    let mut li = Vec::new();
    let mut ri = Vec::new();
    resample_mono(&mono_l, in_rate, TARGET_RATE, &mut li);
    resample_mono(&mono_r, in_rate, TARGET_RATE, &mut ri);
    let frames = li.len().min(ri.len());
    for i in 0..frames {
        out.extend_from_slice(&li[i].to_le_bytes());
        out.extend_from_slice(&ri[i].to_le_bytes());
    }
}

/// Interleaved i16 frames → i16 LE stereo @ 48 kHz bytes appended to `out`.
pub fn append_i16_interleaved(out: &mut Vec<u8>, data: &[i16], in_channels: usize, in_rate: u32) {
    if in_channels == 0 || data.is_empty() {
        return;
    }
    let mut floats = Vec::with_capacity(data.len());
    for &s in data {
        floats.push(s as f32 / 32768.0);
    }
    append_f32_interleaved(out, &floats, in_channels, in_rate);
}

/// Session i16 LE interleaved (`session_channels`) → device interleaved F32 output buffer.
pub fn fill_device_f32_from_session_i16(
    device_out: &mut [f32],
    session: &[u8],
    device_channels: usize,
    session_channels: usize,
) {
    let device_frames = device_out.len() / device_channels.max(1);
    let session_bpf = session_channels.max(1) * 2;
    for f in 0..device_frames {
        let s_off = f * session_bpf;
        let (l, r) = if s_off + 1 < session.len() {
            let l = i16::from_le_bytes([session[s_off], session[s_off + 1]]);
            let r_off = s_off + 2;
            let r = if session_channels >= 2 && r_off + 1 < session.len() {
                i16::from_le_bytes([session[r_off], session[r_off + 1]])
            } else {
                l
            };
            (l as f32 / 32768.0, r as f32 / 32768.0)
        } else {
            (0.0, 0.0)
        };
        let base = f * device_channels;
        for ch in 0..device_channels {
            let v = match ch {
                0 => l,
                1 => r,
                _ => 0.0,
            };
            if base + ch < device_out.len() {
                device_out[base + ch] = v;
            }
        }
    }
}

/// Session i16 LE → device interleaved i16 output.
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resample_identity() {
        let mut out = Vec::new();
        append_f32_interleaved(&mut out, &[0.5, -0.5], 2, TARGET_RATE);
        assert_eq!(out, vec![0, 64, 0, 192]);
    }
}

pub fn fill_device_i16_from_session_i16(
    device_out: &mut [i16],
    session: &[u8],
    device_channels: usize,
    session_channels: usize,
) {
    for (index, sample) in device_out.iter_mut().enumerate() {
        let frame = index / device_channels.max(1);
        let channel = index % device_channels.max(1);
        let source_channel = if session_channels == 1 { 0 } else { channel };
        let offset = (frame * session_channels + source_channel) * 2;
        *sample = if source_channel < session_channels && offset + 1 < session.len() {
            i16::from_le_bytes([session[offset], session[offset + 1]])
        } else {
            0
        };
    }
}
