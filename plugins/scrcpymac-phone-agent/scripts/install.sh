#!/usr/bin/env bash
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_DIR="$PLUGIN_ROOT/server"

echo "==> ScrcpyMac Phone Agent install"
echo "Plugin root: $PLUGIN_ROOT"

if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 is required (3.10+)" >&2
  exit 1
fi

PY_MINOR="$(python3 -c 'import sys; print(sys.version_info.minor)')"
if [[ "$(python3 -c 'import sys; print(sys.version_info >= (3, 10))')" != "True" ]]; then
  echo "ERROR: Python 3.10+ required" >&2
  exit 1
fi

chmod +x "$PLUGIN_ROOT/bin/phone-agent"
chmod +x "$PLUGIN_ROOT/scripts/"*.sh 2>/dev/null || true

if command -v uv >/dev/null 2>&1; then
  echo "==> Installing server dependencies with uv"
  (cd "$SERVER_DIR" && uv pip install --system -e .)
else
  echo "==> Installing server dependencies with pip"
  python3 -m pip install --user -e "$SERVER_DIR"
fi

echo ""
echo "==> Running doctor"
"$PLUGIN_ROOT/bin/phone-agent" doctor || true

echo ""
echo "==> Local test (Cursor)"
echo "Symlink for local plugin testing:"
echo "  ln -sf \"$PLUGIN_ROOT\" ~/.cursor/plugins/local/scrcpymac-phone-agent"
echo ""
echo "==> Local test (Codex)"
echo "  codex plugin marketplace add $(cd "$PLUGIN_ROOT/../.." && pwd)"
echo ""
echo "Done."
