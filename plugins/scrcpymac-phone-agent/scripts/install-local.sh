#!/usr/bin/env bash
# Install this working tree into the local Codex plugin cache so it can be
# tried immediately, without a marketplace round-trip.
#
# The Codex marketplace "scrcpymac" is configured as source_type = "local"
# pointing at this repo (see ~/.codex/config.toml), and Codex COPIES the plugin
# into ~/.codex/plugins/cache/scrcpymac/scrcpymac-phone-agent/<version>/.
# That copy is what actually runs — editing the repo alone changes nothing.
#
#   ./scripts/install-local.sh              # hot-sync into the installed version dir
#   ./scripts/install-local.sh --bump 0.8.0 # publish a new version for Codex to install
#   ./scripts/install-local.sh --dry-run
#
# After a hot-sync, restart Codex (or toggle the plugin off/on) so the MCP
# server processes reload — the old ones keep running the old code.
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$PLUGIN_ROOT/../.." && pwd)"
MARKETPLACE_JSON="$REPO_ROOT/.agents/plugins/marketplace.json"
CACHE_ROOT="$HOME/.codex/plugins/cache/scrcpymac/scrcpymac-phone-agent"

DRY_RUN=0
BUMP_TO=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --bump) BUMP_TO="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

read_version() {
  sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    "$PLUGIN_ROOT/.codex-plugin/plugin.json" | head -1
}

CURRENT="$(read_version)"
[[ -n "$CURRENT" ]] || { echo "ERROR: could not read version from .codex-plugin/plugin.json" >&2; exit 1; }

# ---------------------------------------------------------------- version bump
if [[ -n "$BUMP_TO" ]]; then
  echo "==> bumping $CURRENT -> $BUMP_TO"
  files=(
    "$PLUGIN_ROOT/.codex-plugin/plugin.json"
    "$PLUGIN_ROOT/.cursor-plugin/plugin.json"
    "$MARKETPLACE_JSON"
  )
  for f in "${files[@]}"; do
    [[ -f "$f" ]] || { echo "    skip (missing): ${f#$REPO_ROOT/}"; continue; }
    if [[ "$DRY_RUN" == "1" ]]; then
      echo "    would rewrite \"$CURRENT\" -> \"$BUMP_TO\" in ${f#$REPO_ROOT/}"
    else
      # Only touch exact version strings, never other fields that might match.
      perl -pi -e "s/\"\Q$CURRENT\E\"/\"$BUMP_TO\"/g" "$f"
      echo "    updated ${f#$REPO_ROOT/}"
    fi
  done
  [[ "$DRY_RUN" == "1" ]] || CURRENT="$BUMP_TO"
  echo ""
  echo "Now install it from Codex: the 'scrcpymac' marketplace reads this repo"
  echo "directly, so reinstall/upgrade the plugin and it will pick up $CURRENT."
  echo "(Re-run this script without --bump to also hot-sync the files.)"
  echo ""
fi

TARGET="$CACHE_ROOT/$CURRENT"

# ---------------------------------------------------------------- preflight
echo "==> plugin version : $CURRENT"
echo "==> source         : ${PLUGIN_ROOT#$REPO_ROOT/}"
echo "==> target         : $TARGET"

if [[ ! -d "$TARGET" ]]; then
  echo ""
  echo "Version $CURRENT is not installed in the Codex cache yet."
  echo "Install it once from the Codex plugin UI (marketplace: scrcpymac), then"
  echo "re-run this script to hot-sync subsequent edits."
  echo ""
  echo "Installed versions found:"
  ls -1 "$CACHE_ROOT" 2>/dev/null | sed 's/^/    /' || echo "    (none)"
  exit 1
fi

echo "==> backend        : go (bundled native binary)"
echo ""

# ---------------------------------------------------------------- sync
# Generated dependencies and the Go source tree never belong in a plugin install.
# --delete-excluded also removes stale Python runtimes from older installations.
RSYNC_ARGS=(
  -a --delete --delete-excluded
  --exclude '.venv/'
  --exclude '__pycache__/'
  --exclude '*.pyc'
  --exclude 'ui/node_modules/'
  --exclude 'ui/dist/'
  --exclude 'go/'
  --exclude '.git/'
  --exclude '.DS_Store'
)
[[ "$DRY_RUN" == "1" ]] && RSYNC_ARGS+=(--dry-run -v)

rsync "${RSYNC_ARGS[@]}" "$PLUGIN_ROOT/" "$TARGET/"

if [[ "$DRY_RUN" == "1" ]]; then
  echo ""
  echo "(dry run — nothing written)"
  exit 0
fi

chmod +x "$TARGET/bin/phone-agent" "$TARGET/mcp-server.sh" 2>/dev/null || true
chmod +x "$TARGET/scripts/"*.sh 2>/dev/null || true
chmod +x "$TARGET"/bin/darwin/*/phone-agent 2>/dev/null || true

echo ""
echo "==> synced. Verifying the installed copy:"
if "$TARGET/bin/phone-agent" doctor >/tmp/phone-agent-doctor.json 2>/tmp/phone-agent-doctor.err; then
  head -30 /tmp/phone-agent-doctor.json
else
  echo "    doctor exited non-zero:"
  tail -10 /tmp/phone-agent-doctor.err | sed 's/^/    /'
fi

running="$(
  (pgrep -f 'bin/darwin/.*/phone-agent' 2>/dev/null || true) |
    wc -l |
    tr -d '[:space:]'
)"
echo ""
echo "==> ${running} MCP server process(es) still running the OLD code."
echo "    Restart Codex (or toggle the plugin off/on) to pick this up."
