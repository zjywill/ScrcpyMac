#!/usr/bin/env bash
# Build the Go phone-agent binaries into the plugin's bin/ layout.
#
# Produces:
#   bin/darwin/arm64/phone-agent
#   bin/darwin/x86_64/phone-agent
#
# The bash launcher bin/phone-agent executes the matching binary directly.
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_DIR="$PLUGIN_ROOT/go"

# Pinned so local builds, CI and release artifacts all agree. GOTOOLCHAIN=auto
# (the default) downloads this toolchain on demand, so a newer or older `go` on
# PATH still produces an identical build.
GO_TOOLCHAIN="${PHONE_AGENT_GO_TOOLCHAIN:-go1.26.5}"

ARCHS="${1:-all}"

if [[ ! -f "$GO_DIR/go.mod" ]]; then
  echo "ERROR: $GO_DIR/go.mod not found." >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "ERROR: no 'go' on PATH. Install Go (any 1.21+ will bootstrap $GO_TOOLCHAIN)." >&2
  exit 1
fi

export GOTOOLCHAIN="${GOTOOLCHAIN:-$GO_TOOLCHAIN}"

version="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
  "$PLUGIN_ROOT/.codex-plugin/plugin.json" | head -1)"
version="${version:-0.0.0-dev}"
commit="$(git -C "$PLUGIN_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"

# The widget is embedded into the binary, so always refresh it first.
"$PLUGIN_ROOT/scripts/build-ui.sh"
embed_target="$GO_DIR/internal/widget/assets/scrcpymac-app.html"
if [[ ! -s "$embed_target" ]]; then
  echo "ERROR: widget build did not produce $embed_target" >&2
  exit 1
fi

build_one() {
  local goarch="$1" outarch="$2" out
  out="$PLUGIN_ROOT/bin/darwin/$outarch/phone-agent"
  mkdir -p "$(dirname "$out")"
  echo "==> building darwin/$goarch -> bin/darwin/$outarch/phone-agent"
  ( cd "$GO_DIR" && \
    CGO_ENABLED=0 GOOS=darwin GOARCH="$goarch" \
    go build -trimpath \
      -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
      -o "$out" ./cmd/phone-agent )
  chmod +x "$out"
}

case "$ARCHS" in
  arm64)  build_one arm64 arm64 ;;
  x86_64|amd64) build_one amd64 x86_64 ;;
  all)    build_one arm64 arm64; build_one amd64 x86_64 ;;
  *) echo "Usage: build-go.sh [all|arm64|x86_64]" >&2; exit 1 ;;
esac

echo ""
echo "==> built with $(cd "$GO_DIR" && go version)"
for f in "$PLUGIN_ROOT"/bin/darwin/*/phone-agent; do
  [[ -f "$f" ]] || continue
  printf '    %s (%s)\n' "${f#$PLUGIN_ROOT/}" "$(file -b "$f" | cut -d, -f1-2)"
done
echo ""
echo "Try it: $PLUGIN_ROOT/bin/phone-agent doctor"
