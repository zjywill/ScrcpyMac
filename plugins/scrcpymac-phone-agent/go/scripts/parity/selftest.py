#!/usr/bin/env python3
"""Falsify the parity harness: prove it rejects a broken Go implementation.

A parity harness that has only ever printed PASS proves nothing. This takes the
REAL payloads captured from the device (checked in under fixtures/), perturbs
the Go side one regression at a time, and asserts the harness flags each one.
Every perturbation is a mistake the Go port could plausibly make: map iteration
order, `map[string]any` alphabetisation, `json.MarshalIndent`'s HTML escaping,
`float64` rendering as an integer, a dropped `ok`, a renamed key.

Runs with no device and no Go binary — plain `python3 selftest.py`.
Exit status 0 if every perturbation was caught, 1 otherwise.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any, Callable

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import compare  # noqa: E402
from compare import IGNORE, Rules  # noqa: E402
from parity import Case, compare_doctor, compare_payloads, parse_wanted_cases  # noqa: E402

FIXTURES = os.path.join(HERE, "fixtures")


def load(name: str) -> str:
    with open(os.path.join(FIXTURES, name), encoding="utf-8") as fh:
        return fh.read()


def redump(obj: Any) -> str:
    """Re-serialise the way json_result does, so only the mutation differs."""
    return json.dumps(obj, ensure_ascii=False, indent=2)


def mutate(raw: str, fn: Callable[[Any], Any]) -> str:
    doc = json.loads(raw)
    out = fn(doc)
    return redump(doc if out is None else out)


# ---------------------------------------------------------------------------
# Perturbations
# ---------------------------------------------------------------------------


def reorder_first_last(doc: dict) -> dict:
    keys = list(doc)
    return {keys[-1]: doc[keys[-1]], **{k: doc[k] for k in keys[:-1]}}


def alphabetise(doc: Any) -> Any:
    """What Go's map[string]any does to every payload."""
    if isinstance(doc, dict):
        return {k: alphabetise(doc[k]) for k in sorted(doc)}
    if isinstance(doc, list):
        return [alphabetise(v) for v in doc]
    return doc


CASES: list[tuple[str, str, Callable[[str], str], Rules]] = []


def perturbation(name: str, fixture: str, rules: Rules = Rules()):
    def register(fn: Callable[[str], str]):
        CASES.append((name, fixture, fn, rules))
        return fn

    return register


SHOT_RULES = Rules({"$.base64": IGNORE, "$.size_bytes": IGNORE})
TAP_RULES = Rules({"$.verification.after_size_bytes": IGNORE})


@perturbation("devices: ok moved to the front (setdefault order lost)", "devices.json")
def _(raw: str) -> str:
    return mutate(raw, reorder_first_last)


@perturbation("devices: keys alphabetised (map[string]any)", "devices.json")
def _(raw: str) -> str:
    return mutate(raw, alphabetise)


@perturbation("devices: device key renamed serial -> id", "devices.json")
def _(raw: str) -> str:
    def f(d):
        for dev in d["devices"]:
            dev["id"] = dev.pop("serial")
        return d

    return mutate(raw, f)


@perturbation("devices: product dropped (omitempty on an empty string)", "devices.json")
def _(raw: str) -> str:
    def f(d):
        for dev in d["devices"]:
            dev.pop("product")
        return d

    return mutate(raw, f)


@perturbation("devices: extra key added", "devices.json")
def _(raw: str) -> str:
    def f(d):
        d["transport_id"] = 22
        return d

    return mutate(raw, f)


@perturbation("devices: empty list emitted as null", "devices.json")
def _(raw: str) -> str:
    def f(d):
        d["devices"] = None
        return d

    return mutate(raw, f)


@perturbation("screenshot: width changed", "screenshot_no_image.json", SHOT_RULES)
def _(raw: str) -> str:
    def f(d):
        d["width"] = 1081
        return d

    return mutate(raw, f)


