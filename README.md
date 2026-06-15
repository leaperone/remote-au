# remote-au

**N→1 LAN audio aggregation, from the command line.**

Run `remote-au send` on several machines and `remote-au recv` on one. Each sender
captures its microphone or system audio and streams it over the local network; the
receiver mixes every stream together and plays the result through its speakers.

It is a small, dependency-light reimplementation of the core idea behind
[AudioRelay](https://audiorelay.net/) — *without* the hardest part (a kernel
virtual-microphone driver), because the goal here is simply to **play the combined
audio out of one machine's speakers**, not to expose it as a virtual mic.

```
   Windows  ──capture──┐
   (loopback)          │
                       ▼
   phone / laptop ──►  UDP / TCP  ──►  receiver ──► mix ──► speakers
   (mic)               (LAN)           (any OS)
                       ▲
   another box ────────┘
```

- **Roles, not platforms.** `recv` and `send` both run on Windows, Linux and macOS.
  Any machine can be the receiver; any machine can be a sender.
- **Zero-config discovery.** `send` finds the receiver on the LAN automatically —
  no IP addresses to type.
- **Low latency by default.** Audio travels over UDP (connectionless, no
  head-of-line blocking); TCP is available as a reliable fallback.

---

## Features

- **N→1 aggregation** — many senders → one receiver that mixes and plays back.
- **Cross-platform** — Windows / Linux / macOS, sender *and* receiver.
- **Auto-discovery** — UDP broadcast; `send` with no `--to` connects to the
  receiver it finds on the LAN.
- **UDP (default) or TCP** — `--transport udp|tcp`. UDP for low latency, TCP for
  reliability / restrictive networks.
- **Resilient** — UDP is connectionless: restart the receiver and senders keep
  emitting and the stream resumes on its own.
- **Per-stream jitter buffer** — priming, bounded depth, drop-oldest, loss
  concealment with silence; `--verbose` prints per-stream queue / discarded /
  underrun / latency stats.
- **Bounded & defensive** — every network length is validated before allocation;
  capped concurrent streams; rate-limited discovery replies; idle streams expire.

## Install

From npm (prebuilt binary for your platform is fetched automatically):

```sh
npm install -g remote-au
remote-au --help
```

Supported targets: `darwin-arm64`, `darwin-x64`, `linux-x64`, `linux-arm64`,
`win32-x64`. The `remote-au` package is a thin launcher; npm installs the matching
`remote-au-<platform>-<arch>` binary package via optional dependencies.

### Build from source

Requirements:

