# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog, and this project adheres to semantic
versioning once public releases begin.

## [Unreleased]

### Added

- `remote-au version` and `remote-au --version`.
- `remote-au devices --json` for machine-readable device lists.
- Leveled stderr diagnostics with `--log-level` and `--log-format`.
- Device selection by index or case-insensitive name substring.
- Windows coverage in CI.
- Contributor guide, issue templates, PR template, and troubleshooting docs.

### Changed

- Diagnostics now use stderr while interactive status and JSON data remain on
  stdout.
- Release builds inject the tag version into the CLI binary.
- Project license changed from MIT to AGPL-3.0-or-later.
