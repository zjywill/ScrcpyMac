"""HTTP client for ScrcpyMac.app local Agent Service (127.0.0.1:9477)."""

from __future__ import annotations

import base64
import json
import os
import time
import urllib.error
import urllib.request
from typing import Any, Optional

DEFAULT_AGENT_URL = os.environ.get("SCRCPYMAC_AGENT_URL", "http://127.0.0.1:9477")
DEFAULT_AVAILABILITY_TTL_S = float(os.environ.get("SCRCPYMAC_AGENT_TTL_S", "5"))


class AgentClient:
    def __init__(
        self,
        base_url: str = DEFAULT_AGENT_URL,
        timeout_s: float = 3.0,
        availability_ttl_s: float = DEFAULT_AVAILABILITY_TTL_S,
    ):
        self.base_url = base_url.rstrip("/")
        self.timeout_s = timeout_s
        self.availability_ttl_s = availability_ttl_s
        self._available: Optional[bool] = None
        self._availability_checked_at: float = 0.0
        self._device_cache: Optional[dict[str, Any]] = None
        self._device_cache_at: float = 0.0

    def is_available(self, *, force_check: bool = False) -> bool:
        now = time.monotonic()
        if (
            not force_check
            and self._available is not None
            and (now - self._availability_checked_at) < self.availability_ttl_s
        ):
            return self._available
        try:
            data = self.health()
            self._available = bool(data.get("ok")) and bool(data.get("connected"))
            self._availability_checked_at = now
            if self._available:
                self._update_device_cache_from_health(data)
        except (OSError, urllib.error.URLError, json.JSONDecodeError, TimeoutError):
            self._invalidate_availability()
        return bool(self._available)

    def backend_name(self) -> str:
        return "scrcpymac-agent" if self.is_available() else "adb"

    def health(self) -> dict[str, Any]:
        data = self._get_json("/health")
        if data.get("ok") and data.get("connected"):
            self._update_device_cache_from_health(data)
        return data

    def device_info(self, *, force_refresh: bool = False) -> dict[str, Any]:
        now = time.monotonic()
        if (
            not force_refresh
            and self._device_cache is not None
            and (now - self._device_cache_at) < self.availability_ttl_s
        ):
            return dict(self._device_cache)

        data = self._get_json("/device")
        screen = data.get("screen", {})
        info = {
            "serial": data.get("serial", ""),
            "screen": screen,
            "foreground": {},
            "backend": "scrcpymac-agent",
        }
        self._device_cache = info
        self._device_cache_at = now
        return dict(info)

    def foreground_app(self) -> dict[str, Any]:
        data = self._get_json("/foreground")
        return dict(data.get("foreground", {}))

    def screenshot(self) -> dict:
        png, headers = self._get_bytes_with_headers("/screenshot")
        width = _header_int(headers, "X-ScrcpyMac-Width")
        height = _header_int(headers, "X-ScrcpyMac-Height")
        serial = _header_value(headers, "X-ScrcpyMac-Serial")
        if not serial and self._device_cache:
            serial = str(self._device_cache.get("serial", ""))
        if not width or not height:
            info = self.device_info()
            screen = info.get("screen", {})
            width = width or int(screen.get("width", 0))
            height = height or int(screen.get("height", 0))
            serial = serial or str(info.get("serial", ""))
        return {
            "serial": serial,
            "width": width,
            "height": height,
            "format": "png",
            "base64": base64.b64encode(png).decode("ascii"),
            "png_bytes": png,
            "size_bytes": len(png),
            "backend": "scrcpymac-agent",
        }

    def tap(self, x: int, y: int) -> dict[str, Any]:
        return self._post_json("/tap", {"x": x, "y": y})

    def swipe(
        self, x1: int, y1: int, x2: int, y2: int, duration_ms: int = 300
    ) -> dict[str, Any]:
        return self._post_json(
            "/swipe",
            {"x1": x1, "y1": y1, "x2": x2, "y2": y2, "duration_ms": duration_ms},
        )

    def key(self, name: str) -> dict[str, Any]:
        return self._post_json("/key", {"name": name})

    def paste(self, text: str) -> dict[str, Any]:
        return self._post_json("/paste", {"text": text})

    def ui_tree_xml(self) -> str:
        return self.ui_tree()[0]

    def ui_tree(self) -> tuple[str, str]:
        """Return (xml, serial) from a single /ui-tree request — the response
        already carries the serial, so no extra /device round trip is needed."""
        data = self._get_json("/ui-tree")
        serial = str(data.get("serial", ""))
        if serial:
            self._merge_serial_into_device_cache(serial)
        return str(data.get("xml", "")), serial

    def _get_json(self, path: str) -> dict[str, Any]:
        body, _headers = self._request("GET", path)
        return json.loads(body.decode("utf-8"))

    def _get_bytes_with_headers(self, path: str) -> tuple[bytes, dict[str, str]]:
        return self._request("GET", path)

    def _post_json(self, path: str, payload: dict[str, Any]) -> dict[str, Any]:
        body = json.dumps(payload).encode("utf-8")
        raw, _headers = self._request("POST", path, body=body, content_type="application/json")
        data = json.loads(raw.decode("utf-8"))
        serial = data.get("serial")
        if serial:
            self._merge_serial_into_device_cache(str(serial))
        return data

    def _request(
        self,
        method: str,
        path: str,
        *,
        body: bytes | None = None,
        content_type: str = "application/json",
    ) -> tuple[bytes, dict[str, str]]:
        url = f"{self.base_url}{path}"
        headers: dict[str, str] = {}
        if body is not None:
            headers["Content-Type"] = content_type
        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout_s) as resp:
                resp_headers = {k: v for k, v in resp.headers.items()}
                return resp.read(), resp_headers
        except urllib.error.HTTPError as exc:
            self._invalidate_availability()
            detail = exc.read().decode("utf-8", errors="replace")
            raise OSError(f"agent {method} {path} failed ({exc.code}): {detail}") from exc
        except (urllib.error.URLError, TimeoutError, OSError):
            self._invalidate_availability()
            raise

    def _update_device_cache_from_health(self, data: dict[str, Any]) -> None:
        serial = data.get("serial")
        width = data.get("width")
        height = data.get("height")
        if not serial:
            return
        screen: dict[str, Any] = {}
        if width is not None and height is not None:
            screen = {"width": int(width), "height": int(height)}
        self._device_cache = {
            "serial": str(serial),
            "screen": screen,
            "foreground": {},
            "backend": "scrcpymac-agent",
        }
        self._device_cache_at = time.monotonic()

    def _merge_serial_into_device_cache(self, serial: str) -> None:
        if self._device_cache is None:
            self._device_cache = {
                "serial": serial,
                "screen": {},
                "foreground": {},
                "backend": "scrcpymac-agent",
            }
        else:
            self._device_cache["serial"] = serial
        self._device_cache_at = time.monotonic()

    def _invalidate_availability(self) -> None:
        self._available = False
        self._availability_checked_at = time.monotonic()
        self._device_cache = None
        self._device_cache_at = 0.0


def _header_value(headers: dict[str, str], name: str) -> str:
    for key, value in headers.items():
        if key.lower() == name.lower():
            return value
    return ""


def _header_int(headers: dict[str, str], name: str) -> int:
    raw = _header_value(headers, name)
    if not raw:
        return 0
    try:
        return int(raw)
    except ValueError:
        return 0