- **Go 1.24+**
- **A C toolchain** (the audio backend is [miniaudio](https://miniaud.io/) via
  [`gen2brain/malgo`](https://github.com/gen2brain/malgo), which needs cgo):
  - **macOS** — Xcode Command Line Tools (`xcode-select --install`).
  - **Linux** — GCC/Clang and ALSA/PulseAudio/PipeWire dev headers.
  - **Windows** — MinGW-w64 (`gcc`). MSVC is not supported by cgo.

```sh
git clone https://github.com/leaperone/remote-au.git
cd remote-au
CGO_ENABLED=1 go build -o remote-au ./cmd/remote-au
```

Cross-compiling needs a target C compiler and `CGO_ENABLED=1`; building natively on
each platform is the simplest path.

## Quick start

On the machine that should play the audio (the receiver):

```sh
remote-au recv
```

On each machine that should send audio:

```sh
remote-au send                    # capture the microphone, auto-find the receiver
remote-au send --source loopback  # capture system audio (Windows: native WASAPI)
```

That's it — no IP addresses. Start two senders and the receiver mixes them.

If discovery is blocked on your network (some corporate VLANs isolate broadcast),
point the sender at the receiver directly:

```sh
remote-au send --to 192.168.1.20:47000
```

---

## Commands

### `remote-au devices`
List the local playback and capture devices with their indices, so you can pass
`--device <index>` to `recv`/`send`. Also notes per-platform loopback support.

### `remote-au selftest`
Capture the local microphone and play it straight back through the local speakers —
a quick check that the audio backend works on this machine.

### `remote-au recv` — the aggregator
Listens for senders (TCP **and** UDP on the same port), mixes all active streams,
and plays the result.

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `:47000` | Audio listen address (TCP + UDP). |
| `--device` | auto | Playback device index (see `devices`). |
| `--discovery-port` | `47001` | UDP port the discovery responder listens on. |
| `--name` | hostname | Name advertised to senders during discovery. |
| `--no-discovery` | off | Disable the LAN discovery responder. |

### `remote-au send` — a capture source
Captures audio and streams it to a receiver.

| Flag | Default | Description |
|------|---------|-------------|
| `--to` | *(discover)* | Receiver address, e.g. `192.168.1.20:47000`. Skips discovery. |
| `--peer` | *(none)* | Require a discovered receiver with this name (safe disambiguator). |
| `--source` | `mic` | Capture source: `mic` or `loopback` (system audio). |
| `--transport` | `udp` | Audio transport: `udp` (low latency) or `tcp` (reliable). |
| `--device` | auto | Capture device index (see `devices`). |
| `--discover-timeout` | `1.5s` | How long to wait for discovery replies. |
| `--discovery-port` | `47001` | UDP discovery port (must match the receiver). |
| `--name` | hostname | Name this sender announces. |

### Global flags

Apply to any subcommand, placed **before** it (e.g. `remote-au --verbose recv`):

| Flag | Default | Description |
|------|---------|-------------|
| `--rate` | `48000` | Sample rate (Hz). |
| `--channels` | `2` | Channel count. |
| `--frame-ms` | `10` | PCM packet duration (ms). |
| `--verbose` | off | Verbose audio-backend + per-stream stats logging. |

All endpoints must agree on `rate` / `channels`; the receiver rejects senders whose
format does not match.

---

## How discovery works

- The **receiver** runs a UDP responder. When it receives a discovery query it
  replies (unicast) with its name, a stable per-run instance ID, and the audio
  port.
- The **sender**, when given no `--to`, broadcasts a query and collects replies for
  `--discover-timeout`:
  - **one** receiver found → connects automatically (and prints which one);
  - **several** found → it lists them and asks you to pick with `--to` or `--peer`;
  - **none** found → it tells you to use `--to`.

**Trust model:** discovery trusts the local network — a single discovered receiver
is connected to without a prompt. On an untrusted LAN, pin the target with `--to`
or `--peer`. (Cryptographic peer authentication is intentionally out of scope.)

## Transports

| | UDP (default) | TCP (`--transport tcp`) |
|---|---|---|
| Latency | Lower — no retransmit stalls, no head-of-line blocking | Slightly higher under loss |
| Packet loss | Dropped; concealed with silence (jitter buffer) | Retransmitted (can stall) |
| Connection | Connectionless — survives receiver restarts | Connection-oriented |
| When to use | Default; LAN audio | Lossy / firewalled networks needing reliability |

The receiver always listens on both, so senders can choose freely.

## Platform notes

`recv` and `send` both work on all three desktop OSes. The only platform-specific
concern is **capturing *system* audio** on the sender:

| OS | Microphone | System audio (`--source loopback`) |
|----|------------|------------------------------------|
| **Windows** | ✅ | ✅ native WASAPI loopback |
| **Linux** | ✅ | Select a PulseAudio/PipeWire **monitor** source as the capture device |
| **macOS** | ✅ | Needs a virtual device such as [BlackHole](https://github.com/ExistentialAudio/BlackHole) |

Playback (what the receiver does) works everywhere with no extra setup.

## Audio format

Fixed for now: **48 kHz, 16-bit signed little-endian, 2 channels**, 10 ms packets
(480 frames). Override with the global `--rate` / `--channels` / `--frame-ms` flags
(all endpoints must match).

## Protocol (summary)

- **TCP audio** — magic `RAU1`: a handshake (format + name) then length-prefixed
  audio frames (big-endian header, `seq`, `captureFrame`, payload). Stream framed.
- **UDP audio** — magic `RAUU`: self-contained datagrams, `HELLO` (handshake) sent
  periodically + `AUDIO` (seq / captureFrame / PCM). Payload capped to **960 bytes**
  (5 ms) to stay under the Ethernet MTU and avoid IP fragmentation.
- **Discovery** — magic `RAUD`: bounded `query` / `announce` datagrams.

All length fields are validated and capped before any allocation.

## Project layout

```
cmd/remote-au/          CLI entry point and subcommand wiring
internal/audio/         miniaudio (malgo) capture / playback / device enumeration
internal/protocol/      TCP stream framing + UDP datagram codec
internal/transport/     sender + receiver (TCP accept loop & UDP receive loop)
internal/mixer/         per-stream jitter buffer + N-stream mix engine
internal/discovery/     UDP query/announce responder + finder
internal/stats/         per-stream / aggregate statistics
```

The mix engine runs inside the playback device callback (the audio clock): it pulls
exactly the requested number of frames from each stream's jitter buffer, sums them
with per-source gain and a saturating limiter, and is strictly non-blocking and
allocation-free in steady state.

## Limitations / roadmap

- **Raw PCM** on the wire (~1.5 Mbps per stream). Opus compression is a natural next
  step for weak links and many simultaneous streams.
- No per-stream **volume / mute** controls yet.
- **Mobile** senders (Android / iOS) are not covered — that's a separate, larger
  effort.
- macOS system-audio capture needs a virtual device (see platform notes).

## Releasing (maintainers)

Releases are automated by [`.github/workflows/release.yml`](.github/workflows/release.yml).

One-time setup: add an npm **automation token** as the repository secret
`NPM_TOKEN` (npm → Access Tokens → Generate → Automation).

Then cut a release by pushing a semver tag:

```sh
git tag v0.1.0
git push origin v0.1.0
```

CI builds the cgo binary on each platform's native runner, assembles the
per-platform npm packages (`remote-au-<platform>-<arch>`), publishes them and the
main `remote-au` package to npm, and attaches the raw binaries to a GitHub Release.

## License

[MIT](LICENSE) © leaperone
