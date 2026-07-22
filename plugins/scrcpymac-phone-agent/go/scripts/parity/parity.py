#!/usr/bin/env python3
"""Prove the Go phone-agent is a drop-in replacement for the Python one.

For each migration-milestone tool (doctor, devices, screenshot, tap, ui-tree)
this drives BOTH implementations against the same attached device with
identical arguments and diffs the JSON they return:

  * Python  — imported in-process (`phone_agent.server`), so the tool functions
              themselves run, not a re-implementation of them. The string a
              tool returns IS `content[0].text` on the wire; FastMCP does not
              touch it.
  * Go      — a real MCP stdio session against the built binary
              (`phone-agent mcp` + initialize + tools/call), so what is
              compared is what Codex would actually receive.

Only declared-volatile fields are excused, by path, in each case's rules; see
compare.py. Everything else is compared including key ORDER, which is contract
(Python dicts are insertion-ordered, Go maps are not).

Two things keep the verdict honest on a live device:

  * a preflight that refuses to run if the screen is not static, if the device
    is not the expected one, or if the tap target lands inside a clickable node;
  * a noise baseline — the Python side of the volatile cases runs twice, and
    any path that differs between the two Python runs is attributed to the
    device, not to the port.

Usage (see run-parity.sh, which sets everything up):
    python3 parity.py --go-bin PATH [--serial S] [--cases doctor,devices,...]

Exit status: 0 parity, 1 diffs found, 2 harness or preflight failure.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import re
import shutil
import struct
import subprocess
import sys
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Optional

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import compare  # noqa: E402
from compare import DROP, EXPECTED, IGNORE, Pairs, Report, Rules  # noqa: E402
from mcpstdio import MCPError, StdioMCPClient  # noqa: E402

PLUGIN_ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
SERVER_DIR = os.path.join(PLUGIN_ROOT, "server")

# Doctor checks the Go port deliberately renamed; spec-adb-doctor.md §2.
DOCTOR_PY_ONLY_CHECKS = ["python", "mcp_package"]
DOCTOR_GO_ONLY_CHECKS = ["binary", "plugin_root"]
DOCTOR_PY_ONLY_KEYS = ["uv_available"]

RESET = "\033[0m"
BOLD = "\033[1m"
RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
DIM = "\033[2m"


def colour(text: str, code: str) -> str:
    if not sys.stdout.isatty():
        return text
    return f"{code}{text}{RESET}"


# ---------------------------------------------------------------------------
# Case results
# ---------------------------------------------------------------------------


@dataclass
class Case:
    name: str
    tool: str
    args: dict[str, Any]
    report: Optional[Report] = None
    py_raw: str = ""
    go_raw: str = ""
    notes: list[str] = field(default_factory=list)
    failures: list[str] = field(default_factory=list)
    noise_paths: set[str] = field(default_factory=set)
    skipped: str = ""

    @property
    def real_diffs(self) -> list[compare.Diff]:
        if self.report is None:
            return []
        return [d for d in self.report.diffs if d.path not in self.noise_paths]

    @property
    def noise_diffs(self) -> list[compare.Diff]:
        if self.report is None:
            return []
        return [d for d in self.report.diffs if d.path in self.noise_paths]

    @property
    def ok(self) -> bool:
        if self.skipped:
            return True
        return not self.real_diffs and not self.failures


# ---------------------------------------------------------------------------
# Drivers
# ---------------------------------------------------------------------------


class PythonDriver:
    """The Python implementation, driven through its own module."""

    def __init__(self) -> None:
        if SERVER_DIR not in sys.path:
            sys.path.insert(0, SERVER_DIR)
        import phone_agent  # noqa: F401

        pkg_dir = os.path.dirname(os.path.abspath(phone_agent.__file__))
        expected = os.path.join(SERVER_DIR, "phone_agent")
        if os.path.realpath(pkg_dir) != os.path.realpath(expected):
            raise SystemExit(
                f"phone_agent resolved to {pkg_dir}, not the plugin's {expected}. "
                "Refusing to compare against a different checkout."
            )
        from phone_agent import server as pyserver  # noqa: E402

        self.server = pyserver
        self.version = phone_agent.__version__

    def close(self) -> None:
        try:
            self.server._runtime.close()
        except Exception:
            pass

    # Each of these returns the exact string the tool puts in content[0].text.
    def doctor(self) -> str:
        return self.server.phone_doctor()

    def devices(self) -> str:
        return self.server.phone_list_devices()

    def screenshot(self, include_image: bool) -> tuple[str, Optional[bytes]]:
        out = self.server.phone_screenshot(include_image=include_image)
        if isinstance(out, tuple):
            text, image = out
            return text, bytes(image.data)
        return out, None

    def tap(self, x: int, y: int, verify: bool, retries: int) -> str:
        return self.server.phone_tap(x=x, y=y, verify=verify, retries=retries)

    def ui_tree(self, compact: bool) -> str:
        return self.server.phone_ui_tree(compact=compact)


class GoDriver:
    """The Go implementation, driven over a real MCP stdio session."""

    def __init__(self, client: StdioMCPClient) -> None:
        self.client = client

    def _call(self, tool: str, args: dict[str, Any], timeout: float = 120.0) -> dict[str, Any]:
        res = self.client.call_tool(tool, args, timeout=timeout)
        if res.get("isError"):
            texts = StdioMCPClient.text_blocks(res)
            raise MCPError(f"{tool} returned isError=true: {texts}")
        return res

    def text(self, tool: str, args: dict[str, Any], timeout: float = 120.0) -> str:
        res = self._call(tool, args, timeout)
        texts = StdioMCPClient.text_blocks(res)
        if not texts:
            raise MCPError(f"{tool} returned no text content block: {res}")
        return texts[0]

    def doctor(self) -> str:
        return self.text("phone_doctor", {})

    def devices(self) -> str:
        return self.text("phone_list_devices", {})

    def screenshot(self, include_image: bool) -> tuple[str, Optional[bytes], dict[str, Any]]:
        res = self._call("phone_screenshot", {"include_image": include_image}, timeout=90)
        texts = StdioMCPClient.text_blocks(res)
        images = StdioMCPClient.image_blocks(res)
        png = base64.b64decode(images[0]["data"]) if images else None
        return texts[0], png, res

    def tap(self, x: int, y: int, verify: bool, retries: int) -> str:
        return self.text(
            "phone_tap", {"x": x, "y": y, "verify": verify, "retries": retries}, timeout=120
        )

    def ui_tree(self, compact: bool) -> str:
        return self.text("phone_ui_tree", {"compact": compact}, timeout=90)


# ---------------------------------------------------------------------------
# Device helpers (used only by the preflight/postflight; never by a case)
# ---------------------------------------------------------------------------


class Adb:
    def __init__(self, path: str, serial: str) -> None:
        self.path = path
        self.serial = serial

    def run(self, args: list[str], timeout: float = 20.0) -> subprocess.CompletedProcess:
        argv = [self.path]
        if self.serial:
            argv += ["-s", self.serial]
        return subprocess.run(argv + args, capture_output=True, text=True, timeout=timeout)

    def run_global(self, args: list[str], timeout: float = 20.0) -> subprocess.CompletedProcess:
        return subprocess.run([self.path] + args, capture_output=True, text=True, timeout=timeout)

    def shell(self, cmd: str, timeout: float = 30.0) -> str:
        return self.run(["shell", cmd], timeout=timeout).stdout.replace("\r\n", "\n").strip()

    def screenshot(self) -> bytes:
        argv = [self.path]
        if self.serial:
            argv += ["-s", self.serial]
        return subprocess.run(argv + ["exec-out", "screencap", "-p"], capture_output=True,
                              timeout=40).stdout

    def forwards(self) -> list[str]:
        return sorted(l for l in self.run_global(["forward", "--list"]).stdout.splitlines() if l.strip())

    def foreground(self) -> str:
        return self.shell("dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -1")


def png_size(data: bytes) -> tuple[int, int]:
    """Width/height straight out of the IHDR, no image library needed."""
    if len(data) < 24 or data[:8] != b"\x89PNG\r\n\x1a\n":
        return (0, 0)
    w, h = struct.unpack(">II", data[16:24])
    return int(w), int(h)


# ---------------------------------------------------------------------------
# Comparison core (pure; selftest.py drives these without a device)
# ---------------------------------------------------------------------------


def compare_payloads(
    case: Case,
    py_raw: str,
    go_raw: str,
    rules: Rules,
    py_baseline: Optional[str] = None,
) -> Case:
    """Fill in `case` from the two raw tool results.

    Three independent checks, because any one of them can be fooled:
      1. the ordered path walk (values, types, key order, list shape);
      2. mask the declared-volatile leaves and re-serialise BOTH sides with the
         same encoder — a cross-check on the walk itself;
      3. for a case with no volatility rules at all, plain byte equality.
    """
    case.py_raw, case.go_raw = py_raw, go_raw

    for side, raw in (("python", py_raw), ("go", go_raw)):
        for problem in compare.format_checks(raw):
            if side == "go":
                case.failures.append(f"go json formatting: {problem}")
            else:
                case.notes.append(f"python json formatting: {problem}")

    try:
        py, go = compare.parse(py_raw), compare.parse(go_raw)
    except json.JSONDecodeError as exc:
        case.failures.append(f"result is not valid JSON: {exc}")
        return case

    case.report = compare.diff(py, go, rules)

    if py_baseline is not None:
        try:
            noise = compare.diff(py, compare.parse(py_baseline), rules)
            case.noise_paths = {d.path for d in noise.diffs}
            if case.noise_paths:
                case.notes.append(
                    f"device noise between two consecutive Python runs at "
                    f"{len(case.noise_paths)} path(s): {sorted(case.noise_paths)[:6]}"
                )
            else:
                case.notes.append(
                    "two consecutive Python runs were byte-identical (no device noise)"
                )
        except json.JSONDecodeError as exc:
            case.notes.append(f"noise baseline unparseable: {exc}")

    m_py = compare.dumps(compare.masked(py, rules))
    m_go = compare.dumps(compare.masked(go, rules))
    if m_py != m_go and not case.report.diffs:
        case.failures.append(
            "masked re-serialisation differs but the path walk found nothing — "
            "the diff engine missed something"
        )

    if not rules.spec:
        case.notes.append(
            "raw bytes identical" if py_raw == go_raw else "raw bytes DIFFER (see diffs)"
        )
    return case


# ---------------------------------------------------------------------------
# Doctor: the one tool whose payload intentionally differs
# ---------------------------------------------------------------------------


def compare_doctor(py_raw: str, go_raw: str) -> Case:
    """Compare phone_doctor, allowing exactly the deviations spec-adb-doctor.md
    sanctions and nothing else.

    Sanctioned: the check `python` -> `binary` and `mcp_package` -> `plugin_root`
    renames (which must keep their positions), and the removal of the top-level
    `uv_available`. Everything that survives that removal — including the order
    of the remaining eight checks and every extra key on them — is compared
    strictly.

    Module-level and pure so selftest.py can prove it rejects a broken Go
    doctor without a device.
    """
    case = Case("doctor", "phone_doctor", {})
    case.py_raw, case.go_raw = py_raw, go_raw

    for problem in compare.format_checks(go_raw):
        case.failures.append(f"go json formatting: {problem}")

    p, g = compare.parse(py_raw), compare.parse(go_raw)
    p_names = [c.get("name") for c in p.get("checks", [])]
    g_names = [c.get("name") for c in g.get("checks", [])]

    for name in DOCTOR_PY_ONLY_CHECKS:
        if name not in p_names:
            case.failures.append(f"python doctor lost its '{name}' check: {p_names}")
        if name in g_names:
            case.failures.append(f"go doctor still emits the removed '{name}' check")
    for name in DOCTOR_GO_ONLY_CHECKS:
        if name not in g_names:
            case.failures.append(f"go doctor is missing the intended '{name}' check: {g_names}")
        if name in p_names:
            case.notes.append(f"python doctor unexpectedly has '{name}'")
    for a, b in zip(DOCTOR_PY_ONLY_CHECKS, DOCTOR_GO_ONLY_CHECKS):
        if a in p_names and b in g_names and p_names.index(a) != g_names.index(b):
            case.failures.append(
                f"intended rename {a}->{b} moved position: python #{p_names.index(a)} "
                f"vs go #{g_names.index(b)}"
            )

    for name, side in [(n, "python") for n in DOCTOR_PY_ONLY_CHECKS] + [
        (n, "go") for n in DOCTOR_GO_ONLY_CHECKS
    ]:
        for c in (p if side == "python" else g).get("checks", []):
            if c.get("name") == name:
                case.notes.append(
                    f"intended {side}-only check {name}: ok={c.get('ok')} "
                    f"detail={c.get('detail')!r}"
                )

    def strip(payload: Pairs, drop_names: list[str], drop_keys: list[str]) -> Pairs:
        out = Pairs()
        for k, v in payload:
            if k in drop_keys:
                continue
            if k == "checks":
                v = [c for c in v if c.get("name") not in drop_names]
            out.append((k, v))
        return out

    for k in DOCTOR_PY_ONLY_KEYS:
        if g.has(k):
            case.failures.append(f"go doctor still emits the removed top-level key '{k}'")
        else:
            case.notes.append(f"intended: top-level '{k}' dropped (python had {p.get(k)!r})")

    case.report = compare.diff(
        strip(p, DOCTOR_PY_ONLY_CHECKS, DOCTOR_PY_ONLY_KEYS),
        strip(g, DOCTOR_GO_ONLY_CHECKS, []),
        Rules(),
    )
    case.notes.append("compared strictly after removing the two intended renames and uv_available")
    return case


# ---------------------------------------------------------------------------
# The harness
# ---------------------------------------------------------------------------


class Harness:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.cases: list[Case] = []
        self.preflight_notes: list[str] = []
        self.artifacts = args.artifacts
        os.makedirs(self.artifacts, exist_ok=True)
        self.adb: Optional[Adb] = None
        self.baseline_forwards: list[str] = []
        self.baseline_foreground = ""

    # -- artifacts ---------------------------------------------------------

    def save(self, name: str, text: str) -> None:
        with open(os.path.join(self.artifacts, name), "w", encoding="utf-8") as fh:
            fh.write(text)

    # -- preflight ---------------------------------------------------------

    def preflight(self, adb_path: str) -> None:
        self.adb = Adb(adb_path, self.args.serial)
        listing = self.adb.run_global(["devices", "-l"]).stdout
        ready = [
            l.split()[0]
            for l in listing.splitlines()[1:]
            if l.strip() and len(l.split()) > 1 and l.split()[1] == "device"
        ]
        if self.args.serial and self.args.serial not in ready:
            raise SystemExit(
                f"PREFLIGHT: device {self.args.serial} is not in state 'device'. adb devices -l:\n{listing}"
            )
        if not ready:
            raise SystemExit(f"PREFLIGHT: no ready device. adb devices -l:\n{listing}")
        if len(ready) > 1 and not self.args.serial:
            raise SystemExit(f"PREFLIGHT: {len(ready)} devices ready; pass --serial. \n{listing}")
        self.preflight_notes.append(f"adb: {adb_path}")
        self.preflight_notes.append(f"adb version: {self.adb.run_global(['version']).stdout.splitlines()[0]}")
        self.preflight_notes.append(f"ready devices: {ready}")

        self.baseline_forwards = self.adb.forwards()
        self.preflight_notes.append(
            f"pre-existing adb forwards ({len(self.baseline_forwards)}, not ours): "
            + (", ".join(self.baseline_forwards) or "none")
        )

        self.baseline_foreground = self.adb.foreground()
        self.preflight_notes.append(f"foreground: {self.baseline_foreground}")

        awake = "mWakefulness=Awake" in self.adb.shell("dumpsys power | grep mWakefulness")
        self.preflight_notes.append(f"screen awake: {awake}")
        if not awake:
            raise SystemExit(
                "PREFLIGHT: the screen is not awake. Screenshots would be black and taps "
                "would be swallowed by the lock screen, so no result would mean anything."
            )

        # Static screen: a moving screen makes screenshot/tap/ui-tree parity
        # unfalsifiable. Reuse the Python's own scoring function so the number
        # means the same thing as the one inside a tap verification.
        from phone_agent.actions import _screenshot_change_score

        shots = []
        for _ in range(3):
            png = self.adb.screenshot()
            shots.append({"png_bytes": png, "size_bytes": len(png)})
            time.sleep(0.4)
        scores = [
            round(_screenshot_change_score(shots[0], shots[1]), 4),
            round(_screenshot_change_score(shots[0], shots[2]), 4),
        ]
        self.static_scores = scores
        self.preflight_notes.append(
            f"screen change scores over 1.2s: {scores} "
            f"(png sizes {[s['size_bytes'] for s in shots]})"
        )
        self.screen_static = max(scores) == 0.0 and len({s["size_bytes"] for s in shots}) == 1
        if not self.screen_static:
            self.preflight_notes.append(
                "WARNING: the screen is NOT static; volatile-value comparisons will be "
                "reported as device noise rather than parity failures."
            )

    def tap_target_is_safe(self, x: int, y: int) -> tuple[bool, str]:
        """Refuse to tap inside anything the accessibility tree calls clickable.

        The foreground app on this device is a third-party app; a tap that
        lands on a control would both perturb the device and destroy the
        static-screen assumption every other case depends on.
        """
        assert self.adb is not None
        xml = self.adb.shell(
            "uiautomator dump /sdcard/window_dump.xml >/dev/null 2>&1 && "
            "cat /sdcard/window_dump.xml; rm -f /sdcard/window_dump.xml"
        )
        hits = []
        for m in re.finditer(
            r'clickable="(true|false)"[^>]*bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"', xml
        ):
            clickable = m.group(1) == "true"
            x1, y1, x2, y2 = (int(m.group(i)) for i in range(2, 6))
            if clickable and x1 <= x <= x2 and y1 <= y <= y2:
                hits.append(f"[{x1},{y1}][{x2},{y2}]")
        if hits:
            return False, f"({x},{y}) is inside clickable node(s) {hits}"
        return True, f"({x},{y}) is inside no clickable node"

    def postflight(self) -> list[str]:
        assert self.adb is not None
        out = []
        forwards = self.adb.forwards()
        leaked = [f for f in forwards if f not in self.baseline_forwards]
        gone = [f for f in self.baseline_forwards if f not in forwards]
        out.append(
            "adb forwards: "
            + ("unchanged" if not leaked and not gone else f"leaked={leaked} disappeared={gone}")
        )
        left = self.adb.shell("ls /sdcard/window_dump.xml 2>&1")
        out.append(f"/sdcard/window_dump.xml: {left or '<empty>'}")
        fg = self.adb.foreground()
        out.append(
            "foreground: " + ("unchanged" if fg == self.baseline_foreground else f"CHANGED -> {fg}")
        )
        return out

    # -- case plumbing -----------------------------------------------------

    def compare_case(
        self,
        case: Case,
        py_raw: str,
        go_raw: str,
        rules: Rules,
        py_baseline: Optional[str] = None,
    ) -> Case:
        self.save(f"{case.name}.python.json", py_raw)
        self.save(f"{case.name}.go.json", go_raw)
        if py_baseline is not None:
            self.save(f"{case.name}.python.baseline.json", py_baseline)
        compare_payloads(case, py_raw, go_raw, rules, py_baseline)
        self.cases.append(case)
        return case

    # -- the cases ---------------------------------------------------------

    def case_doctor(self, py: PythonDriver, go: GoDriver) -> None:
        py_raw, go_raw = py.doctor(), go.doctor()
        self.save("doctor.python.json", py_raw)
        self.save("doctor.go.json", go_raw)
        self.cases.append(compare_doctor(py_raw, go_raw))

    def case_devices(self, py: PythonDriver, go: GoDriver) -> None:
        case = Case("devices", "phone_list_devices", {})
        self.compare_case(case, py.devices(), go.devices(), Rules())

    def case_screenshot(self, py: PythonDriver, go: GoDriver) -> None:
        # (a) metadata + base64, the deterministic shape.
        case = Case("screenshot_no_image", "phone_screenshot", {"include_image": False})
        py_raw, _ = py.screenshot(include_image=False)
        go_raw, _, _ = go.screenshot(include_image=False)
        rules = Rules({"$.base64": IGNORE, "$.size_bytes": IGNORE})
        self.compare_case(case, py_raw, go_raw, rules)

        p, g = compare.parse(py_raw), compare.parse(go_raw)
        py_png = base64.b64decode(p.get("base64", ""))
        go_png = base64.b64decode(g.get("base64", ""))
        case.notes.append(
            f"png: python {len(py_png)}B {png_size(py_png)} sha={hashlib.sha256(py_png).hexdigest()[:12]} | "
            f"go {len(go_png)}B {png_size(go_png)} sha={hashlib.sha256(go_png).hexdigest()[:12]}"
        )
        if png_size(py_png) != png_size(go_png):
            case.failures.append(
                f"decoded PNG dimensions differ: {png_size(py_png)} vs {png_size(go_png)}"
            )
        if p.get("size_bytes") != len(py_png) or g.get("size_bytes") != len(go_png):
            case.failures.append("size_bytes does not match the decoded base64 length")
        if getattr(self, "screen_static", False):
            same = py_png == go_png
            case.notes.append(
                "screen is static and the two captures are "
                + ("byte-identical" if same else "NOT byte-identical (device re-encoded)")
            )

        # (b) include_image=true: the two-content-block shape.
        case2 = Case("screenshot_with_image", "phone_screenshot", {"include_image": True})
        py_raw2, py_img = py.screenshot(include_image=True)
        go_raw2, go_img, go_res = go.screenshot(include_image=True)
        self.compare_case(case2, py_raw2, go_raw2, Rules({"$.size_bytes": IGNORE}))
        blocks = [c.get("type") for c in go_res.get("content", [])]
        if blocks != ["text", "image"]:
            case2.failures.append(f"go content blocks are {blocks}, expected ['text', 'image']")
        imgs = StdioMCPClient.image_blocks(go_res)
        if imgs and imgs[0].get("mimeType") != "image/png":
            case2.failures.append(f"go image mimeType is {imgs[0].get('mimeType')!r}, expected 'image/png'")
        if go_res.get("structuredContent") is not None:
            case2.failures.append(
                "go phone_screenshot returned structuredContent; the Python tool has no "
                "return annotation so FastMCP emits none"
            )
        if "base64" in compare.parse(py_raw2).keys() or "base64" in compare.parse(go_raw2).keys():
            case2.failures.append("include_image=true must omit the base64 key")
        case2.notes.append(
            f"image bytes: python {len(py_img or b'')}B {png_size(py_img or b'')} | "
            f"go {len(go_img or b'')}B {png_size(go_img or b'')}"
        )
        if png_size(py_img or b"") != png_size(go_img or b""):
            case2.failures.append("image content-block PNG dimensions differ")

    def case_tap(self, py: PythonDriver, go: GoDriver) -> None:
        x, y = self.args.tap_x, self.args.tap_y
        safe, why = self.tap_target_is_safe(x, y)
        note = f"tap target {why}"
        if not safe:
            for name in ("tap_no_verify", "tap_verify"):
                c = Case(name, "phone_tap", {"x": x, "y": y})
                c.skipped = f"refused: {why}. Pass --tap-x/--tap-y for an inert point."
                self.cases.append(c)
            return

        # (a) verify=false — one adb `input tap`, fully deterministic result.
        case = Case("tap_no_verify", "phone_tap", {"x": x, "y": y, "verify": False, "retries": 2})
        case.notes.append(note)
        py_raw = py.tap(x, y, verify=False, retries=2)
        go_raw = go.tap(x, y, verify=False, retries=2)
        self.compare_case(case, py_raw, go_raw, Rules())

        # (b) verify=true — the 5-point cross, screenshot scoring and give-up
        #     branch. after_size_bytes is genuinely volatile; change_score is
        #     NOT excused, because on a static screen it must be exactly 0.0 on
        #     both sides, and that also pins Go's float rendering.
        case2 = Case("tap_verify", "phone_tap", {"x": x, "y": y, "verify": True, "retries": 2})
        case2.notes.append(note)
        rules = Rules({"$.verification.after_size_bytes": IGNORE})
        py_raw2 = py.tap(x, y, verify=True, retries=2)
        go_raw2 = go.tap(x, y, verify=True, retries=2)
        self.compare_case(case2, py_raw2, go_raw2, rules)
        for side, raw in (("python", py_raw2), ("go", go_raw2)):
            if '"change_score": 0.0' not in raw and '"change_score": 0' in raw:
                case2.failures.append(
                    f"{side} rendered change_score as an integer; json.dumps emits 0.0"
                )

    def case_ui_tree(self, py: PythonDriver, go: GoDriver) -> None:
        # Interleaved so neither side's ui_tree cache is ever hit: alternating
        # compact/raw forces a fresh dump on every call in both processes.
        py_compact_1 = py.ui_tree(compact=True)
        py_raw_1 = py.ui_tree(compact=False)
        go_compact = go.ui_tree(compact=True)
        go_raw = go.ui_tree(compact=False)
        py_compact_2 = py.ui_tree(compact=True)
        py_raw_2 = py.ui_tree(compact=False)

        c1 = Case("ui_tree_compact", "phone_ui_tree", {"compact": True})
        self.compare_case(c1, py_compact_1, go_compact, Rules(), py_baseline=py_compact_2)
        parsed = compare.parse(py_compact_1)
        c1.notes.append(
            f"{parsed.get('count')} nodes, degraded={parsed.get('degraded')}"
        )

        c2 = Case("ui_tree_raw", "phone_ui_tree", {"compact": False})
        self.compare_case(c2, py_raw_1, go_raw, Rules(), py_baseline=py_raw_2)
        c2.notes.append(f"xml length: python {len(compare.parse(py_raw_1).get('xml',''))} chars")

    def case_errors(self, py: PythonDriver) -> None:
        """The `{"ok": false, "error": ...}` envelope, byte for byte.

        Error strings are model-visible — they are all Codex gets when a tool
        fails — so the AdbError templates are as much contract as the success
        payloads. Forcing a serial no device has makes every `-s <serial>` call
        fail identically without touching the real device at all.
        """
        bogus = "parity0deadbeef"
        env = dict(os.environ)
        env["PHONE_AGENT_SERIAL"] = bogus
        stderr_path = os.path.join(self.artifacts, "go-server-errors.stderr.log")
        client = StdioMCPClient([self.args.go_bin, "mcp"], env=env, cwd=PLUGIN_ROOT,
                                stderr_path=stderr_path, read_timeout=90)
        prev_serial = os.environ.get("PHONE_AGENT_SERIAL")
        prev_actions = py.server._actions
        try:
            client.start()
            client.initialize()
            go = GoDriver(client)
            # Rebuild the Python's lazy singleton so its AdbClient picks the
            # bogus serial out of the environment, exactly as a fresh process
            # would.
            os.environ["PHONE_AGENT_SERIAL"] = bogus
            py.server._actions = None

            for name, py_call, go_call in (
                ("error_screenshot",
                 lambda: py.screenshot(include_image=False)[0],
                 lambda: go.screenshot(include_image=False)[0]),
                ("error_tap",
                 lambda: py.tap(100, 1400, verify=False, retries=2),
                 lambda: go.tap(100, 1400, verify=False, retries=2)),
                ("error_ui_tree",
                 lambda: py.ui_tree(compact=True),
                 lambda: go.ui_tree(compact=True)),
            ):
                case = Case(name, name.replace("error_", "phone_"), {"serial": bogus})
                py_raw, go_raw = py_call(), go_call()
                self.compare_case(case, py_raw, go_raw, Rules())
                parsed = compare.parse(py_raw)
                if parsed.get("ok") is not False:
                    case.failures.append(
                        f"expected the failure envelope, got ok={parsed.get('ok')!r}"
                    )
                else:
                    case.notes.append(f"error: {parsed.get('error')!r}")
        finally:
            client.close()
            py.server._actions = prev_actions
            if prev_serial is None:
                os.environ.pop("PHONE_AGENT_SERIAL", None)
            else:
                os.environ["PHONE_AGENT_SERIAL"] = prev_serial

    # -- CLI surface -------------------------------------------------------

    def case_cli(self, py: PythonDriver) -> None:
        """`phone-agent devices|doctor` — the launcher's non-MCP surface."""
        from phone_agent.adb import AdbClient, resolve_adb_path

        env = dict(os.environ)
        out = subprocess.run(
            [self.args.go_bin, "devices"], capture_output=True, text=True, env=env, timeout=60
        )
        go_devices = out.stdout
        client = AdbClient(adb_path=resolve_adb_path())
        py_devices = json.dumps([d.to_dict() for d in client.list_devices()], indent=2) + "\n"
        case = Case("cli_devices", "phone-agent devices", {})
        case.notes.append("bare JSON array, not the phone_list_devices wrapper object")
        self.compare_case(case, py_devices.rstrip("\n"), go_devices.rstrip("\n"), Rules())

    # -- run ---------------------------------------------------------------

    def report(self) -> int:
        print()
        print(colour("=" * 78, DIM))
        print(colour(f"{BOLD}PARITY: Python (module) vs Go (MCP stdio)", BOLD))
        print(colour("=" * 78, DIM))
        for n in self.preflight_notes:
            print(f"  {n}")
        print()

        failed = 0
        for case in self.cases:
            if case.skipped:
                print(colour(f"SKIP  {case.name}", YELLOW), f"— {case.skipped}")
                continue
            status = colour("PASS", GREEN) if case.ok else colour("FAIL", RED)
            arg = json.dumps(case.args, ensure_ascii=False) if case.args else "{}"
            print(f"{status}  {BOLD}{case.name}{RESET}  {case.tool} {arg}")
            for n in case.notes:
                print(colour(f"        · {n}", DIM))
            if case.report:
                for d in case.report.expected:
                    print(colour(f"      expected diff {d.path}: {d.py!r} -> {d.go!r}", YELLOW))
                for d in case.report.ignored:
                    print(colour(f"      volatile {d.path}: python={compare._short(d.py, 80)} "
                                 f"go={compare._short(d.go, 80)}", DIM))
                for d in case.noise_diffs:
                    print(colour(f"      device noise {d.render()}", YELLOW))
            for d in case.real_diffs:
                print(colour("      " + d.render(), RED))
            for f in case.failures:
                print(colour(f"      FAILURE: {f}", RED))
            if not case.ok:
                failed += 1
        print()
        total = len([c for c in self.cases if not c.skipped])
        if failed:
            print(colour(f"{failed}/{total} cases differ", RED))
        else:
            print(colour(f"all {total} cases match", GREEN))
        print(f"raw outputs: {self.artifacts}")
        return 1 if failed else 0


