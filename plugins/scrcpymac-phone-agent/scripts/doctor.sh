#!/usr/bin/env bash
set -euo pipefail
PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PHONE_AGENT_ROOT="$PLUGIN_ROOT"
export PYTHONPATH="$PLUGIN_ROOT/server${PYTHONPATH:+:$PYTHONPATH}"
exec "$PLUGIN_ROOT/bin/phone-agent" doctor
