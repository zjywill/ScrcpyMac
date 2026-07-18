"""High-level phone actions built on adb."""

from __future__ import annotations

import base64
import json
import re
import time
import xml.etree.ElementTree as ET
from typing import Any, Optional

from phone_agent.adb import AdbClient, AdbError
from phone_agent.agent_client import AgentClient

KEYCODES = {
    "back": 4,
    "home": 3,
    "recents": 187,
    "enter": 66,
    "delete": 67,
    "tab": 61,
    "menu": 82,
    "power": 26,
    "volume_up": 24,
    "volume_down": 25,
    "paste": 279,
}


class PhoneActions:
    def __init__(
        self,
        client: Optional[AdbClient] = None,
        agent: Optional[AgentClient] = None,
    ):
        self.client = client or AdbClient()
        self.agent = agent or AgentClient()

    def backend(self) -> str:
        return self.agent.backend_name()

    def _ready(self) -> AdbClient:
        self.client.ensure_device()
        return self.client

    def devices(self) -> list[dict]:
        return [d.to_dict() for d in self.client.list_devices()]

    def device_info(self) -> dict:
        if self.agent.is_available():
            info = self.agent.device_info()
            try:
                client = self._ready()
                info["foreground"] = client.current_app()
            except AdbError:
                pass
            return info
        client = self._ready()
        width, height = client.screen_size()
        app = client.current_app()
        return {
            "serial": client.serial,
            "screen": {"width": width, "height": height},
            "foreground": app,
            "backend": "adb",
        }

    def screenshot(self) -> dict:
        if self.agent.is_available():
            return self.agent.screenshot()
        client = self._ready()
        png = client.screenshot_png()
        width, height = client.screen_size()
        return {
            "serial": client.serial,
            "width": width,
            "height": height,
            "format": "png",
            "base64": base64.b64encode(png).decode("ascii"),
            "png_bytes": png,
            "size_bytes": len(png),
            "backend": "adb",
        }

    def tap(self, x: int, y: int) -> dict:
        if self.agent.is_available():
            result = self.agent.tap(x, y)
            return {**result, "serial": self.agent.device_info().get("serial", "")}
        client = self._ready()
        client.shell(f"input tap {int(x)} {int(y)}")
        return {"ok": True, "action": "tap", "x": x, "y": y, "serial": client.serial}

    def swipe(
        self,
        x1: int,
        y1: int,
        x2: int,
        y2: int,
        duration_ms: int = 300,
    ) -> dict:
        if self.agent.is_available():
            result = self.agent.swipe(x1, y1, x2, y2, duration_ms=duration_ms)
            return {**result, "serial": self.agent.device_info().get("serial", "")}
        client = self._ready()
        client.shell(
            f"input swipe {int(x1)} {int(y1)} {int(x2)} {int(y2)} {int(duration_ms)}"
        )
        return {
            "ok": True,
            "action": "swipe",
            "from": [x1, y1],
            "to": [x2, y2],
            "duration_ms": duration_ms,
            "serial": client.serial,
        }

    def long_press(self, x: int, y: int, duration_ms: int = 1000) -> dict:
        return self.swipe(x, y, x, y, duration_ms=duration_ms)

    def key(self, name: str) -> dict:
        if self.agent.is_available():
            result = self.agent.key(name)
            return {**result, "serial": self.agent.device_info().get("serial", "")}
        client = self._ready()
        key = name.lower().strip()
        if key not in KEYCODES:
            raise AdbError(f"Unknown key {name!r}. Supported: {', '.join(KEYCODES)}")
        code = KEYCODES[key]
        client.shell(f"input keyevent {code}")
        return {"ok": True, "action": "key", "key": key, "code": code, "serial": client.serial}

    def type_text(self, text: str) -> dict:
        client = self._ready()
        if not text:
            raise AdbError("text must not be empty")
        escaped = (
            text.replace("\\", "\\\\")
            .replace("%", "%25")
            .replace(" ", "%s")
            .replace("'", "\\'")
            .replace('"', '\\"')
            .replace("&", "\\&")
            .replace("<", "\\<")
            .replace(">", "\\>")
            .replace("|", "\\|")
            .replace(";", "\\;")
            .replace("(", "\\(")
            .replace(")", "\\)")
        )
        client.shell(f"input text {escaped}")
        return {"ok": True, "action": "type", "length": len(text), "serial": client.serial}

    def paste(self, text: str) -> dict:
        if self.agent.is_available():
            result = self.agent.paste(text)
            return {**result, "serial": self.agent.device_info().get("serial", "")}
        client = self._ready()
        if not text:
            raise AdbError("text must not be empty")
        safe = text.replace("'", "'\\''")
        client.shell(f"cmd clipboard set-text '{safe}'")
        time.sleep(0.15)
        client.shell("input keyevent 279")
        return {"ok": True, "action": "paste", "length": len(text), "serial": client.serial}

    def launch_app(self, package: str, activity: Optional[str] = None) -> dict:
        client = self._ready()
        if activity:
            component = f"{package}/{activity}"
            client.shell(f"am start -n {component}")
        else:
            client.shell(f"monkey -p {package} -c android.intent.category.LAUNCHER 1")
        time.sleep(1.0)
        return {
            "ok": True,
            "action": "launch",
            "package": package,
            "activity": activity,
            "foreground": client.current_app(),
            "serial": client.serial,
        }

    def shell(self, command: str) -> dict:
        client = self._ready()
        output = client.shell(command)
        return {"ok": True, "output": output, "serial": client.serial}

    def ui_tree(self, *, compact: bool = True) -> dict:
        client = self._ready()
        xml = client.ui_tree_xml()
        if not compact:
            return {"ok": True, "xml": xml, "serial": client.serial}

        nodes = []
        try:
            root = ET.fromstring(xml)
            for node in root.iter("node"):
                text = node.attrib.get("text", "").strip()
                desc = node.attrib.get("content-desc", "").strip()
                bounds = node.attrib.get("bounds", "")
                clickable = node.attrib.get("clickable", "false") == "true"
                if not (text or desc or clickable):
                    continue
                center = _bounds_center(bounds)
                nodes.append(
                    {
                        "text": text,
                        "content_desc": desc,
                        "resource_id": node.attrib.get("resource-id", ""),
                        "class": node.attrib.get("class", ""),
                        "clickable": clickable,
                        "bounds": bounds,
                        "center": center,
                    }
                )
        except ET.ParseError:
            return {"ok": True, "xml": xml, "serial": client.serial, "parse_error": True}

        return {"ok": True, "nodes": nodes, "count": len(nodes), "serial": client.serial}

    def find_and_tap(
        self,
        *,
        text: Optional[str] = None,
        content_desc: Optional[str] = None,
        timeout_s: float = 10,
        poll_interval_s: float = 0.5,
    ) -> dict:
        deadline = time.time() + timeout_s
        last_tree: dict[str, Any] = {}
        while time.time() < deadline:
            last_tree = self.ui_tree(compact=True)
            node = _find_node(last_tree.get("nodes", []), text=text, content_desc=content_desc)
            if node and node.get("center"):
                x, y = node["center"]
                tap_result = self.tap(x, y)
                return {"ok": True, "matched": node, "tap": tap_result}
            time.sleep(poll_interval_s)
        raise AdbError(
            f"Element not found within {timeout_s}s "
            f"(text={text!r}, content_desc={content_desc!r}). "
            f"Last tree had {last_tree.get('count', 0)} nodes."
        )

    def wait_for_text(self, text: str, *, timeout_s: float = 10) -> dict:
        deadline = time.time() + timeout_s
        while time.time() < deadline:
            tree = self.ui_tree(compact=True)
            node = _find_node(tree.get("nodes", []), text=text)
            if node:
                return {"ok": True, "found": node, "serial": self.client.serial}
            time.sleep(0.5)
        raise AdbError(f"Text {text!r} not found within {timeout_s}s")

    def enable_wifi_adb(self, port: int = 5555) -> dict:
        client = self._ready()
        output = client.enable_tcpip(port)
        return {
            "ok": True,
            "action": "enable_tcpip",
            "port": port,
            "output": output,
            "serial": client.serial,
        }

    def get_device_ip(self) -> dict:
        client = self._ready()
        ip = client.device_wifi_ip()
        return {"ok": True, "ip": ip, "serial": client.serial}

    def connect_wifi(self, host: str, port: int = 5555) -> dict:
        output = self.client.connect_wifi(host, port=port)
        return {
            "ok": True,
            "action": "connect_wifi",
            "target": host if ":" in host else f"{host}:{port}",
            "output": output,
        }

    def disconnect_wifi(self, host: str = "") -> dict:
        output = self.client.disconnect_wifi(host)
        return {"ok": True, "action": "disconnect_wifi", "output": output}


def _bounds_center(bounds: str) -> Optional[list[int]]:
    match = re.match(r"\[(\d+),(\d+)\]\[(\d+),(\d+)\]", bounds)
    if not match:
        return None
    x1, y1, x2, y2 = map(int, match.groups())
    return [(x1 + x2) // 2, (y1 + y2) // 2]


def _find_node(
    nodes: list[dict],
    *,
    text: Optional[str] = None,
    content_desc: Optional[str] = None,
) -> Optional[dict]:
    for node in nodes:
        if text and text in (node.get("text") or ""):
            return node
        if content_desc and content_desc in (node.get("content_desc") or ""):
            return node
    return None


def json_result(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=False, indent=2)
