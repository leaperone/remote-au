# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.0] - 2026-10-24

### Added

- Rust rewrite of the LAN audio daemon (macOS receiver / Windows sender).
- Resident discovery and pairing workflow for multi-sender aggregation.
- Adaptive bitrate and receive-buffer tuning from link quality signals.
- Autostart helpers and reconnect behavior when senders go offline.
- `remote-au --version`, `remote-au devices --json`, leveled stderr diagnostics,
  and device selection by index.

### Changed

- Diagnostics use stderr; interactive status and JSON remain on stdout.
- Release builds embed the Cargo package version in the CLI.
- Native Rust release archives replace the Go/npm tag publishing workflow.
  The project remains licensed under AGPL-3.0-or-later.

### Verified (manual)

- Windows system audio capture through to Mac hardware playback on a user-tested
  setup (2026-10-24).

### Not verified in this release

- Windows login-triggered scheduled-task autostart and reboot recovery paths were
  not re-tested for v0.4.0.

[0.4.0]: https://github.com/leaperone/remote-au/releases/tag/v0.4.0
