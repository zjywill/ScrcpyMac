#!/usr/bin/env bash
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UI_ROOT="$PLUGIN_ROOT/ui"
EMBED_ROOT="$PLUGIN_ROOT/go/internal/widget/assets"

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

mkdir -p "$EMBED_ROOT"
cp "$UI_ROOT/dist/index.html" "$EMBED_ROOT/scrcpymac-app.html"

echo "ScrcpyMac widget built: $EMBED_ROOT/scrcpymac-app.html"
