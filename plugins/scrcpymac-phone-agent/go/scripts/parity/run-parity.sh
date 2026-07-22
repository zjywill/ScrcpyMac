#!/usr/bin/env bash
# Run the Python-vs-Go parity harness against the attached device.
#
#   go/scripts/parity/run-parity.sh                       # build, then compare everything
#   go/scripts/parity/run-parity.sh --cases doctor,devices
#   PARITY_GO_BIN=bin/darwin/arm64/phone-agent go/scripts/parity/run-parity.sh
#
# Requires: a device in `adb devices` state `device`, and a Python that can
# import the plugin's own phone_agent package — normally the plugin's .venv,
# which scripts/ensure-runtime.sh creates. Nothing here writes to server/.
#
# Exit status: 0 parity, 1 diffs found, 2 harness or preflight failure.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_DIR="$(cd "$HERE/../.." && pwd)"
PLUGIN_ROOT="$(cd "$GO_DIR/.." && pwd)"

# 1. The Go binary under test. Built fresh unless one is handed in, so a stale
#    artefact can never be mistaken for a passing port.
GO_BIN="${PARITY_GO_BIN:-}"
if [ -z "$GO_BIN" ]; then
  GO_BIN="$HERE/out/phone-agent"
  mkdir -p "$HERE/out"
  echo "==> building $GO_BIN"
  (cd "$GO_DIR" && go build -o "$GO_BIN" ./cmd/phone-agent)
fi
if [ ! -x "$GO_BIN" ]; then
  echo "ERROR: $GO_BIN is not executable" >&2
  exit 2
fi

# 2. A Python that resolves phone_agent to THIS checkout. parity.py re-checks
#    and refuses to run if it resolved somewhere else.
PY="${PARITY_PYTHON:-}"
if [ -z "$PY" ]; then
  for candidate in "$PLUGIN_ROOT/.venv/bin/python" "${PHONE_AGENT_PYTHON:-}" python3; do
    [ -n "$candidate" ] || continue
    if command -v "$candidate" >/dev/null 2>&1 &&
       PYTHONPATH="$PLUGIN_ROOT/server" "$candidate" -c 'import phone_agent, mcp' >/dev/null 2>&1; then
      PY="$candidate"
      break
    fi
  done
fi
if [ -z "$PY" ]; then
  echo "ERROR: no Python found that can import both phone_agent and mcp." >&2
  echo "       Run scripts/ensure-runtime.sh, or set PARITY_PYTHON." >&2
  exit 2
fi

echo "==> python: $PY"
echo "==> go:     $GO_BIN"

export PYTHONPATH="$PLUGIN_ROOT/server${PYTHONPATH:+:$PYTHONPATH}"
export PHONE_AGENT_ROOT="$PLUGIN_ROOT"

exec "$PY" "$HERE/parity.py" --go-bin "$GO_BIN" "$@"
