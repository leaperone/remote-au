#!/usr/bin/env python3
"""Verify this application's current-user LaunchAgent restart and uninstall behavior."""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time

LABEL = "com.leaperone.remote-au.daemon"


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--repo", type=Path, required=True)
    ap.add_argument("--output", type=Path, required=True)
    ap.add_argument("--runtime", type=Path, default=Path("/Volumes/CodingSpace/remote-au-runtime"))
    ap.add_argument("--config-dir", type=Path)
    args = ap.parse_args()
    domain = f"gui/{os.getuid()}/{LABEL}"
    runtime = args.runtime.resolve()
    binary = runtime / "bin/remote-au"
    config = args.config_dir or Path.home() / "Library/Application Support/remote-au/config"
    records = []

    def run(*cmd, check=True):
        environment = os.environ.copy()
        environment.update({"REMOTE_AU_VOLUME_ROOT":str(runtime),"REMOTE_AU_BIN":str(binary),"REMOTE_AU_CONFIG_DIR":str(config),"REMOTE_AU_LOG_DIR":str(Path.home() / "Library/Logs/remote-au"),"REMOTE_AU_ROLE":"server"})
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=15, env=environment)
        if check and p.returncode:
            raise RuntimeError(f"{cmd[0]} failed: {p.stderr[-2000:]}")
        return p

    def state():
        p = run("launchctl", "print", domain, check=False)
        match = re.search(r"\bpid = (\d+)", p.stdout)
        return p.returncode, int(match[1]) if match else None

    def await_pid(old=None):
        end = time.monotonic() + 50
        while time.monotonic() < end:
            code, pid = state()
            if code == 0 and pid and pid != old:
                result = run(str(binary), "--config-dir", str(config), "status", check=False)
                if result.returncode == 0:
                    return pid, json.loads(result.stdout)
            time.sleep(.5)
        raise RuntimeError("LaunchAgent failed to become ready/restart")

    installer = args.repo / "scripts/macos/install-launchagent.sh"
    uninstaller = args.repo / "scripts/macos/uninstall-launchagent.sh"
    pairing = config / "pairing.json"
    if not pairing.exists():
        run(str(binary), "--config-dir", str(config), "pair")
    before = pairing.read_bytes()
    run("bash", str(installer))
    first, status = await_pid()
    records.append({"event": "installed", "pid": first, "status": status})
    os.kill(first, signal.SIGKILL)
    restarted, status = await_pid(first)
    records.append({"event": "crash_restarted", "old_pid": first, "new_pid": restarted, "status": status})
    run("bash", str(uninstaller))
    if state()[0] == 0:
        raise RuntimeError("LaunchAgent still registered after uninstall")
    if not pairing.exists() or pairing.read_bytes() != before:
        raise RuntimeError("uninstall changed pairing")
    records.append({"event": "uninstalled_and_pairing_preserved"})
    run("bash", str(installer))
    final, status = await_pid()
    records.append({"event": "reinstalled_and_left_running", "pid": final, "status": status})
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(records, ensure_ascii=False, indent=2))
    print(json.dumps(records, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
