#!/usr/bin/env bash
# Create and update the plugin-local Python runtime without modifying system Python.
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_DIR="$PLUGIN_ROOT/server"
VENV_DIR="$PLUGIN_ROOT/.venv"
LOCK_DIR="$PLUGIN_ROOT/.runtime-bootstrap.lock"
MARKER="$VENV_DIR/.phone-agent-runtime"
PYPROJECT="$SERVER_DIR/pyproject.toml"
BASE_PYTHON="${PHONE_AGENT_BOOTSTRAP_PYTHON:-${PHONE_AGENT_PYTHON:-python3}}"
RUNTIME_SCHEMA="1"

log() {
  printf '%s\n' "$*" >&2
}

if ! command -v "$BASE_PYTHON" >/dev/null 2>&1; then
  log "ERROR: Python 3.10+ is required (not found: $BASE_PYTHON)"
  exit 1
fi
BASE_PYTHON="$(command -v "$BASE_PYTHON")"

if ! "$BASE_PYTHON" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 10) else 1)'; then
  log "ERROR: Python 3.10+ is required"
  exit 1
fi

pyproject_hash() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$PYPROJECT" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$PYPROJECT" | awk '{print $1}'
  else
    "$BASE_PYTHON" - "$PYPROJECT" <<'PY'
import hashlib
import pathlib
import sys

print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())
PY
  fi
}

EXPECTED_MARKER="$RUNTIME_SCHEMA:$(pyproject_hash)"

runtime_ready() {
  local installed_marker=""

  [[ -x "$VENV_DIR/bin/python" && -f "$MARKER" ]] || return 1
  installed_marker="$(cat "$MARKER" 2>/dev/null || true)"
  [[ "$installed_marker" == "$EXPECTED_MARKER" ]] || return 1
  "$VENV_DIR/bin/python" -c 'import mcp; import phone_agent' >/dev/null 2>&1
}

if runtime_ready; then
  exit 0
fi

attempts=0
while ! mkdir "$LOCK_DIR" 2>/dev/null; do
  if runtime_ready; then
    exit 0
  fi

  owner_pid="$(cat "$LOCK_DIR/pid" 2>/dev/null || true)"
  if [[ -n "$owner_pid" && ! "$owner_pid" =~ ^[0-9]+$ ]]; then
    rm -rf "$LOCK_DIR"
    continue
  fi
  if [[ -n "$owner_pid" ]] && ! kill -0 "$owner_pid" 2>/dev/null; then
    rm -rf "$LOCK_DIR"
    continue
  fi

  attempts=$((attempts + 1))
  if [[ -z "$owner_pid" && "$attempts" -ge 20 ]]; then
    rm -rf "$LOCK_DIR"
    attempts=0
    continue
  fi
  if [[ "$attempts" -ge 240 ]]; then
    log "ERROR: timed out waiting for Phone Agent runtime setup"
    exit 1
  fi
  sleep 0.25
done

cleanup_lock() {
  rm -rf "$LOCK_DIR"
}
trap cleanup_lock EXIT
printf '%s\n' "$$" > "$LOCK_DIR/pid"

if runtime_ready; then
  exit 0
fi

rm -rf "$VENV_DIR"
log "==> Creating Phone Agent runtime at $VENV_DIR"

if command -v uv >/dev/null 2>&1; then
  uv venv --python "$BASE_PYTHON" "$VENV_DIR" >&2
  uv pip install --python "$VENV_DIR/bin/python" -e "$SERVER_DIR" >&2
else
  "$BASE_PYTHON" -m venv "$VENV_DIR" >&2
  "$VENV_DIR/bin/python" -m pip install --disable-pip-version-check -e "$SERVER_DIR" >&2
fi

if ! "$VENV_DIR/bin/python" -c 'import mcp; import phone_agent' >/dev/null 2>&1; then
  log "ERROR: Phone Agent runtime verification failed"
  exit 1
fi

printf '%s\n' "$EXPECTED_MARKER" > "$MARKER.tmp"
mv "$MARKER.tmp" "$MARKER"
log "==> Phone Agent runtime ready"