@perturbation("screenshot: format spelled differently", "screenshot_no_image.json", SHOT_RULES)
def _(raw: str) -> str:
    def f(d):
        d["format"] = "PNG"
        return d

    return mutate(raw, f)


@perturbation("screenshot: base64 moved after ok", "screenshot_no_image.json", SHOT_RULES)
def _(raw: str) -> str:
    def f(d):
        b = d.pop("base64")
        d["base64"] = b
        return d

    return mutate(raw, f)


@perturbation("tap: change_score rendered as int (math/float64 -> 0)", "tap_verify.json", TAP_RULES)
def _(raw: str) -> str:
    def f(d):
        for a in d["verification"]["attempts"]:
            a["change_score"] = int(a["change_score"])
        return d

    return mutate(raw, f)


@perturbation("tap: one retry too many (retries not clamped to retries+1)", "tap_verify.json", TAP_RULES)
def _(raw: str) -> str:
    def f(d):
        att = d["verification"]["attempts"]
        att.append({"point": [68, 1400], "screen_changed": False, "change_score": 0.0})
        return d

    return mutate(raw, f)


@perturbation("tap: cross offsets in the wrong order", "tap_verify.json", TAP_RULES)
def _(raw: str) -> str:
    def f(d):
        att = d["verification"]["attempts"]
        att[1], att[2] = att[2], att[1]
        return d

    return mutate(raw, f)


@perturbation("tap: reports the requested point, not the last attempted", "tap_verify.json", TAP_RULES)
def _(raw: str) -> str:
    def f(d):
        d["y"] = 1400
        return d

    return mutate(raw, f)


@perturbation("tap: verification node keys alphabetised", "tap_verify.json", TAP_RULES)
def _(raw: str) -> str:
    def f(d):
        d["verification"] = alphabetise(d["verification"])
        return d

    return mutate(raw, f)


@perturbation("ui_tree: node flag order changed (checked before checkable)", "ui_tree_compact.json")
def _(raw: str) -> str:
    def f(d):
        n = d["nodes"][0]
        d["nodes"][0] = {k: n[k] for k in reversed(list(n))}
        return d

    return mutate(raw, f)


@perturbation("ui_tree: center computed with rounding instead of //", "ui_tree_compact.json")
def _(raw: str) -> str:
    def f(d):
        for n in d["nodes"]:
            if n.get("center"):
                n["center"] = [n["center"][0] + 1, n["center"][1]]
                break
        return d

    return mutate(raw, f)


@perturbation("ui_tree: one node dropped by a stricter inclusion filter", "ui_tree_compact.json")
def _(raw: str) -> str:
    def f(d):
        d["nodes"].pop()
        d["count"] -= 1
        return d

    return mutate(raw, f)


@perturbation("ui_tree: index is the XML attribute, not the output position", "ui_tree_compact.json")
def _(raw: str) -> str:
    def f(d):
        for n in d["nodes"]:
            n["index"] = 0
        return d

    return mutate(raw, f)


@perturbation("ui_tree: Chinese text escaped (ensure_ascii semantics)", "ui_tree_compact.json")
def _(raw: str) -> str:
    return json.dumps(json.loads(raw), ensure_ascii=True, indent=2)


@perturbation("ui_tree: HTML-escaped < > & (MarshalIndent default)", "ui_tree_raw.json")
def _(raw: str) -> str:
    return (
        redump(json.loads(raw))
        .replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("&", "\\u0026")
    )


@perturbation("ui_tree: trailing newline (Encode appends one)", "ui_tree_compact.json")
def _(raw: str) -> str:
    return redump(json.loads(raw)) + "\n"


@perturbation("ui_tree: 4-space indent", "ui_tree_compact.json")
def _(raw: str) -> str:
    return json.dumps(json.loads(raw), ensure_ascii=False, indent=3)


