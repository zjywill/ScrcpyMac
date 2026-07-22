#!/usr/bin/env bash
# Drive a real MCP handshake through the phone-agent binary over stdio.
#
# Usage:
#   go/scripts/smoke-stdio.sh                 # build from source, then smoke it
#   go/scripts/smoke-stdio.sh path/to/binary  # smoke an already-built binary
#
# Point it at bin/darwin/<arch>/phone-agent after a release build to check the
# artefact that actually ships, including its codesigning and its embedded
# widget — something the in-tree Go test cannot do.
#
# The exhaustive version of this lives in Go and runs with `go test ./...`:
#   cmd/phone-agent/stdio_smoke_test.go  — full response assertions
#   internal/mcpserver/contract_test.go  — field-by-field contract comparison
# This script is the coarse, dependency-free equivalent for CI and for release
# artefacts.

set -euo pipefail

GO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_ROOT="$(cd "$GO_DIR/.." && pwd)"

BIN="${1:-}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [ -z "$BIN" ]; then
  echo "building cmd/phone-agent..."
  BIN="$WORK/phone-agent"
  (cd "$GO_DIR" && go build -o "$BIN" ./cmd/phone-agent)
fi

if [ ! -x "$BIN" ]; then
  echo "ERROR: $BIN is not an executable" >&2
  exit 1
fi

OUT="$WORK/stdout.jsonl"
ERR="$WORK/stderr.log"

# Newline-delimited JSON-RPC, which is what mcp.StdioTransport speaks.
# The trailing sleep keeps stdin open long enough for every response to be
# written; closing it immediately would race the server's own shutdown.
{
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
    '{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"phone_backend","arguments":{}}}'
  sleep 2
} | PHONE_AGENT_ROOT="$PLUGIN_ROOT" PHONE_AGENT_LOG_LEVEL=debug "$BIN" mcp >"$OUT" 2>"$ERR"

fail() {
  echo "FAIL: $*" >&2
  echo "--- stdout ---" >&2
  cat "$OUT" >&2
  echo "--- stderr ---" >&2
  cat "$ERR" >&2
  exit 1
}

# 1. stdout must be JSON-RPC and nothing else. This is the release blocker: one
#    stray log line, banner or fmt.Print corrupts the framing.
line_no=0
while IFS= read -r line; do
  line_no=$((line_no + 1))
  [ -z "$line" ] && continue
  case "$line" in
    '{"jsonrpc":"2.0"'*) ;;
    *) fail "stdout line $line_no is not a JSON-RPC message: $line" ;;
  esac
done <"$OUT"

[ "$line_no" -ge 4 ] || fail "expected at least 4 responses on stdout, got $line_no"

# 2. The handshake, the whole tool surface and the widget resource.
grep -q '"serverInfo":{"name":"scrcpymac-phone-agent"' "$OUT" ||
  grep -q '"name":"scrcpymac-phone-agent"' "$OUT" ||
  fail "initialize did not report the expected serverInfo"

for tool in open_scrcpymac scrcpymac_ui_state phone_backend phone_doctor phone_tap phone_ui_tree phone_send_wechat; do
  grep -q "\"name\":\"$tool\"" "$OUT" || fail "tools/list is missing $tool"
done

grep -q '"uri":"ui://widget/scrcpymac/app.html"' "$OUT" || fail "resources/list is missing the widget resource"
grep -q '"connectDomains":\["http://127.0.0.1:[1-9]' "$OUT" ||
  fail "the widget CSP has no concrete loopback port; the listener did not bind before the resource was published"

grep -q '"error"' "$OUT" && fail "a request returned a JSON-RPC error"

# 3. The log must exist, and must be on stderr.
[ -s "$ERR" ] || fail "stderr is empty; the startup log is missing, so check 1 proved nothing"
grep -q "phone-agent mcp starting" "$ERR" || fail "stderr does not carry the startup log"

tools=$(tr ',' '\n' <"$OUT" | grep -c '"name":"phone_\|"name":"scrcpymac_ui_\|"name":"open_scrcpymac' || true)
echo "OK: $line_no JSON-RPC messages on stdout, nothing else; $(wc -c <"$ERR" | tr -d ' ') bytes of log on stderr"
echo "OK: tool-name occurrences seen: $tools"
