use std::net::{IpAddr, Ipv4Addr, SocketAddr};

use crate::crypto::{discovery_key, mac16, mac_eq, parse_invitation, random_id};

pub const DISCOVERY_MAGIC: [u8; 4] = *b"RAUD";
pub const DISCOVERY_VERSION: u8 = 2;
pub const INSTANCE_ID_LEN: usize = 16;
pub const CHALLENGE_LEN: usize = 16;
pub const MAC_LEN: usize = 16;
pub const HEADER_LEN: usize = 4 + 1 + 1 + CHALLENGE_LEN + MAC_LEN;
pub const MAX_PACKET_BYTES: usize = 512;

pub const DEFAULT_DISCOVERY_PORTS: [u16; 3] = [47001, 48001, 49001];

const TYPE_QUERY: u8 = 1;
const TYPE_ANNOUNCE: u8 = 2;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Announce {
    pub tcp_port: u16,
    pub instance_id: [u8; INSTANCE_ID_LEN],
    pub advertised_addr: Ipv4Addr,
    pub name: String,
    pub epoch: [u8; 16],
}

#[derive(Debug)]
pub struct SignedQuery {
    pub bytes: Vec<u8>,
    pub challenge: [u8; CHALLENGE_LEN],
}

pub fn directed_query_targets(extra: &[Ipv4Addr]) -> Vec<SocketAddr> {
    let mut hosts = Vec::with_capacity(extra.len() + 4);
    hosts.extend_from_slice(extra);
    hosts.push(Ipv4Addr::new(192, 168, 2, 255));
    hosts.push(Ipv4Addr::new(192, 168, 1, 255));
    hosts.push(Ipv4Addr::BROADCAST);
    hosts.push(Ipv4Addr::LOCALHOST);
    let mut out = Vec::with_capacity(hosts.len() * DEFAULT_DISCOVERY_PORTS.len());
    for host in hosts {
        for port in DEFAULT_DISCOVERY_PORTS {
            out.push(SocketAddr::new(IpAddr::V4(host), port));
        }
    }
    out
}

/// Audio contact uses the packet source, never a loopback address stuffed into the announce.
pub fn contact_addr(announce: &Announce, source: SocketAddr) -> SocketAddr {
    SocketAddr::new(source.ip(), announce.tcp_port)
}

pub fn local_lan_v4() -> Ipv4Addr {
    let Ok(sock) = std::net::UdpSocket::bind((Ipv4Addr::UNSPECIFIED, 0)) else {
        return Ipv4Addr::UNSPECIFIED;
    };
    for probe in ["192.168.2.255:9", "192.168.1.1:9", "8.8.8.8:9"] {
        if sock.connect(probe).is_ok() {
            if let Ok(addr) = sock.local_addr() {
                if let IpAddr::V4(v4) = addr.ip() {
                    if !v4.is_loopback() && !v4.is_unspecified() {
                        return v4;
                    }
                }
            }
        }
    }
    Ipv4Addr::UNSPECIFIED
}

pub fn encode_query(name: &str, token: &str) -> Result<SignedQuery, String> {
    let key = discovery_key(&parse_invitation(token).map_err(|e| e.to_string())?);
    let name = clip(name);
    let mut challenge = [0u8; CHALLENGE_LEN];
    rand::rngs::OsRng.fill_bytes(&mut challenge);
    let mut body = Vec::with_capacity(1 + name.len());
    body.push(u8::try_from(name.len()).map_err(|_| "discovery name too long")?);
    body.extend_from_slice(name.as_bytes());
    let bytes = seal(TYPE_QUERY, &challenge, &body, &key)?;
    Ok(SignedQuery { bytes, challenge })
}

pub fn encode_announce(
    announce: &Announce,
    challenge: &[u8; CHALLENGE_LEN],
    token: &str,
) -> Result<Vec<u8>, String> {
    let key = discovery_key(&parse_invitation(token).map_err(|e| e.to_string())?);
    if announce.tcp_port == 0 {
        return Err("discovery announce missing audio port".into());
    }
    let name = clip(&announce.name);
    let mut body = Vec::with_capacity(2 + 16 + 16 + 4 + 1 + name.len());
    body.extend_from_slice(&announce.tcp_port.to_be_bytes());
    body.extend_from_slice(&announce.instance_id);
    body.extend_from_slice(&announce.epoch);
    body.extend_from_slice(&announce.advertised_addr.octets());
    body.push(u8::try_from(name.len()).map_err(|_| "discovery name too long")?);
    body.extend_from_slice(name.as_bytes());
    seal(TYPE_ANNOUNCE, challenge, &body, &key)
}

pub fn respond_to_query(packet: &[u8], token: &str, template: &Announce) -> Option<Vec<u8>> {
    let (challenge, name) = open_query(packet, token)?;
    if !name.is_empty() && name != template.name {
        return None;
    }
    encode_announce(template, &challenge, token).ok()
}

