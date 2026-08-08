#!/usr/bin/env bash
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> ScrcpyMac Phone Agent install"
echo "Plugin root: $PLUGIN_ROOT"

chmod +x "$PLUGIN_ROOT/bin/phone-agent" "$PLUGIN_ROOT/mcp-server.sh"
chmod +x "$PLUGIN_ROOT/scripts/"*.sh 2>/dev/null || true
chmod +x "$PLUGIN_ROOT"/bin/darwin/*/phone-agent 2>/dev/null || true

echo "==> Verifying bundled Go runtime"
"$PLUGIN_ROOT/bin/phone-agent" version

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
