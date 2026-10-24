#!/usr/bin/env python3
"""Run paired daemons through a reproducible UDP impairment proxy. No drivers needed."""
import argparse
import contextlib
import heapq
import json
import os
from pathlib import Path
import random
import select
import signal
import socket
import subprocess
import tempfile
import threading
import time


class Link:
    """A bounded bidirectional UDP link; no privileged network configuration."""
    def __init__(self, upstream, seed=42):
        self.front = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.front.bind(("127.0.0.1", 0))
        self.back = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.back.bind(("127.0.0.1", 0))
        self.upstream = upstream
        self.client = None
        self.delay = self.jitter = self.loss = 0
        self.capacity_bps = 0
        self.next_free = {"up":0.0,"down":0.0}
        self.running = True
        self.rng = random.Random(seed)
        self.count = {"forwarded": 0, "dropped": 0}
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    @property
    def address(self):
        return "%s:%d" % self.front.getsockname()

    def run(self):
        pending = []
        ordinal = 0
        while self.running:
            ready, _, _ = select.select([self.front, self.back], [], [], .002)
            for sock in ready:
                data, peer = sock.recvfrom(65535)
                if sock is self.front:
                    self.client = peer
                    dest_sock, dest = self.back, self.upstream
                    direction = "up"
                else:
                    if peer != self.upstream or self.client is None:
                        continue
                    dest_sock, dest = self.front, self.client
                    direction = "down"
                if self.rng.random() < self.loss or len(pending) >= 4096:
                    self.count["dropped"] += 1
                    continue
                ordinal += 1
                due = time.monotonic() + max(0, self.delay + self.rng.uniform(-self.jitter, self.jitter))
                if self.capacity_bps:
                    due = max(due, self.next_free[direction]) + len(data) * 8 / self.capacity_bps
                    if due - time.monotonic() > .15:
                        self.count["dropped"] += 1
                        continue
                    self.next_free[direction] = due
                heapq.heappush(pending, (due, ordinal, dest_sock, dest, data))
            while pending and pending[0][0] <= time.monotonic():
                _, _, sock, peer, data = heapq.heappop(pending)
                sock.sendto(data, peer)
                self.count["forwarded"] += 1
        self.front.close()
        self.back.close()

    def close(self):
        self.running = False
        self.thread.join(2)