@perturbation("ui_tree: degraded flag added when it should not be", "ui_tree_compact.json")
def _(raw: str) -> str:
    def f(d):
        d["degraded"] = True
        return d

    return mutate(raw, f)


# ---------------------------------------------------------------------------


def run_payload_cases() -> list[str]:
    problems = []
    for name, fixture, fn, rules in CASES:
        py_raw = load(fixture)
        go_raw = fn(py_raw)
        if go_raw == py_raw:
            problems.append(f"{name}: perturbation was a no-op — the test proves nothing")
            continue
        case = Case("selftest", "n/a", {})
        compare_payloads(case, py_raw, go_raw, rules)
        caught = bool(case.real_diffs or case.failures)
        status = "detected" if caught else "MISSED"
        print(f"  [{status}] {name}")
        if not caught:
            problems.append(f"{name}: NOT DETECTED")
        else:
            first = (case.real_diffs[0].render().splitlines()[0] if case.real_diffs
                     else case.failures[0])
            print(f"            -> {first}")
    return problems


def run_control_cases() -> list[str]:
    """The other half: the harness must NOT cry wolf."""
    problems = []

    py_raw = load("devices.json")
    case = Case("control-identical", "n/a", {})
    compare_payloads(case, py_raw, py_raw, Rules())
    ok = not case.real_diffs and not case.failures
    print(f"  [{'ok' if ok else 'FALSE POSITIVE'}] identical payloads compare clean")
    if not ok:
        problems.append(f"identical payloads reported a diff: {case.real_diffs} {case.failures}")

    # A volatile field genuinely differing must not fail the case.
    shot = load("screenshot_no_image.json")
    doc = json.loads(shot)
    doc["size_bytes"] += 17
    doc["base64"] = doc["base64"][:-4] + "AAA="
    case = Case("control-volatile", "n/a", {})
    compare_payloads(case, shot, redump(doc), SHOT_RULES)
    ok = not case.real_diffs and not case.failures and len(case.report.ignored) == 2
    print(f"  [{'ok' if ok else 'BROKEN'}] declared-volatile fields are excused and reported")
    if not ok:
        problems.append("volatile rule did not excuse size_bytes/base64")

    # Device noise: a path that also differs between two Python runs must be
    # attributed to the device, not to the port.
    tree = load("ui_tree_compact.json")
    doc = json.loads(tree)
    doc["nodes"][5]["text"] = "9.99"
    drifted = redump(doc)
    case = Case("control-noise", "n/a", {})
    compare_payloads(case, tree, drifted, Rules(), py_baseline=drifted)
    ok = not case.real_diffs and case.noise_diffs
    print(f"  [{'ok' if ok else 'BROKEN'}] a diff the Python also shows is attributed to the device")
    if not ok:
        problems.append("noise baseline did not reclassify a device-driven diff")

    # ...but a path the Python is stable on must still fail.
    case = Case("control-noise-strict", "n/a", {})
    compare_payloads(case, tree, drifted, Rules(), py_baseline=tree)
    ok = bool(case.real_diffs)
    print(f"  [{'ok' if ok else 'MISSED'}] a diff the Python does NOT show still fails")
    if not ok:
        problems.append("noise baseline swallowed a real diff")
    return problems


