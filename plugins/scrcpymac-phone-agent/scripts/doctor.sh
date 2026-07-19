#!/usr/bin/env bash
set -euo pipefail
# bin/phone-agent already sets PHONE_AGENT_ROOT/PYTHONPATH/ADB_PATH itself.
PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "$PLUGIN_ROOT/bin/phone-agent" doctor
