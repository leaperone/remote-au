# Contributing

Thanks for working on remote-au. This project is a cgo-based Go CLI, so please
verify changes on the platform you touch when possible.

## Build from Source

Requirements:

- Go 1.24+
- `CGO_ENABLED=1`
- A native C toolchain for the audio backend:
  - macOS: Xcode Command Line Tools (`xcode-select --install`)
  - Linux: GCC or Clang plus ALSA/PulseAudio/PipeWire development headers
  - Windows: MinGW-w64 (`gcc`); MSVC is not supported by Go cgo here

```sh
CGO_ENABLED=1 go build ./...
CGO_ENABLED=1 go test -race ./...
```

## Pull Requests

- Keep changes focused and describe user-visible behavior.
- Add or update tests for protocol, discovery, transport, CLI parsing, and JSON
  output changes.
- Keep stdout machine-readable for data-producing commands; diagnostics belong on
  stderr.
- Do not add third-party dependencies without a clear need.

## Commits

Use short, imperative commit subjects, for example:

```text
Add JSON device listing
Move diagnostics to stderr
```

## License

By contributing, you agree that your contributions are licensed under
AGPL-3.0-or-later, the same license as the project.
