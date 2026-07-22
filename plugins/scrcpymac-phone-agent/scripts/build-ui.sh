#!/usr/bin/env bash
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UI_ROOT="$PLUGIN_ROOT/ui"
STATIC_ROOT="$PLUGIN_ROOT/server/phone_agent/static"

cleanup() {
  if [[ -d "$UI_ROOT/node_modules" ]]; then
    find "$UI_ROOT/node_modules" -depth -delete
  fi
  if [[ -d "$UI_ROOT/dist" ]]; then
    find "$UI_ROOT/dist" -depth -delete
  fi
}
trap cleanup EXIT

cd "$UI_ROOT"
npm ci
npm run check
npm run build

mkdir -p "$STATIC_ROOT"
cp "$UI_ROOT/dist/index.html" "$STATIC_ROOT/scrcpymac-app.html"

echo "ScrcpyMac widget built: $STATIC_ROOT/scrcpymac-app.html"
