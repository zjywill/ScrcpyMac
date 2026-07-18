"""Thin adb wrapper used by the MCP server."""

from __future__ import annotations

import os
import platform
import re
import shutil
import subprocess
from dataclasses import dataclass
from typing import Optional


@dataclass
class Device:
    serial: str
    state: str
    model: str = ""
    product: str = ""

    def to_dict(self) -> dict:
        return {
            "serial": self.serial,
            "state": self.state,
            "model": self.model,
            "product": self.product,
        }


class AdbError(RuntimeError):
    pass


def _bundled_adb_path() -> Optional[str]:
    root = os.environ.get("PHONE_AGENT_ROOT")
    if not root:
        return None
    machine = platform.machine().lower()
    if machine in ("arm64", "aarch64"):
        arch = "arm64"
    elif machine in ("x86_64", "amd64"):
        arch = "x86_64"
    else:
        return None
    candidate = os.path.join(root, "bin", "darwin", arch, "adb")
    if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
        return candidate
    return None


def resolve_adb_path() -> str:
    """Resolve adb binary from env, bundled plugin binary, or common paths."""
    env_path = os.environ.get("ADB_PATH") or os.environ.get("ANDROID_ADB")
    if env_path and os.path.isfile(env_path) and os.access(env_path, os.X_OK):
        return env_path

    bundled = _bundled_adb_path()
    if bundled:
        return bundled

    which = shutil.which("adb")
    if which:
        return which

    candidates = [
        os.path.expanduser("~/Library/Android/sdk/platform-tools/adb"),
        "/opt/homebrew/bin/adb",
        "/usr/local/bin/adb",
        "/usr/bin/adb",
    ]
    for path in candidates:
        if os.path.isfile(path) and os.access(path, os.X_OK):
            return path

    raise AdbError(
        "adb not found. Install Android platform-tools, run scripts/install.sh, "
        "or set ADB_PATH."
    )


class AdbClient:
    def __init__(self, serial: Optional[str] = None, adb_path: Optional[str] = None):
        self.adb_path = adb_path or resolve_adb_path()
        self.serial = serial or os.environ.get("PHONE_AGENT_SERIAL")

    def _base_cmd(self) -> list[str]:
        cmd = [self.adb_path]
        if self.serial:
            cmd.extend(["-s", self.serial])
        return cmd

    def run(
        self,
        args: list[str],
        *,
        check: bool = True,
        text: bool = True,
        timeout: Optional[float] = 60,
    ) -> subprocess.CompletedProcess:
        cmd = self._base_cmd() + args
        try:
            result = subprocess.run(
                cmd,
                capture_output=True,
                text=text,
                timeout=timeout,
                check=False,
            )
        except subprocess.TimeoutExpired as exc:
            raise AdbError(f"adb timed out: {' '.join(cmd)}") from exc

        if check and result.returncode != 0:
            stderr = (result.stderr or "").strip()
            stdout = (result.stdout or "").strip()
            detail = stderr or stdout or f"exit {result.returncode}"
            raise AdbError(f"adb {' '.join(args)} failed: {detail}")
        return result

    def run_bytes(self, args: list[str], *, timeout: Optional[float] = 60) -> bytes:
        cmd = self._base_cmd() + args
        try:
            result = subprocess.run(
                cmd,
                capture_output=True,
                timeout=timeout,
                check=False,
            )
        except subprocess.TimeoutExpired as exc:
            raise AdbError(f"adb timed out: {' '.join(cmd)}") from exc

        if result.returncode != 0:
            stderr = (result.stderr or b"").decode("utf-8", errors="replace").strip()
            raise AdbError(f"adb {' '.join(args)} failed: {stderr}")
        return result.stdout

    def list_devices(self) -> list[Device]:
        result = self.run(["devices", "-l"])
        devices: list[Device] = []
        for line in (result.stdout or "").splitlines()[1:]:
            line = line.strip()
            if not line:
                continue
            parts = line.split()
            if len(parts) < 2:
                continue
            serial, state = parts[0], parts[1]
            model = ""
            product = ""
            for token in parts[2:]:
                if token.startswith("model:"):
                    model = token.split(":", 1)[1]
                elif token.startswith("product:"):
                    product = token.split(":", 1)[1]
            devices.append(Device(serial=serial, state=state, model=model, product=product))
        return devices

    def ensure_device(self) -> str:
        if self.serial:
            return self.serial
        devices = [d for d in self.list_devices() if d.state == "device"]
        if not devices:
            raise AdbError("No Android device connected. Plug in USB or run adb connect.")
        if len(devices) > 1:
            serials = ", ".join(d.serial for d in devices)
            raise AdbError(
                f"Multiple devices connected ({serials}). Set PHONE_AGENT_SERIAL or pass serial."
            )
        self.serial = devices[0].serial
        return self.serial

    def shell(self, command: str, *, timeout: Optional[float] = 60) -> str:
        result = self.run(["shell", command], timeout=timeout)
        return (result.stdout or "").strip()

    def screen_size(self) -> tuple[int, int]:
        output = self.shell("wm size")
        match = re.search(r"(\d+)x(\d+)", output)
        if not match:
            raise AdbError(f"Could not parse screen size from: {output!r}")
        return int(match.group(1)), int(match.group(2))

    def current_app(self) -> dict:
        output = self.shell(
            "dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -1"
        )
        package = ""
        activity = ""
        match = re.search(r"([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)", output)
        if match:
            package, activity = match.group(1), match.group(2)
        return {"package": package, "activity": activity, "raw": output}

    def screenshot_png(self) -> bytes:
        return self.run_bytes(["exec-out", "screencap", "-p"], timeout=30)

    def ui_tree_xml(self) -> str:
        remote = "/sdcard/window_dump.xml"
        self.shell(f"uiautomator dump {remote}", timeout=30)
        xml = self.shell(f"cat {remote}", timeout=30)
        self.shell(f"rm -f {remote}")
        return xml
