#!/usr/bin/env bash
set -euo pipefail

APP_NAME="remote-au"
LABEL="com.leaperone.${APP_NAME}.daemon"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
DEFAULT_VOLUME_ROOT="/Volumes/CodingSpace/remote-au-runtime"
VOLUME_ROOT="${REMOTE_AU_VOLUME_ROOT:-$DEFAULT_VOLUME_ROOT}"

die() { echo "error: $*" >&2; exit 1; }

purge_owned_runtime() {
  [[ -z "${REMOTE_AU_BIN:-}" && -z "${REMOTE_AU_CONFIG_DIR:-}" && -z "${REMOTE_AU_LOG_DIR:-}" ]] || die "purge refuses custom binary/config/log paths"
  python3 - <<'PYTHON'
from pathlib import Path
import shutil
root = Path('/Volumes/CodingSpace/remote-au-runtime')
if root.is_symlink():
    raise SystemExit('refusing to purge a symlink runtime root')
owned_dirs = [root/'config', root/'logs', Path.home()/'Library/Application Support/remote-au/config', Path.home()/'Library/Logs/remote-au']
for path in owned_dirs:
    if path.is_symlink():
        raise SystemExit('refusing to purge symlink: ' + str(path))
for path in owned_dirs:
    if path.exists():
        shutil.rmtree(path)
binary = root / 'bin/remote-au'
binary.unlink(missing_ok=True)
for path in (root / 'bin', root):
    try:
        path.rmdir()
    except OSError:
        pass
PYTHON
}

if [[ -f "$PLIST" ]]; then
  label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$PLIST" 2>/dev/null || true)"
  if [[ "$label" != "$LABEL" ]]; then
    die "refusing to remove plist with foreign Label=$label"
  fi
  managed="$(/usr/libexec/PlistBuddy -c 'Print :RemoteAuManaged' "$PLIST" 2>/dev/null || true)"
  [[ "$managed" == "true" ]] || die "refusing to remove an unmanaged plist"
  launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
  rm -f "$PLIST"
  echo "Removed LaunchAgent ${LABEL}"
else
  echo "LaunchAgent plist not found (already removed)."
fi

if [[ "${PURGE:-0}" == "1" ]]; then
  if [[ -n "${REMOTE_AU_VOLUME_ROOT:-}" && "$REMOTE_AU_VOLUME_ROOT" != "$DEFAULT_VOLUME_ROOT" ]]; then
    die "PURGE=1 with custom REMOTE_AU_VOLUME_ROOT is refused; delete owned paths manually"
  fi
  purge_owned_runtime "$VOLUME_ROOT"
  echo "Purged owned runtime files under ${VOLUME_ROOT} (bin/config/logs)."
else
  echo "Kept binary under ${VOLUME_ROOT}, config under ~/Library/Application Support/remote-au, and logs under ~/Library/Logs/remote-au. PURGE=1 deletes the default owned paths."
fi
