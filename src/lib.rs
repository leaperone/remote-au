pub mod audio;
pub mod cli;
pub mod config;
pub mod control;
pub mod crypto;
pub mod daemon;
pub mod discovery;
pub mod mixer;
pub mod protocol;
pub mod transport;

pub const VERSION: &str = env!("CARGO_PKG_VERSION");