pub fn open_announce(
    packet: &[u8],
    token: &str,
    challenge: &[u8; CHALLENGE_LEN],
) -> Option<Announce> {
    let (kind, got, body) = open(packet, token)?;
    if kind != TYPE_ANNOUNCE || got != *challenge || body.len() < 2 + 16 + 16 + 4 + 1 {
        return None;
    }
    let port = u16::from_be_bytes(body.get(..2)?.try_into().ok()?);
    if port == 0 {
        return None;
    }
    let mut instance_id = [0u8; 16];
    instance_id.copy_from_slice(body.get(2..18)?);
    let mut epoch = [0u8; 16];
    epoch.copy_from_slice(body.get(18..34)?);
    let advertised_addr = Ipv4Addr::new(body[34], body[35], body[36], body[37]);
    let name_len = usize::from(*body.get(38)?);
    if body.len() != 39 + name_len {
        return None;
    }
    let name = std::str::from_utf8(body.get(39..39 + name_len)?)
        .ok()?
        .to_string();
    Some(Announce {
        tcp_port: port,
        instance_id,
        advertised_addr,
        name,
        epoch,
    })
}

pub fn dedup_prefer_direct(items: Vec<(Announce, SocketAddr)>) -> Vec<(Announce, SocketAddr)> {
    let mut best: Vec<(Announce, SocketAddr)> = Vec::new();
    for item in items {
        if let Some(existing) = best
            .iter_mut()
            .find(|(ann, _)| ann.instance_id == item.0.instance_id)
        {
            if source_rank(item.1) < source_rank(existing.1) {
                *existing = item;
            }
        } else {
            best.push(item);
        }
    }
    best.sort_by_key(|(_, src)| source_rank(*src));
    best
}

pub fn source_rank(addr: SocketAddr) -> u32 {
    match addr.ip() {
        IpAddr::V4(v4) => {
            let o = v4.octets();
            if v4.is_loopback() {
                4
            } else if v4.is_unspecified() || v4.is_broadcast() {
                3
            } else if o[0] == 192 && o[1] == 168 && o[2] == 2 {
                0
            } else if v4.is_private() {
                1
            } else {
                2
            }
        }
        IpAddr::V6(v6) if v6.is_loopback() => 4,
        IpAddr::V6(_) => 3,
    }
}

pub fn fresh_instance() -> [u8; 16] {
    random_id()
}

fn open_query(packet: &[u8], token: &str) -> Option<([u8; CHALLENGE_LEN], String)> {
    let (kind, challenge, body) = open(packet, token)?;
    if kind != TYPE_QUERY || body.is_empty() {
        return None;
    }
    let name_len = usize::from(body[0]);
    if body.len() != 1 + name_len {
        return None;
    }
    let name = std::str::from_utf8(body.get(1..)?).ok()?.to_string();
    Some((challenge, name))
}

fn open(packet: &[u8], token: &str) -> Option<(u8, [u8; CHALLENGE_LEN], Vec<u8>)> {
    if packet.len() < HEADER_LEN || packet.len() > MAX_PACKET_BYTES {
        return None;
    }
    if packet.get(..4)? != DISCOVERY_MAGIC.as_slice() || packet.get(4)? != &DISCOVERY_VERSION {
        return None;
    }
    let kind = *packet.get(5)?;
    let mut challenge = [0u8; CHALLENGE_LEN];
    challenge.copy_from_slice(packet.get(6..22)?);
    let mut mac = [0u8; MAC_LEN];
    mac.copy_from_slice(packet.get(22..38)?);
    let body = packet.get(HEADER_LEN..)?.to_vec();
    let key = discovery_key(&parse_invitation(token).ok()?);
    let expect = mac16(&key, &signed_bytes(kind, &challenge, &body));
    if !mac_eq(&mac, &expect) {
        return None;
    }
    Some((kind, challenge, body))
}

fn seal(
    kind: u8,
    challenge: &[u8; CHALLENGE_LEN],
    body: &[u8],
    key: &[u8; 32],
) -> Result<Vec<u8>, String> {
    if HEADER_LEN + body.len() > MAX_PACKET_BYTES {
        return Err("discovery packet too large".into());
    }
    let mut out = vec![0u8; HEADER_LEN + body.len()];
    out[..4].copy_from_slice(&DISCOVERY_MAGIC);
    out[4] = DISCOVERY_VERSION;
    out[5] = kind;
    out[6..22].copy_from_slice(challenge);
    out[HEADER_LEN..].copy_from_slice(body);
    let mac = mac16(key, &signed_bytes(kind, challenge, body));
    out[22..38].copy_from_slice(&mac);
    Ok(out)
}

fn signed_bytes(kind: u8, challenge: &[u8], body: &[u8]) -> Vec<u8> {
    let mut msg = Vec::with_capacity(6 + challenge.len() + body.len());
    msg.extend_from_slice(&DISCOVERY_MAGIC);
    msg.push(DISCOVERY_VERSION);
    msg.push(kind);
    msg.extend_from_slice(challenge);
    msg.extend_from_slice(body);
    msg
}

fn clip(name: &str) -> String {
    crate::protocol::clip_name(name)
}

use rand::RngCore;
