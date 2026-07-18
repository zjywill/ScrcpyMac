#!/usr/bin/env bash
# MCP entrypoint with a stable path relative to the plugin root.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$ROOT/bin/phone-agent" mcp
