#!/usr/bin/env bash
# Write MCP config snippets with absolute paths for Cursor / Claude Desktop.
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAUNCHER="$PLUGIN_ROOT/mcp-server.sh"
chmod +x "$LAUNCHER" "$PLUGIN_ROOT/bin/phone-agent"

MCP_JSON="$(cat <<EOF
{
  "mcpServers": {
    "scrcpymac-phone-agent": {
      "command": "$LAUNCHER",
      "args": []
    }
  }
}
EOF
)"

echo "==> ScrcpyMac Phone Agent — MCP configuration"
echo ""
echo "Plugin root: $PLUGIN_ROOT"
echo ""
echo "--- Cursor (~/.cursor/mcp.json) ---"
echo "$MCP_JSON"
echo ""
echo "--- Claude Desktop (macOS) ---"
echo "File: ~/Library/Application Support/Claude/claude_desktop_config.json"
echo "$MCP_JSON"
echo ""

CURSOR_DIR="$HOME/.cursor/plugins/local"
mkdir -p "$CURSOR_DIR"
if [[ -e "$CURSOR_DIR/scrcpymac-phone-agent" && ! -L "$CURSOR_DIR/scrcpymac-phone-agent" ]]; then
  echo "WARN: $CURSOR_DIR/scrcpymac-phone-agent exists and is not a symlink — skipping" >&2
else
  ln -sfn "$PLUGIN_ROOT" "$CURSOR_DIR/scrcpymac-phone-agent"
  echo "==> Linked Cursor local plugin:"
  echo "    $CURSOR_DIR/scrcpymac-phone-agent -> $PLUGIN_ROOT"
fi

CURSOR_MCP="$HOME/.cursor/mcp.json"
if [[ -f "$CURSOR_MCP" ]] && grep -q 'scrcpymac-phone-agent' "$CURSOR_MCP" 2>/dev/null; then
  echo "==> Cursor mcp.json already references scrcpymac-phone-agent"
else
  echo "==> To register MCP globally, merge the JSON above into:"
  echo "    $CURSOR_MCP"
  echo "    (Plugin MCP from Customize may work without this if using local plugin link.)"
fi

echo ""
echo "==> Codex (from repo root):"
echo "    codex plugin marketplace add $(cd "$PLUGIN_ROOT/../.." && pwd)"
echo ""
echo "Done. Reload Cursor / restart Codex, then run phone_doctor."
