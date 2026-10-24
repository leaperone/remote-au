use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::{
    fs::{self, OpenOptions},
    io::Write,
    path::{Path, PathBuf},
};

#[derive(Clone)]
pub struct ConfigPaths {
    pub root: PathBuf,
}

impl ConfigPaths {
    pub fn resolve(explicit: Option<PathBuf>) -> Result<Self> {
        let root = explicit
            .or_else(|| std::env::var_os("REMOTE_AU_CONFIG_DIR").map(PathBuf::from))
            .unwrap_or_else(default_config_dir);
        fs::create_dir_all(&root)
            .with_context(|| format!("create config dir {}", root.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&root, fs::Permissions::from_mode(0o700))?;
        }
        Ok(Self { root })
    }
    pub fn control_socket(&self) -> PathBuf {
        self.root.join("control.json")
    }
    pub fn state_file(&self) -> PathBuf {
        self.root.join("daemon.json")
    }
    pub fn preferences(&self) -> Result<Preferences> {
        let p = self.root.join("preferences.json");
        if !p.exists() {
            return Ok(Preferences::default());
        }
        Ok(serde_json::from_slice(&fs::read(p)?)?)
    }
    pub fn save_preferences(&self, prefs: &Preferences) -> Result<()> {
        private_write(
            &self.root.join("preferences.json"),
            &serde_json::to_vec_pretty(prefs)?,
        )?;
        Ok(())
    }
}

#[derive(Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Preferences {
    pub server: Option<String>,
    pub output_device: Option<usize>,
    pub capture_device: Option<usize>,
    pub master_gain: Option<f32>,
}

pub fn private_write(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let tmp = path.with_extension(format!(
        "tmp-{}-{:016x}",
        std::process::id(),
        rand::random::<u64>()
    ));
    let result = (|| {
        let mut f = options.open(&tmp)?;
        f.write_all(bytes)?;
        f.sync_all()?;
        #[cfg(windows)]
        if path.exists() {
            fs::remove_file(path)?;
        }
        fs::rename(&tmp, path)
    })();
    if result.is_err() {
        let _ = fs::remove_file(tmp);
    }
    result
}

fn default_config_dir() -> PathBuf {
    #[cfg(windows)]
    {
        return std::env::var_os("LOCALAPPDATA")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from("."))
            .join("remote-au")
            .join("config");
    }
    #[cfg(target_os = "macos")]
    {
        return std::env::var_os("HOME")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from("."))
            .join("Library/Application Support/remote-au/config");
    }
    #[cfg(all(not(windows), not(target_os = "macos")))]
    {
        std::env::var_os("HOME")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from("."))
            .join(".config/remote-au")
    }
}

pub fn audio_format_from_globals(
    rate: u32,
    channels: u8,
    frame_ms: u32,
) -> crate::protocol::AudioFormat {
    let frame_samples = ((rate as u64 * frame_ms as u64) / 1000).clamp(1, u16::MAX as u64) as u16;
    crate::protocol::AudioFormat {
        sample_rate: rate,
        channels,
        frame_samples,
    }
}

/// Held by the daemon for its lifetime; the OS releases the lock after a crash.
pub struct DaemonGuard {
    _file: std::fs::File,
}

impl ConfigPaths {
    pub fn acquire_daemon_lock(&self) -> std::io::Result<DaemonGuard> {
        let mut options = OpenOptions::new();
        options.read(true).write(true).create(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        #[cfg(windows)]
        {
            use std::os::windows::fs::OpenOptionsExt;
            options.share_mode(0);
        }
        let file = options.open(self.root.join("daemon.lock"))?;
        #[cfg(unix)]
        {
            use std::os::fd::AsRawFd;
            extern "C" {
                fn flock(
                    fd: std::os::raw::c_int,
                    operation: std::os::raw::c_int,
                ) -> std::os::raw::c_int;
            }
            // flock holds no Rust references; the owned file descriptor remains live in the guard.
            if unsafe { flock(file.as_raw_fd(), 2 | 4) } != 0 {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::AddrInUse,
                    "another daemon owns this configuration directory",
                ));
            }
        }
        Ok(DaemonGuard { _file: file })
    }
}