def run_doctor_cases() -> list[str]:
    problems = []
    py_raw = load("doctor.python.json")
    go_raw = load("doctor.go.json")

    case = compare_doctor(py_raw, go_raw)
    ok = not case.real_diffs and not case.failures
    print(f"  [{'ok' if ok else 'FALSE POSITIVE'}] the sanctioned doctor deviations pass")
    if not ok:
        problems.append(f"real doctor pair failed: {case.failures} {case.real_diffs}")

    def check(name: str, fn: Callable[[dict], dict]) -> None:
        broken = redump(fn(json.loads(go_raw)))
        c = compare_doctor(py_raw, broken)
        caught = bool(c.real_diffs or c.failures)
        print(f"  [{'detected' if caught else 'MISSED'}] doctor: {name}")
        if not caught:
            problems.append(f"doctor {name}: NOT DETECTED")
        else:
            print(f"            -> {(c.failures or [c.real_diffs[0].render().splitlines()[0]])[0]}")

    def readd_uv(d):
        d["uv_available"] = True
        return d

    def readd_python_check(d):
        d["checks"].insert(1, {"name": "python", "ok": True, "detail": "3.13"})
        return d

    def drop_binary(d):
        d["checks"] = [c for c in d["checks"] if c["name"] != "binary"]
        return d

    def move_plugin_root(d):
        c = [x for x in d["checks"] if x["name"] == "plugin_root"][0]
        d["checks"].remove(c)
        d["checks"].append(c)
        return d

    def break_backend(d):
        d["backend"] = "plugin-h264"
        return d

    def break_summary(d):
        d["summary"] = "OK"
        return d

    def drop_bundled(d):
        for c in d["checks"]:
            c.pop("bundled", None)
        return d

    def reorder_check_keys(d):
        d["checks"] = [alphabetise(c) for c in d["checks"]]
        return d

    def reorder_checks(d):
        d["checks"][-1], d["checks"][-2] = d["checks"][-2], d["checks"][-1]
        return d

    def drop_device_extra(d):
        for c in d["checks"]:
            c.pop("devices", None)
        return d

    def change_version(d):
        d["version"] = "0.7.3"
        return d

    def flip_ok(d):
        d["ok"] = False
        return d

    for name, fn in [
        ("uv_available resurrected", readd_uv),
        ("the removed 'python' check resurrected", readd_python_check),
        ("the intended 'binary' check missing", drop_binary),
        ("'plugin_root' moved out of position", move_plugin_root),
        ("backend string changed", break_backend),
        ("summary string changed", break_summary),
        ("'bundled' extras dropped", drop_bundled),
        ("check keys alphabetised", reorder_check_keys),
        ("check order changed", reorder_checks),
        ("device list extra dropped", drop_device_extra),
        ("version drifted", change_version),
        ("top-level ok flipped", flip_ok),
    ]:
        check(name, fn)
    return problems


def run_case_selection_controls() -> list[str]:
    problems = []
    got = parse_wanted_cases("doctor, ui-tree,doctor")
    if got == ["doctor", "ui-tree", "doctor"]:
        print("  [ok] valid case subsets are preserved")
    else:
        problems.append(f"valid case subset parsed as {got!r}")

    for raw, expected in [
        ("doctor,totally_bogus_case_name", "unknown parity case"),
        (", ,", "no parity cases selected"),
    ]:
        try:
            parse_wanted_cases(raw)
        except ValueError as exc:
            if expected in str(exc):
                print(f"  [ok] invalid --cases {raw!r} is rejected")
            else:
                problems.append(f"{raw!r} failed with unexpected error: {exc}")
        else:
            problems.append(f"{raw!r} was accepted")
    return problems


def main() -> int:
    if not os.path.isdir(FIXTURES):
        print(f"ERROR: {FIXTURES} is missing. Run run-parity.sh once, then "
              "scripts/parity/refresh-fixtures.sh.", file=sys.stderr)
        return 2
    print("payload perturbations (each must be DETECTED):")
    problems = run_payload_cases()
    print("\ndoctor (sanctioned deviations pass, everything else must be DETECTED):")
    problems += run_doctor_cases()
    print("\ncontrols (the harness must not cry wolf):")
    problems += run_control_cases()
    print("\ncase selection (zero-case success must be impossible):")
    problems += run_case_selection_controls()
    print()
    if problems:
        for p in problems:
            print(f"SELFTEST FAILURE: {p}")
        return 1
    total = len(CASES) + 12
    print(f"selftest passed: {total} regressions detected, 4 controls clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
