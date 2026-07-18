"""Environment diagnostics for the phone agent plugin."""

from __future__ import annotations

import json
import os
import platform
import shutil
import sys
import urllib.error
from typing import Any

from phone_agent import __version__
from phone_agent.adb import AdbClient, AdbError, resolve_adb_path
from phone_agent.agent_client import AgentClient


def run_doctor() -> dict[str, Any]:
    checks: list[dict[str, Any]] = []

    def add(name: str, ok: bool, detail: str, **extra: Any) -> None:
        checks.append({"name": name, "ok": ok, "detail": detail, **extra})

    add("platform", True, f"{platform.system()} {platform.machine()}")
    add("python", sys.version_info >= (3, 10), f"{sys.version_info.major}.{sys.version_info.minor}")

    try:
        import mcp  # noqa: F401

        add("mcp_package", True, "installed")
    except ImportError:
        add("mcp_package", False, "pip install mcp")

    try:
        adb_path = resolve_adb_path()
        bundled = adb_path.startswith(os.environ.get("PHONE_AGENT_ROOT", "\0"))
        add("adb", True, adb_path, bundled=bundled)
    except AdbError as exc:
        add("adb", False, str(exc))
        return {
            "ok": False,
            "version": __version__,
            "checks": checks,
            "summary": "adb is required before controlling a phone",
        }

    try:
        version = AdbClient(adb_path=adb_path).run(["version"]).stdout.strip()
        add("adb_version", True, version.splitlines()[0] if version else "unknown")
    except AdbError as exc:
        add("adb_version", False, str(exc))

    devices: list[dict] = []
    try:
        client = AdbClient(adb_path=adb_path)
        devices = [d.to_dict() for d in client.list_devices()]
        ready = [d for d in devices if d["state"] == "device"]
        if ready:
            add("device", True, f"{len(ready)} ready device(s)", devices=devices)
        elif devices:
            add(
                "device",
                False,
                "device(s) found but not authorized — accept USB debugging prompt",
                devices=devices,
            )
        else:
            add("device", False, "no devices — connect USB or adb connect", devices=[])
    except AdbError as exc:
        add("device", False, str(exc))

    ready_devices = [d for d in devices if d.get("state") == "device"]
    if len(ready_devices) == 1:
        serial = ready_devices[0]["serial"]
        try:
            client = AdbClient(serial=serial, adb_path=adb_path)
            w, h = client.screen_size()
            add("screen_size", True, f"{w}x{h}")
            app = client.current_app()
            add("foreground_app", True, app.get("package") or "unknown", activity=app.get("activity"))
        except AdbError as exc:
            add("screen_size", False, str(exc))

    agent = AgentClient()
    try:
        agent_health = agent.health()
        agent_up = bool(agent_health.get("ok"))
        agent_connected = bool(agent_health.get("connected"))
        if agent_connected:
            add(
                "scrcpymac_agent",
                True,
                "fast path via ScrcpyMac.app :9477",
                backend="scrcpymac-agent",
            )
        elif agent_up:
            add(
                "scrcpymac_agent",
                False,
                "service running but mirror not connected — open ScrcpyMac and Connect",
            )
        else:
            add("scrcpymac_agent", False, "not available (adb fallback)")
    except (OSError, urllib.error.URLError, json.JSONDecodeError, TimeoutError):
        add("scrcpymac_agent", False, "not running — optional; uses adb instead")

    preferred_backend = "scrcpymac-agent" if agent.is_available(force_check=True) else "adb"
    all_ok = all(c["ok"] for c in checks if c["name"] not in ("foreground_app", "scrcpymac_agent"))
    return {
        "ok": all_ok,
        "version": __version__,
        "backend": preferred_backend,
        "checks": checks,
        "summary": "ready" if all_ok else "fix failed checks above",
        "uv_available": shutil.which("uv") is not None,
    }
