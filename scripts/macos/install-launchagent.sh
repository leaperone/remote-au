#!/usr/bin/env bash
# macOS per-user LaunchAgent for remote-au server (Rust CLI). No system-wide config.
set -euo pipefail

APP_NAME="remote-au"
LABEL="com.leaperone.${APP_NAME}.daemon"
MANAGED_KEY="RemoteAuManaged"
DEFAULT_VOLUME_ROOT="/Volumes/CodingSpace/remote-au-runtime"
VOLUME_ROOT="${REMOTE_AU_VOLUME_ROOT:-$DEFAULT_VOLUME_ROOT}"
BIN="${REMOTE_AU_BIN:-${VOLUME_ROOT}/bin/remote-au}"
CONFIG_DIR="${REMOTE_AU_CONFIG_DIR:-${VOLUME_ROOT}/config}"
LOG_DIR="${REMOTE_AU_LOG_DIR:-${VOLUME_ROOT}/logs}"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
ROTATE_LOGS="${ROTATE_LOGS:-0}"
ROLE="${REMOTE_AU_ROLE:-server}"
SOURCE="${REMOTE_AU_SOURCE:-mic}"
[[ "$ROLE" == "server" || "$ROLE" == "send" ]] || { echo "invalid REMOTE_AU_ROLE" >&2; exit 1; }

die() { echo "error: $*" >&2; exit 1; }

mkdir -p "$(dirname "$PLIST")" "$CONFIG_DIR" "$LOG_DIR"
chmod 700 "$CONFIG_DIR" "$LOG_DIR" 2>/dev/null || true

if [[ ! -x "$BIN" ]]; then
  die "executable not found or not executable: $BIN (set REMOTE_AU_BIN)"
fi

rotate_if_large() {
  local f="$1"
  [[ -f "$f" ]] || return 0
  local sz
  sz=$(stat -f%z "$f" 2>/dev/null || echo 0)
  if [[ "$sz" -gt 5242880 ]]; then
    mv -f "$f" "${f}.$(date +%Y%m%d-%H%M%S)"
  fi
}

if [[ "$ROTATE_LOGS" == "1" ]]; then
  rotate_if_large "${LOG_DIR}/server.stdout.log"
  rotate_if_large "${LOG_DIR}/server.stderr.log"
fi

STDOUT_LOG="${LOG_DIR}/server.stdout.log"
STDERR_LOG="${LOG_DIR}/server.stderr.log"

plist_label_from_file() {
  local f="$1"
  /usr/libexec/PlistBuddy -c 'Print :Label' "$f" 2>/dev/null || true
}

plist_managed_from_file() {
  local f="$1"
  /usr/libexec/PlistBuddy -c "Print :${MANAGED_KEY}" "$f" 2>/dev/null || true
}

if [[ -f "$PLIST" ]]; then
  existing_label="$(plist_label_from_file "$PLIST")"
  [[ "$existing_label" == "$LABEL" ]] || die "refusing to overwrite a foreign or invalid LaunchAgent"
  [[ "$(plist_managed_from_file "$PLIST")" == "true" ]] || die "refusing to replace an unmanaged LaunchAgent"
  cp -p "$PLIST" "${PLIST}.previous"
  if launchctl list 2>/dev/null | grep -q "${LABEL}"; then
    launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
  fi
fi

export REMOTE_AU_PLIST_OUT="$PLIST"
export REMOTE_AU_LABEL="$LABEL"
export REMOTE_AU_BIN_PATH="$BIN"
export REMOTE_AU_CONFIG_PATH="$CONFIG_DIR"
export REMOTE_AU_STDOUT_PATH="$STDOUT_LOG"
export REMOTE_AU_STDERR_PATH="$STDERR_LOG"
export REMOTE_AU_ROLE="$ROLE"
export REMOTE_AU_SOURCE="$SOURCE"
export REMOTE_AU_MANAGED_KEY="$MANAGED_KEY"

python3 - <<'PY'
import os
import plistlib

out = os.environ["REMOTE_AU_PLIST_OUT"]
plist = {
    "Label": os.environ["REMOTE_AU_LABEL"],
    "RemoteAuManaged": True,
    "ProgramArguments": [
        os.environ["REMOTE_AU_BIN_PATH"],
        "--config-dir",
        os.environ["REMOTE_AU_CONFIG_PATH"],
        os.environ["REMOTE_AU_ROLE"],
        "--log-level",
        "info",
    ],
    "RunAtLoad": True,
    "KeepAlive": True,
    "ThrottleInterval": 30,
    "StandardOutPath": os.environ["REMOTE_AU_STDOUT_PATH"],
    "StandardErrorPath": os.environ["REMOTE_AU_STDERR_PATH"],
    "EnvironmentVariables": {
        "PATH": "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
    },
}
if os.environ["REMOTE_AU_ROLE"] == "send":
    plist["ProgramArguments"] += ["--source", os.environ["REMOTE_AU_SOURCE"]]
with open(out, "wb") as f:
    plistlib.dump(plist, f)
PY

chmod 644 "$PLIST"

launchctl bootstrap "gui/$(id -u)" "$PLIST"
launchctl enable "gui/$(id -u)/${LABEL}"
launchctl kickstart -k "gui/$(id -u)/${LABEL}" || true

echo "Installed LaunchAgent ${LABEL}"
echo "  binary: ${BIN}"
echo "  config: ${CONFIG_DIR} (chmod 700; kept on uninstall unless PURGE=1)"
echo "  logs:   ${LOG_DIR}"
echo "Pairing is required on the server; run remote-au pair there before senders connect (no auto-pair in this script)."
