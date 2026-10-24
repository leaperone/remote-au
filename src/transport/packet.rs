use crate::crypto::CipherDir;
use crate::protocol::{assemble, parse_packet, HEADER_LEN, MAX_DATAGRAM};

pub struct Opened {
    pub kind: u8,
    pub epoch: [u8; 16],
    pub session: [u8; 16],
    pub counter: u64,
    pub body: Vec<u8>,
}

pub fn seal_packet(
    dir: &CipherDir,
    epoch: &[u8; 16],
    session: &[u8; 16],
    counter: u64,
    kind: u8,
    body: &[u8],
) -> Option<Vec<u8>> {
    if counter == 0 || HEADER_LEN + body.len() + 16 > MAX_DATAGRAM {
        return None;
    }
    let mut header = [0u8; HEADER_LEN];
    header[..4].copy_from_slice(b"RAU2");
    header[4] = 2;
    header[5] = kind;
    header[6..22].copy_from_slice(epoch);
    header[22..38].copy_from_slice(session);
    header[38..46].copy_from_slice(&counter.to_be_bytes());
    let ct = dir.seal(counter, &header, body).ok()?;
    assemble(kind, epoch, session, counter, &ct)
}

pub fn open_packet(dir: &CipherDir, packet: &[u8]) -> Option<Opened> {
    let view = parse_packet(packet)?;
    let body = dir.open(view.counter, view.aad, view.ciphertext).ok()?;
    Some(Opened {
        kind: view.kind,
        epoch: view.epoch,
        session: view.session,
        counter: view.counter,
        body,
    })
}