def free_port():
    with contextlib.closing(socket.socket(socket.AF_INET, socket.SOCK_DGRAM)) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--binary", type=Path, required=True)
    ap.add_argument("--output", type=Path, required=True)
    ap.add_argument("--playback", action="store_true", help="Use the current speaker at low master gain")
    ap.add_argument("--stage-seconds", type=float, default=15)
    args = ap.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="acceptance-", dir=args.output))
    os.chmod(work, 0o700)
    binary = str(args.binary.resolve())
    procs, logs, events, links = [], [], [], []
    start = time.monotonic()

    def record(event, **values):
        events.append({"t_seconds": round(time.monotonic() - start, 3), "event": event, **values})
        print(event, flush=True)

    def cli(config, *cmd):
        p = subprocess.run([binary, "--config-dir", str(config), *cmd], capture_output=True, text=True, timeout=8)
        if p.returncode:
            raise RuntimeError(f"CLI {cmd[0]} failed: {p.stderr[-1000:]}")
        return p.stdout

    def spawn(config, role, *extra):
        f = open(work / f"{role}-{len(procs)}.log", "w")
        logs.append(f)
        p = subprocess.Popen([binary, "--config-dir", str(config), role, *extra], stdout=f, stderr=f, start_new_session=True)
        procs.append(p)
        return p

    def status(config):
        return json.loads(cli(config, "status"))

    def wait_status(config, process):
        until = time.monotonic() + 15
        while time.monotonic() < until:
            if process.poll() is not None:
                raise RuntimeError("daemon exited before control became ready")
            try:
                return status(config)
            except (RuntimeError, json.JSONDecodeError):
                time.sleep(.2)
        raise RuntimeError("daemon status did not become ready")

    def stop(p):
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGTERM)
            try:
                p.wait(timeout=4)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait()

    def sample(stage, receiver, sender):
        end = time.monotonic() + args.stage_seconds
        initial = status(receiver).get("playback", {}).get("nonzero_samples", 0)
        last = None
        while time.monotonic() < end:
            time.sleep(min(2, max(0, end - time.monotonic())))
            last = status(receiver)
            record(stage, receiver=last, sender=status(sender))
        if last and last.get("playback", {}).get("nonzero_samples",0) <= initial + 1000:
            raise RuntimeError(f"{stage}: synthetic audio never reached the playback callback")
        if last and last.get("callbacks",0) > last.get("playback",{}).get("callbacks",0) + 2:
            raise RuntimeError(f"{stage}: mixer is being pulled by more than one playback clock")
        if not last or last.get("packets_received", 0) <= 0:
            raise RuntimeError(f"{stage}: receiver accepted no audio")
        if last.get("callbacks", 0) <= 0:
            raise RuntimeError(f"{stage}: playback did not consume audio")
        if not any(stream.get("packets", 0) > 0 for stream in last.get("streams", [])):
            raise RuntimeError(f"{stage}: no active source with audio")

    try:
        receiver, sender, second = (work / n for n in ("receiver", "sender", "second"))
        cli(receiver, "pair")
        invitation = work / "invitation.json"
        cli(receiver, "pair", "--export-file", str(invitation))
        cli(sender, "pair", "--import-file", str(invitation))
        cli(second, "pair", "--import-file", str(invitation))
        invitation.unlink()
        port = free_port()
        server_args = ["--addr", f"127.0.0.1:{port}", "--no-discovery"]
        if not args.playback:
            server_args.append("--null-playback")
        r = spawn(receiver, "server", *server_args)
        record("receiver_ready", status=wait_status(receiver, r))
        cli(receiver, "volume", "--master", "0.04")
        link = Link(("127.0.0.1", port))
        links.append(link)
        s = spawn(sender, "send", "--to", link.address, "--source", "synth", "--name", "acceptance-synth")
        wait_status(sender, s)
        sample("clean", receiver, sender)
        link.delay, link.jitter, link.loss = .018, .012, .03
        sample("impaired_18ms_12msjitter_3pctloss", receiver, sender)
        link.delay = link.jitter = link.loss = 0
        link.capacity_bps = 350000
        sample("bandwidth_350kbps", receiver, sender)
        link.capacity_bps = 0
        # Allow the next periodic bandwidth probe to observe the restored link.
        sample("recovered", receiver, sender)
        sample("recovered_next_probe", receiver, sender)
        sample("recovered_final_probe", receiver, sender)
        sample("recovered_steady", receiver, sender)
        s2 = spawn(second, "send", "--to", f"127.0.0.1:{port}", "--source", "synth", "--name", "acceptance-second")
        wait_status(second, s2)
        time.sleep(3)
        mixed = status(receiver)
        if len([s for s in mixed.get("streams", []) if s.get("packets",0)>0]) < 2:
            raise RuntimeError("two sources were not admitted together")
        record("two_sources", receiver=mixed, second=status(second))
        stop(s2)
        stop(r)
        time.sleep(1)
        r = spawn(receiver, "server", *server_args)
        wait_status(receiver, r)
        sample("receiver_restarted_sender_unchanged", receiver, sender)
        stop(s)
        time.sleep(12)
        waiting = status(receiver)
        if waiting.get("streams"):
            raise RuntimeError("offline sources did not expire")
        record("sender_offline_waiting", receiver=waiting)
        with contextlib.closing(socket.socket(socket.AF_INET, socket.SOCK_DGRAM)) as bad:
            for length in (0, 4, 32, 1200, 1500):
                bad.sendto(bytes([42]) * length, ("127.0.0.1", port))
        time.sleep(.3)
        record("unauthenticated_garbage_rejected", receiver=status(receiver))
        record("completed", link_counts=link.count, playback=args.playback, work_dir=str(work))
    except Exception as e:
        record("failed", error=str(e))
        raise
    finally:
        for p in reversed(procs):
            stop(p)
        for link in links:
            link.close()
        for f in logs:
            f.close()
        (args.output / "measurements.json").write_text(json.dumps(events, indent=2, ensure_ascii=False))
        for p in work.rglob("pairing.json"):
            p.unlink()


if __name__ == "__main__":
    main()
