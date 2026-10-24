mod codec;
mod format;
mod wire;

pub use codec::{
    codec_name, decode_canonical, encode_canonical, CODEC_ADPCM24, CODEC_ADPCM48, CODEC_PCM,
};
pub use format::{
    split_pcm, AudioFormat, ProtocolError, MAX_DATAGRAM, MAX_PCM_CHUNK, MAX_STREAMS, STREAM_IDLE,
    WIRE_CHANNELS, WIRE_FRAME_SAMPLES, WIRE_PACE, WIRE_RATE,
};
pub use wire::{
    assemble, clip_name, decode_audio, decode_feedback, decode_hello, decode_probe, encode_audio,
    encode_feedback, encode_hello, encode_probe, parse_packet, AudioBody, FeedbackBody, HeaderView,
    ProbeBody, FEEDBACK_BODY_LEN, HEADER_LEN, KIND_AUDIO, KIND_FEEDBACK, KIND_HELLO, KIND_PROBE,
};
