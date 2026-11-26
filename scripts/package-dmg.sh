#!/usr/bin/env bash
# Build ScrcpyMac in Release (ad-hoc signed) and wrap it in a DMG with a
# drag-to-Applications layout. Output lands at dist/ScrcpyMac-<version>.dmg.
#
# Recipients will hit Gatekeeper ("unidentified developer") the first time
# because we are not signed with a Developer ID. Two workarounds — pick one
# and tell users:
#   1. Right-click the app → Open → Open (only the first time)
#   2. Terminal:  xattr -cr /Applications/ScrcpyMac.app

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

APP_NAME="ScrcpyMac"
SCHEME="ScrcpyMac"
CONFIG="Release"
PROJECT="ScrcpyMac.xcodeproj"

VERSION="$(awk -F'["[:space:]]+' '/^[[:space:]]*MARKETING_VERSION:/ {print $3; exit}' project.yml)"
if [[ -z "${VERSION:-}" ]]; then
  echo "could not read MARKETING_VERSION from project.yml" >&2
  exit 1
fi

BUILD_DIR="$ROOT/build/dmg"
DIST_DIR="$ROOT/dist"
STAGING="$BUILD_DIR/staging"
DMG_PATH="$DIST_DIR/${APP_NAME}-${VERSION}.dmg"

rm -rf "$BUILD_DIR"
mkdir -p "$STAGING" "$DIST_DIR"

echo "==> xcodebuild $SCHEME ($CONFIG)"
xcodebuild \
  -project "$PROJECT" \
  -scheme "$SCHEME" \
  -configuration "$CONFIG" \
  -derivedDataPath "$BUILD_DIR/DerivedData" \
  -destination "generic/platform=macOS" \
  ARCHS="arm64 x86_64" \
  ONLY_ACTIVE_ARCH=NO \
  CODE_SIGN_IDENTITY="-" \
  CODE_SIGNING_REQUIRED=NO \
  CODE_SIGNING_ALLOWED=NO \
  clean build >/dev/null

APP_SRC="$BUILD_DIR/DerivedData/Build/Products/$CONFIG/${APP_NAME}.app"
if [[ ! -d "$APP_SRC" ]]; then
  echo "build succeeded but no .app at $APP_SRC" >&2
  exit 1
fi

echo "==> bundling toolchain (adb + scrcpy-server) into .app"
# Recipients won't have adb or scrcpy installed. Ship both inside the bundle so
# Toolchain.detect() picks the `bundled` branch and nothing has to be on PATH.
ADB_CANDIDATES=(
  "$HOME/Library/Android/sdk/platform-tools/adb"
  "/opt/homebrew/bin/adb"
  "/usr/local/bin/adb"
)
SERVER_CANDIDATES=(
  "/opt/homebrew/share/scrcpy/scrcpy-server"
  "/usr/local/share/scrcpy/scrcpy-server"
)
ADB_SRC=""; for c in "${ADB_CANDIDATES[@]}"; do [[ -x "$c" ]] && ADB_SRC="$c" && break; done
# The client speaks one exact server protocol version (ScrcpyProtocol.version);
# a mismatched jar exits immediately ("server version does not match") and the
# app only sees "Short read from scrcpy-server". Homebrew upgrades scrcpy on
# its own schedule, so verify the jar instead of trusting whatever is installed.
# Override with SCRCPY_SERVER=/path/to/scrcpy-server-vX.Y.Z.
PROTOCOL_VERSION="$(sed -n 's/.*static let version = "\(.*\)".*/\1/p' ScrcpyMac/ScrcpyProtocol.swift)"
server_matches() {
  # The version lives in classes.dex either as its own constant (3.x) or
  # inlined into the mismatch error message (4.x, R8-optimized).
  # No `grep -q`: it exits early, SIGPIPEs `strings`, and pipefail then fails.
  local v="${PROTOCOL_VERSION//./\\.}"
  unzip -p "$1" classes.dex 2>/dev/null | strings \
    | grep -E -e "^${v}\$" -e "server version \(${v}\)" >/dev/null
}
if [[ -n "${SCRCPY_SERVER:-}" ]]; then
  SERVER_CANDIDATES=("$SCRCPY_SERVER")
fi
SERVER_SRC=""
for c in "${SERVER_CANDIDATES[@]}"; do
  [[ -f "$c" ]] || continue
  if server_matches "$c"; then SERVER_SRC="$c"; break; fi
  echo "    skipping $c (not scrcpy-server $PROTOCOL_VERSION)" >&2
done
if [[ -z "$ADB_SRC" ]]; then
  echo "could not find adb — install platform-tools or scrcpy first" >&2
  exit 1
fi
if [[ -z "$SERVER_SRC" ]]; then
  echo "could not find scrcpy-server $PROTOCOL_VERSION — download scrcpy-server-v$PROTOCOL_VERSION" >&2
  echo "from https://github.com/Genymobile/scrcpy/releases and pass SCRCPY_SERVER=/path/to/it" >&2
  exit 1
fi

RES="$APP_SRC/Contents/Resources"
mkdir -p "$RES/bin" "$RES/share"
cp "$ADB_SRC" "$RES/bin/adb"
cp "$SERVER_SRC" "$RES/share/scrcpy-server"
chmod +x "$RES/bin/adb"

echo "    adb:    $ADB_SRC"
echo "    server: $SERVER_SRC"

# Re-sign ad-hoc after modifying bundle contents — otherwise the signature
# covers only the old (empty) Resources and macOS will refuse to launch it.
codesign --force --deep --sign - "$APP_SRC"

echo "==> staging DMG contents"
cp -R "$APP_SRC" "$STAGING/"
ln -s /Applications "$STAGING/Applications"
# Pre-strip quarantine: has no effect once a browser re-adds it on download,
# but helps when the DMG is moved via AirDrop / USB / SCP.
xattr -cr "$STAGING/${APP_NAME}.app"

echo "==> hdiutil create"
rm -f "$DMG_PATH"
hdiutil create \
  -volname "${APP_NAME} ${VERSION}" \
  -srcfolder "$STAGING" \
  -format UDZO \
  -fs HFS+ \
  -ov \
  "$DMG_PATH" >/dev/null

echo
echo "==> done"
echo "    $DMG_PATH"
du -h "$DMG_PATH" | awk '{print "    size: " $1}'
