#!/usr/bin/env bash
# Download Google platform-tools adb into the plugin bundle.
# macOS: bin/darwin/adb (universal binary from Google)
# Linux: bin/linux/x86_64/adb (for dev/CI)
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLATFORM="${1:-$(uname -s | tr '[:upper:]' '[:lower:]')}"

case "$PLATFORM" in
  darwin|macos|mac)
    ZIP_URL="https://dl.google.com/android/repository/platform-tools-latest-darwin.zip"
    DEST="$PLUGIN_ROOT/bin/darwin/adb"
  ;;
  linux)
    ZIP_URL="https://dl.google.com/android/repository/platform-tools-latest-linux.zip"
    DEST="$PLUGIN_ROOT/bin/linux/x86_64/adb"
  ;;
  *)
    echo "Unsupported platform: $PLATFORM (use darwin or linux)" >&2
    exit 1
  ;;
esac

if [[ -x "$DEST" ]]; then
  echo "adb already present at $DEST"
  "$DEST" version | head -1
  exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> Downloading platform-tools ($PLATFORM)"
curl -fsSL "$ZIP_URL" -o "$TMP/platform-tools.zip"

echo "==> Extracting adb"
unzip -q "$TMP/platform-tools.zip" -d "$TMP"
mkdir -p "$(dirname "$DEST")"
cp "$TMP/platform-tools/adb" "$DEST"
chmod +x "$DEST"

# Mirror universal mac adb to arch-specific paths for launcher compatibility.
if [[ "$PLATFORM" == "darwin" || "$PLATFORM" == "macos" || "$PLATFORM" == "mac" ]]; then
  mkdir -p "$PLUGIN_ROOT/bin/darwin/arm64" "$PLUGIN_ROOT/bin/darwin/x86_64"
  cp "$DEST" "$PLUGIN_ROOT/bin/darwin/arm64/adb"
  cp "$DEST" "$PLUGIN_ROOT/bin/darwin/x86_64/adb"
  chmod +x "$PLUGIN_ROOT/bin/darwin/arm64/adb" "$PLUGIN_ROOT/bin/darwin/x86_64/adb"
fi

echo "==> Installed: $DEST"
"$DEST" version | head -1
