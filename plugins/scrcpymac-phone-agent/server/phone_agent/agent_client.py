"""HTTP client for ScrcpyMac.app local Agent Service (127.0.0.1:9477)."""

from __future__ import annotations

import base64
import json
import os
import urllib.error
import urllib.request
from typing import Any, Optional

DEFAULT_AGENT_URL = os.environ.get("SCRCPYMAC_AGENT_URL", "http://127.0.0.1:9477")


class AgentClient:
    def __init__(self, base_url: str = DEFAULT_AGENT_URL, timeout_s: float = 3.0):
        self.base_url = base_url.rstrip("/")
        self.timeout_s = timeout_s
        self._available: Optional[bool] = None

    def is_available(self, *, force_check: bool = False) -> bool:
        if self._available is not None and not force_check:
            return self._available
        try:
            data = self.health()
            self._available = bool(data.get("ok")) and bool(data.get("connected"))
        except (OSError, urllib.error.URLError, json.JSONDecodeError, TimeoutError):
            self._available = False
        return self._available

    def backend_name(self) -> str:
        return "scrcpymac-agent" if self.is_available() else "adb"

    def health(self) -> dict[str, Any]:
        return self._get_json("/health")

    def device_info(self) -> dict[str, Any]:
        data = self._get_json("/device")
        screen = data.get("screen", {})
        return {
            "serial": data.get("serial", ""),
            "screen": screen,
            "foreground": {},
            "backend": "scrcpymac-agent",
        }

    def screenshot(self) -> dict[str, Any]:
        png = self._get_bytes("/screenshot")
        info = self.device_info()
        screen = info.get("screen", {})
        width = int(screen.get("width", 0))
        height = int(screen.get("height", 0))
        return {
            "serial": info.get("serial", ""),
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

    def _get_json(self, path: str) -> dict[str, Any]:
        raw = self._request("GET", path)
        return json.loads(raw.decode("utf-8"))

    def _get_bytes(self, path: str) -> bytes:
        return self._request("GET", path)

    def _post_json(self, path: str, payload: dict[str, Any]) -> dict[str, Any]:
        body = json.dumps(payload).encode("utf-8")
        raw = self._request("POST", path, body=body, content_type="application/json")
        return json.loads(raw.decode("utf-8"))

    def _request(
        self,
        method: str,
        path: str,
        *,
        body: bytes | None = None,
        content_type: str = "application/json",
    ) -> bytes:
        url = f"{self.base_url}{path}"
        headers: dict[str, str] = {}
        if body is not None:
            headers["Content-Type"] = content_type
        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout_s) as resp:
                return resp.read()
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")
            raise OSError(f"agent {method} {path} failed ({exc.code}): {detail}") from exc
