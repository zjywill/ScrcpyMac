#!/usr/bin/env bash
# Refresh selftest.py's fixtures from the last real run's artefacts.
#
# The fixtures are REAL device output, captured by run-parity.sh into out/, not
# hand-written JSON — so selftest.py perturbs the same shapes the port actually
# has to produce. Only the screenshot base64 is trimmed, because 280 KB of
# image in a fixture buys nothing.
#
# Run this after a run whose cases all passed; committing a fixture captured
# from a failing run would bake the failure in as the expected shape.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${1:-$HERE/out}"
FIX="$HERE/fixtures"

for f in devices screenshot_no_image tap_verify ui_tree_compact ui_tree_raw; do
  [ -f "$OUT/$f.python.json" ] || { echo "ERROR: $OUT/$f.python.json missing — run run-parity.sh first" >&2; exit 1; }
done
[ -f "$OUT/doctor.python.json" ] && [ -f "$OUT/doctor.go.json" ] ||
  { echo "ERROR: doctor artefacts missing — run run-parity.sh first" >&2; exit 1; }

mkdir -p "$FIX"
cp "$OUT/devices.python.json"        "$FIX/devices.json"
cp "$OUT/tap_verify.python.json"     "$FIX/tap_verify.json"
cp "$OUT/ui_tree_compact.python.json" "$FIX/ui_tree_compact.json"
cp "$OUT/ui_tree_raw.python.json"    "$FIX/ui_tree_raw.json"
cp "$OUT/doctor.python.json"         "$FIX/doctor.python.json"
cp "$OUT/doctor.go.json"             "$FIX/doctor.go.json"

python3 - "$OUT/screenshot_no_image.python.json" "$FIX/screenshot_no_image.json" <<'PY'
import json, sys
src, dst = sys.argv[1], sys.argv[2]
with open(src, encoding="utf-8") as fh:
    doc = json.load(fh)
# Keep the key ORDER (that is the point) and a plausible-looking value; the
# selftest never decodes it.
doc["base64"] = doc["base64"][:64]
with open(dst, "w", encoding="utf-8") as fh:
    fh.write(json.dumps(doc, ensure_ascii=False, indent=2))
PY

echo "refreshed fixtures in $FIX:"
ls -l "$FIX"
