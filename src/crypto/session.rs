use std::{fs, path::Path};

use blake2::{Blake2b512, Digest};
use chacha20poly1305::aead::{Aead, KeyInit, Payload};
use chacha20poly1305::{ChaCha20Poly1305, Key, Nonce};
use rand::RngCore;
use serde::{Deserialize, Serialize};
use subtle::ConstantTimeEq;
use thiserror::Error;

const TOKEN_BYTES: usize = 32;

#[derive(Debug, Error)]
pub enum CryptoError {
    #[error("invalid token")]
    InvalidToken,
    #[error("decrypt failed")]
    Decrypt,
    #[error("replay detected")]
    Replay,
    #[error("io: {0}")]
    Io(std::io::Error),
}

/// Group invitation. Any peer that holds it is trusted; there is no per-peer revoke list.
pub fn generate_invitation_token() -> String {
    let mut buf = [0u8; TOKEN_BYTES];
    rand::rngs::OsRng.fill_bytes(&mut buf);
    hex::encode(buf)
}

pub fn parse_invitation(token: &str) -> Result<[u8; 32], CryptoError> {
    let bytes = hex::decode(token.trim()).map_err(|_| CryptoError::InvalidToken)?;
    if bytes.len() != TOKEN_BYTES {
        return Err(CryptoError::InvalidToken);
    }
    let mut out = [0u8; 32];
    out.copy_from_slice(&bytes);
    Ok(out)
}

pub fn random_id() -> [u8; 16] {
    let mut id = [0u8; 16];
    rand::rngs::OsRng.fill_bytes(&mut id);
    id
}

fn derive32(label: &[u8], token: &[u8; 32], epoch: &[u8; 16], session: &[u8; 16]) -> [u8; 32] {
    let mut hasher = Blake2b512::new();
    hasher.update(label);
    hasher.update(token);
    hasher.update(epoch);
    hasher.update(session);
    let digest = hasher.finalize();
    let mut key = [0u8; 32];
    key.copy_from_slice(&digest[..32]);
    key
}

fn prefix(label: &[u8], key: &[u8; 32]) -> [u8; 4] {
    let mut hasher = Blake2b512::new();
    hasher.update(label);
    hasher.update(key);
    let digest = hasher.finalize();
    let mut out = [0u8; 4];
    out.copy_from_slice(&digest[..4]);
    out
}

#[derive(Clone)]
pub struct CipherDir {
    cipher: ChaCha20Poly1305,
    nonce_prefix: [u8; 4],
}

impl CipherDir {
    fn new(key: [u8; 32], label: &[u8]) -> Self {
        Self {
            cipher: ChaCha20Poly1305::new(Key::from_slice(&key)),
            nonce_prefix: prefix(label, &key),
        }
    }

    fn nonce(&self, counter: u64) -> [u8; 12] {
        let mut nonce = [0u8; 12];
        nonce[..4].copy_from_slice(&self.nonce_prefix);
        nonce[4..].copy_from_slice(&counter.to_be_bytes());
        nonce
    }

    pub fn seal(&self, counter: u64, aad: &[u8], plaintext: &[u8]) -> Result<Vec<u8>, CryptoError> {
        if counter == 0 {
            return Err(CryptoError::Decrypt);
        }
        let nonce = self.nonce(counter);
        self.cipher
            .encrypt(
                Nonce::from_slice(&nonce),
                Payload {
                    msg: plaintext,
                    aad,
                },
            )
            .map_err(|_| CryptoError::Decrypt)
    }

    pub fn open(
        &self,
        counter: u64,
        aad: &[u8],
        ciphertext: &[u8],
    ) -> Result<Vec<u8>, CryptoError> {
        if counter == 0 {
            return Err(CryptoError::Decrypt);
        }
        let nonce = self.nonce(counter);
        self.cipher
            .decrypt(
                Nonce::from_slice(&nonce),
                Payload {
                    msg: ciphertext,
                    aad,
                },
            )
            .map_err(|_| CryptoError::Decrypt)
    }
}

#[derive(Clone)]
pub struct DirectionKeys {
    pub sender_to_receiver: CipherDir,
    pub receiver_to_sender: CipherDir,
}

impl DirectionKeys {
    pub fn derive(token: &[u8; 32], epoch: &[u8; 16], session: &[u8; 16]) -> Self {
        let s2r = derive32(b"remote-au-s2r-v2", token, epoch, session);
        let r2s = derive32(b"remote-au-r2s-v2", token, epoch, session);
        Self {
            sender_to_receiver: CipherDir::new(s2r, b"remote-au-nonce-s2r-v2"),
            receiver_to_sender: CipherDir::new(r2s, b"remote-au-nonce-r2s-v2"),
        }
    }
}

pub fn discovery_key(token: &[u8; 32]) -> [u8; 32] {
    let mut hasher = Blake2b512::new();
    hasher.update(b"remote-au-discovery-v2");
    hasher.update(token);
    let digest = hasher.finalize();
    let mut key = [0u8; 32];
    key.copy_from_slice(&digest[..32]);
    key
}

pub fn mac16(key: &[u8], message: &[u8]) -> [u8; 16] {
    let mut hasher = Blake2b512::new();
    hasher.update(b"remote-au-mac-v2");
    hasher.update(&(key.len() as u32).to_be_bytes());
    hasher.update(key);
    hasher.update(&(message.len() as u64).to_be_bytes());
    hasher.update(message);
    let digest = hasher.finalize();
    let mut out = [0u8; 16];
    out.copy_from_slice(&digest[..16]);
    out
}

pub fn mac_eq(a: &[u8; 16], b: &[u8; 16]) -> bool {
    a.ct_eq(b).unwrap_u8() == 1
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PairingConfig {
    pub invitation_token: String,
    pub receiver_name: Option<String>,
}

#[derive(Debug, Clone)]
pub struct PairingStore {
    path: std::path::PathBuf,
}

impl PairingStore {
    pub fn new(dir: &Path) -> Self {
        Self {
            path: dir.join("pairing.json"),
        }
    }

    pub fn load(&self) -> Result<Option<PairingConfig>, CryptoError> {
        if !self.path.exists() {
            return Ok(None);
        }
        let data = fs::read_to_string(&self.path).map_err(CryptoError::Io)?;
        let cfg: PairingConfig =
            serde_json::from_str(&data).map_err(|_| CryptoError::InvalidToken)?;
        Ok(Some(cfg))
    }

    pub fn save(&self, cfg: &PairingConfig) -> Result<(), CryptoError> {
        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent).map_err(CryptoError::Io)?;
        }
        let data = serde_json::to_string_pretty(cfg).map_err(|_| CryptoError::InvalidToken)?;
        fs::write(&self.path, data).map_err(CryptoError::Io)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&self.path, fs::Permissions::from_mode(0o600))
                .map_err(CryptoError::Io)?;
        }
        Ok(())
    }
}
