#!/usr/bin/env bash
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> ScrcpyMac Phone Agent install"
echo "Plugin root: $PLUGIN_ROOT"

if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 is required (3.10+)" >&2
  exit 1
fi

if [[ "$(python3 -c 'import sys; print(sys.version_info >= (3, 10))')" != "True" ]]; then
  echo "ERROR: Python 3.10+ required" >&2
  exit 1
fi

chmod +x "$PLUGIN_ROOT/bin/phone-agent" "$PLUGIN_ROOT/mcp-server.sh"
chmod +x "$PLUGIN_ROOT/scripts/"*.sh 2>/dev/null || true

echo "==> Installing server dependencies"
"$PLUGIN_ROOT/scripts/ensure-runtime.sh"

UNAME="$(uname -s)"
if [[ "$UNAME" == "Darwin" ]]; then
  echo "==> Downloading bundled adb for macOS (if missing)"
  "$PLUGIN_ROOT/scripts/download-adb.sh" darwin || true
elif [[ "$UNAME" == "Linux" ]]; then
  echo "==> Downloading bundled adb for Linux dev (if missing)"
  "$PLUGIN_ROOT/scripts/download-adb.sh" linux || true
fi

echo ""
"$PLUGIN_ROOT/scripts/configure.sh"

echo ""
echo "==> Running doctor"
"$PLUGIN_ROOT/bin/phone-agent" doctor || true