# ---------------------------------------------------------------------------


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--go-bin", required=True, help="path to the built Go phone-agent binary")
    ap.add_argument("--serial", default=os.environ.get("PHONE_AGENT_SERIAL", ""),
                    help="device serial (also exported to both implementations)")
    ap.add_argument("--adb-path", default=os.environ.get("ADB_PATH", ""),
                    help="pin adb for both sides; default is each side's own resolution")
    ap.add_argument("--cases", default="doctor,devices,screenshot,tap,ui-tree,cli,errors",
                    help="comma-separated subset to run")
    ap.add_argument("--artifacts", default=os.path.join(HERE, "out"),
                    help="directory for the raw JSON both sides produced")
    ap.add_argument("--tap-x", type=int, default=int(os.environ.get("PARITY_TAP_X", "100")))
    ap.add_argument("--tap-y", type=int, default=int(os.environ.get("PARITY_TAP_Y", "1400")))
    args = ap.parse_args(argv)

    if not os.access(args.go_bin, os.X_OK):
        print(f"ERROR: {args.go_bin} is not executable", file=sys.stderr)
        return 2

    os.environ["PHONE_AGENT_ROOT"] = PLUGIN_ROOT
    if args.serial:
        os.environ["PHONE_AGENT_SERIAL"] = args.serial
    if args.adb_path:
        os.environ["ADB_PATH"] = args.adb_path

    wanted = [c.strip() for c in args.cases.split(",") if c.strip()]
    harness = Harness(args)

    try:
        pydriver = PythonDriver()
    except Exception as exc:
        print(f"ERROR: could not import the Python implementation: {exc}", file=sys.stderr)
        return 2

    from phone_agent.adb import resolve_adb_path

    try:
        adb_path = resolve_adb_path()
    except Exception as exc:
        print(f"ERROR: adb is not resolvable: {exc}", file=sys.stderr)
        return 2

    harness.preflight_notes.append(f"plugin root: {PLUGIN_ROOT}")
    harness.preflight_notes.append(f"python: {sys.executable} ({sys.version.split()[0]})")
    harness.preflight_notes.append(f"go binary: {args.go_bin}")
    harness.preflight_notes.append(
        f"ADB_PATH env: {os.environ.get('ADB_PATH', '<unset>')} | which adb: {shutil.which('adb')}"
    )
    harness.preflight_notes.append(f"python plugin version: {pydriver.version}")

    try:
        harness.preflight(adb_path)
    except SystemExit as exc:
        print(colour(str(exc), RED), file=sys.stderr)
        return 2

    stderr_path = os.path.join(harness.artifacts, "go-server.stderr.log")
    env = dict(os.environ)
    client = StdioMCPClient([args.go_bin, "mcp"], env=env, cwd=PLUGIN_ROOT,
                            stderr_path=stderr_path, read_timeout=150)
    status = 2
    try:
        client.start()
        client.initialize()
        harness.preflight_notes.append(
            f"go serverInfo: {client.server_info.get('name')} {client.server_info.get('version')}"
        )
        godriver = GoDriver(client)

        if "doctor" in wanted:
            harness.case_doctor(pydriver, godriver)
        if "devices" in wanted:
            harness.case_devices(pydriver, godriver)
        if "screenshot" in wanted:
            harness.case_screenshot(pydriver, godriver)
        if "tap" in wanted:
            harness.case_tap(pydriver, godriver)
        if "ui-tree" in wanted:
            harness.case_ui_tree(pydriver, godriver)
        if "cli" in wanted:
            harness.case_cli(pydriver)
        if "errors" in wanted:
            harness.case_errors(pydriver)

        status = harness.report()
    except (MCPError, OSError) as exc:
        print(colour(f"HARNESS ERROR: {exc}", RED), file=sys.stderr)
        harness.report()
        status = 2
    finally:
        client.close()
        pydriver.close()
        print()
        print("postflight (nothing of ours may be left behind):")
        try:
            for line in harness.postflight():
                print(f"  {line}")
        except Exception as exc:  # noqa: BLE001
            print(f"  postflight failed: {exc}")
        err = client.stderr_text()
        if err:
            print(f"  go stderr: {len(err)} bytes -> {stderr_path}")
    return status


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
