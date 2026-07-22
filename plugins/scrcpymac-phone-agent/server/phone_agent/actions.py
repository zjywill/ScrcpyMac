"""High-level phone actions built on adb."""

from __future__ import annotations

import base64
import io
import json
import os
import re
import shlex
import time
import xml.etree.ElementTree as ET
from typing import Any, Optional

from PIL import Image, ImageChops

from phone_agent.adb import AdbClient, AdbError
from phone_agent.scrcpy_runtime import ScrcpyRuntime, ScrcpyRuntimeError

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
        runtime: Optional[ScrcpyRuntime] = None,
    ):
        self._client = client
        self.runtime = runtime or ScrcpyRuntime()
        self._ui_tree_cache: Optional[dict[str, Any]] = None

    @property
    def client(self) -> AdbClient:
        if self._client is None:
            self._client = AdbClient()
        return self._client

    def backend(self) -> str:
        return self.runtime.backend_name()

    def selected_serial(self) -> str:
        status = self.runtime.status()
        if status.get("serial"):
            return str(status["serial"])
        if self._client is not None and self._client.serial:
            return self._client.serial
        return os.environ.get("PHONE_AGENT_SERIAL", "")

    def select_device(self, serial: str) -> dict[str, Any]:
        serial = serial.strip()
        if not serial:
            raise AdbError("device serial must not be empty")
        devices = self.devices()
        device = next((item for item in devices if item["serial"] == serial), None)
        if device is None:
            raise AdbError(f"Android device not found: {serial}")
        if device["state"] != "device":
            raise AdbError(f"Android device is {device['state']}: {serial}")
        self.client.serial = serial
        self._invalidate_ui_tree_cache()
        return {
            "ok": True,
            "serial": serial,
            "device": device,
        }

    def _ready(self) -> AdbClient:
        self.client.ensure_device()
        return self.client

    def _invalidate_ui_tree_cache(self) -> None:
        self._ui_tree_cache = None

    def _foreground_app(self) -> dict[str, Any]:
        return self._ready().current_app()

    def devices(self) -> list[dict]:
        return [d.to_dict() for d in self.client.list_devices()]

    def device_info(self) -> dict:
        runtime = self.runtime.status()
        if runtime.get("state") == "streaming":
            return {
                "serial": runtime["serial"],
                "screen": {
                    "width": runtime["deviceWidth"],
                    "height": runtime["deviceHeight"],
                },
                "video": {
                    "width": runtime["frameWidth"],
                    "height": runtime["frameHeight"],
                    "fps": runtime["fps"],
                    "codec": runtime["codec"],
                },
                "foreground": self._foreground_app(),
                "backend": "plugin-h264",
            }
        client = self._ready()
        width, height = client.screen_size()
        app = client.current_app()
        return {
            "serial": client.serial,
            "screen": {"width": width, "height": height},
            "foreground": app,
            "backend": "adb",
        }

    def current_app(self) -> dict:
        app = self._foreground_app()
        serial = self.selected_serial() or self._ready().serial
        return {"foreground": app, "serial": serial}

    def screenshot(self) -> dict:
        return self._adb_screenshot()

    def preview_frame(self, max_width: int = 540, quality: int = 60) -> dict:
        return self._adb_screenshot()

    def _adb_screenshot(self) -> dict:
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

    def _tap_once(self, x: int, y: int) -> dict:
        runtime = self.runtime.status()
        if runtime.get("state") == "streaming":
            width = max(1, int(runtime["deviceWidth"]))
            height = max(1, int(runtime["deviceHeight"]))
            result = self.runtime.tap_relative(
                int(x) / max(width - 1, 1),
                int(y) / max(height - 1, 1),
            )
        else:
            client = self._ready()
            client.shell(f"input tap {int(x)} {int(y)}")
            result = {"ok": True, "action": "tap", "x": x, "y": y, "serial": client.serial}
        self._invalidate_ui_tree_cache()
        return result

    def tap(
        self,
        x: int,
        y: int,
        *,
        verify: bool = False,
        retries: int = 2,
        retry_radius_px: int = 32,
        settle_s: float = 0.45,
    ) -> dict:
        """Tap native device coordinates, optionally verifying a screen change.

        Verification retries around the requested point in a small cross. This
        compensates for imperfect visual bounding boxes without silently
        converting between unspecified screenshot coordinate systems.
        """
        point = self._clamp_device_point(x, y)
        if not verify:
            return self._tap_once(*point)

        retries = max(0, min(int(retries), 4))
        retry_radius_px = max(1, min(int(retry_radius_px), 96))
        settle_s = max(0.1, min(float(settle_s), 2.0))
        try:
            baseline = self.screenshot()
        except (AdbError, OSError):
            result = self._tap_once(*point)
            result["verification"] = {
                "requested": True,
                "available": False,
                "attempts": 1,
            }
            return result

        offsets = [
            (0, 0),
            (0, -retry_radius_px),
            (0, retry_radius_px),
            (-retry_radius_px, 0),
            (retry_radius_px, 0),
        ][: retries + 1]
        attempts: list[dict[str, Any]] = []
        last_result: dict[str, Any] = {}

        for dx, dy in offsets:
            candidate = self._clamp_device_point(point[0] + dx, point[1] + dy)
            last_result = self._tap_once(*candidate)
            time.sleep(settle_s)
            try:
                after = self.screenshot()
                change_score = _screenshot_change_score(baseline, after)
                changed = change_score >= 0.035
            except (AdbError, OSError):
                changed = False
                change_score = 0.0
                after = {}
            attempts.append(
                {
                    "point": [candidate[0], candidate[1]],
                    "screen_changed": changed,
                    "change_score": round(change_score, 4),
                }
            )
            if changed:
                last_result["verification"] = {
                    "requested": True,
                    "available": True,
                    "verified": True,
                    "attempts": attempts,
                    "after_size_bytes": after.get("size_bytes", 0),
                }
                return last_result

        last_result["verification"] = {
            "requested": True,
            "available": True,
            "verified": False,
            "attempts": attempts,
            "hint": "No screen change detected after tapping the target and nearby points.",
        }
        return last_result

    def tap_relative(
        self,
        x: float,
        y: float,
        *,
        verify: bool = True,
        retries: int = 2,
    ) -> dict:
        """Tap normalized screenshot coordinates in the inclusive range 0..1."""
        if not (0.0 <= x <= 1.0 and 0.0 <= y <= 1.0):
            raise AdbError("relative x and y must be between 0 and 1")
        width, height = self._required_screen_size()
        device_x = round(x * (width - 1))
        device_y = round(y * (height - 1))
        return {
            "ok": True,
            "coordinate_space": "relative",
            "source": [x, y],
            "device_point": [device_x, device_y],
            "tap": self.tap(
                device_x,
                device_y,
                verify=verify,
                retries=retries,
            ),
        }

    def tap_image(
        self,
        x: int,
        y: int,
        image_width: int,
        image_height: int,
        *,
        verify: bool = True,
        retries: int = 2,
    ) -> dict:
        """Map a point from a displayed/resized screenshot into device pixels."""
        if image_width <= 0 or image_height <= 0:
            raise AdbError("image_width and image_height must be positive")
        if not (0 <= x < image_width and 0 <= y < image_height):
            raise AdbError("image point must be inside image_width x image_height")
        width, height = self._required_screen_size()
        device_x = round((x / max(image_width - 1, 1)) * (width - 1))
        device_y = round((y / max(image_height - 1, 1)) * (height - 1))
        return {
            "ok": True,
            "coordinate_space": "image",
            "source": {
                "point": [x, y],
                "size": [image_width, image_height],
            },
            "device_point": [device_x, device_y],
            "device_size": [width, height],
            "tap": self.tap(
                device_x,
                device_y,
                verify=verify,
                retries=retries,
            ),
        }

    def swipe(
        self,
        x1: int,
        y1: int,
        x2: int,
        y2: int,
        duration_ms: int = 300,
    ) -> dict:
        runtime = self.runtime.status()
        if runtime.get("state") == "streaming":
            width = max(1, int(runtime["deviceWidth"]))
            height = max(1, int(runtime["deviceHeight"]))
            result = self.runtime.swipe_relative(
                int(x1) / max(width - 1, 1),
                int(y1) / max(height - 1, 1),
                int(x2) / max(width - 1, 1),
                int(y2) / max(height - 1, 1),
                duration_ms=duration_ms,
            )
        else:
            client = self._ready()
            client.shell(
                f"input swipe {int(x1)} {int(y1)} {int(x2)} {int(y2)} {int(duration_ms)}"
            )
            result = {
                "ok": True,
                "action": "swipe",
                "from": [x1, y1],
                "to": [x2, y2],
                "duration_ms": duration_ms,
                "serial": client.serial,
            }
        self._invalidate_ui_tree_cache()
        return result

    def long_press(self, x: int, y: int, duration_ms: int = 1000) -> dict:
        return self.swipe(x, y, x, y, duration_ms=duration_ms)

    def key(self, name: str) -> dict:
        key = name.lower().strip()
        if key not in KEYCODES:
            raise AdbError(f"Unknown key {name!r}. Supported: {', '.join(KEYCODES)}")
        if self.runtime.is_active():
            try:
                result = self.runtime.key(key)
            except ScrcpyRuntimeError as exc:
                raise AdbError(str(exc)) from exc
        else:
            client = self._ready()
            code = KEYCODES[key]
            client.shell(f"input keyevent {code}")
            result = {
                "ok": True,
                "action": "key",
                "key": key,
                "code": code,
                "serial": client.serial,
            }
        self._invalidate_ui_tree_cache()
        return result

    def type_text(self, text: str) -> dict:
        client = self._ready()
        if not text:
            raise AdbError("text must not be empty")
        # `input text` treats %s as space and % as an escape prefix; everything
        # else is neutralized by single-quoting for the device shell (blocks
        # $-expansion, backticks, and metacharacters).
        escaped = text.replace("%", "%25").replace(" ", "%s")
        client.shell(f"input text {shlex.quote(escaped)}")
        self._invalidate_ui_tree_cache()
        return {"ok": True, "action": "type", "length": len(text), "serial": client.serial}

    def paste(self, text: str) -> dict:
        if not text:
            raise AdbError("text must not be empty")
        if self.runtime.is_active():
            try:
                result = self.runtime.paste(text)
            except ScrcpyRuntimeError as exc:
                raise AdbError(str(exc)) from exc
        else:
            client = self._ready()
            client.shell(f"cmd clipboard set-text {shlex.quote(text)}")
            time.sleep(0.15)
            client.shell(f"input keyevent {KEYCODES['paste']}")
            result = {"ok": True, "action": "paste", "length": len(text), "serial": client.serial}
        self._invalidate_ui_tree_cache()
        return result

    def launch_app(self, package: str, activity: Optional[str] = None) -> dict:
        client = self._ready()
        if activity:
            component = f"{package}/{activity}"
            client.shell(f"am start -n {component}")
        else:
            client.shell(f"monkey -p {package} -c android.intent.category.LAUNCHER 1")
        self._invalidate_ui_tree_cache()
        time.sleep(1.0)
        return {
            "ok": True,
            "action": "launch",
            "package": package,
            "activity": activity,
            "foreground": self._foreground_app(),
            "serial": client.serial,
        }

    def shell(self, command: str) -> dict:
        client = self._ready()
        output = client.shell(command)
        return {"ok": True, "output": output, "serial": client.serial}

    def _dump_ui_xml(self) -> tuple[str, str]:
        client = self._ready()
        return client.ui_tree_xml(), client.serial

    def ui_tree(self, *, compact: bool = True, force_refresh: bool = False) -> dict:
        if not force_refresh and self._ui_tree_cache is not None:
            cached = self._ui_tree_cache
            if compact and "nodes" in cached:
                return dict(cached)
            if not compact and "xml" in cached:
                return dict(cached)

        xml, serial = self._dump_ui_xml()
        if not xml.strip():
            # uiautomator dump can transiently return nothing right after a
            # navigation/animation; one quick retry usually recovers it.
            time.sleep(0.3)
            xml, serial = self._dump_ui_xml()
        if not compact:
            result = {"ok": True, "xml": xml, "serial": serial}
            self._ui_tree_cache = result
            return result

        nodes = []
        try:
            root = ET.fromstring(xml)
            for node in root.iter("node"):
                attrib = node.attrib
                text = attrib.get("text", "").strip()
                desc = attrib.get("content-desc", "").strip()
                cls = attrib.get("class", "")
                clickable = attrib.get("clickable") == "true"
                scrollable = attrib.get("scrollable") == "true"
                checkable = attrib.get("checkable") == "true"
                editable = "EditText" in cls
                # Keep interactive/semantic nodes, plus scrollable containers,
                # empty input fields, and switches/checkboxes (a Switch is often
                # clickable=false because its parent row takes the click).
                if not (text or desc or clickable or scrollable or editable or checkable):
                    continue
                bounds = attrib.get("bounds", "")
                item = {
                    "index": len(nodes),
                    "text": text,
                    "content_desc": desc,
                    "resource_id": attrib.get("resource-id", ""),
                    "class": cls,
                    "clickable": clickable,
                    "bounds": bounds,
                    "center": _bounds_center(bounds),
                }
                # Boolean flags are emitted only when noteworthy to keep the
                # compact tree small; absence means the default state.
                if scrollable:
                    item["scrollable"] = True
                if attrib.get("enabled") == "false":
                    item["enabled"] = False
                if attrib.get("password") == "true":
                    item["password"] = True
                if attrib.get("focused") == "true":
                    item["focused"] = True
                if attrib.get("selected") == "true":
                    item["selected"] = True
                if checkable:
                    item["checkable"] = True
                    item["checked"] = attrib.get("checked") == "true"
                nodes.append(item)
        except ET.ParseError:
            # Never cache a broken dump; the next call should re-dump fresh.
            self._invalidate_ui_tree_cache()
            return {
                "ok": True,
                "xml": xml,
                "serial": serial,
                "parse_error": True,
                "degraded": True,
                "hint": (
                    "UI dump was empty or unparseable. Retry phone_ui_tree or "
                    "fall back to phone_screenshot for vision."
                ),
            }

        has_webview = any("WebView" in n.get("class", "") for n in nodes)
        # Few interactive nodes usually means WebView/Compose/custom-drawn UI
        # where the accessibility tree lacks real structure.
        interactive = sum(1 for n in nodes if n.get("clickable") or n.get("text"))
        result = {"ok": True, "nodes": nodes, "count": len(nodes), "serial": serial}
        if has_webview or interactive < 3:
            result["degraded"] = True
            result["hint"] = (
                "UI tree looks incomplete (WebView/Compose/custom-drawn). "
                "Fall back to phone_screenshot for vision."
            )
        self._ui_tree_cache = result
        return result

    def _screen_size(self) -> Optional[tuple[int, int]]:
        runtime = self.runtime.status()
        if runtime.get("state") == "streaming":
            width = int(runtime.get("deviceWidth", 0))
            height = int(runtime.get("deviceHeight", 0))
            if width and height:
                return width, height
        try:
            return self._ready().screen_size()
        except (AdbError, OSError):
            return None

    def _required_screen_size(self) -> tuple[int, int]:
        size = self._screen_size()
        if size is None:
            raise AdbError("Could not determine the device screen size")
        return size

    def _clamp_device_point(self, x: int, y: int) -> tuple[int, int]:
        size = self._screen_size()
        if size is None:
            return int(x), int(y)
        width, height = size
        return (
            max(0, min(width - 1, int(x))),
            max(0, min(height - 1, int(y))),
        )

    def _scroll_once(self, *, direction: str = "up") -> None:
        """One mid-screen scroll. up = content moves up (reveal what's below)."""
        size = self._screen_size()
        if size is None:
            return
        width, height = size
        x = width // 2
        if direction == "up":
            y1, y2 = int(height * 0.7), int(height * 0.3)
        else:
            y1, y2 = int(height * 0.3), int(height * 0.7)
        self.swipe(x, y1, x, y2, duration_ms=350)  # swipe invalidates the ui_tree cache
        time.sleep(0.4)

    def _poll_for_node(
        self,
        criteria: "NodeCriteria",
        *,
        timeout_s: float,
        poll_interval_s: float = 0.4,
        index: int = 0,
        scroll_to_find: int = 0,
    ) -> tuple[dict, dict]:
        """Poll the UI tree until a node matching `criteria` appears.

        Returns (matched_node, last_tree). Raises AdbError on timeout. This is
        the single wait/backoff mechanism shared by find_and_tap/wait_for_text.
        With scroll_to_find > 0, scrolls up to that many times while the node
        is missing (to reach off-screen list items).
        """
        deadline = time.time() + timeout_s
        last_tree: dict[str, Any] = {}
        scrolls_used = 0
        attempt = 0
        while time.time() < deadline:
            last_tree = self.ui_tree(compact=True, force_refresh=attempt > 0)
            node = _find_node(last_tree.get("nodes", []), criteria, index=index)
            if node is not None:
                return node, last_tree
            if scrolls_used < scroll_to_find:
                self._scroll_once(direction="up")
                scrolls_used += 1
                attempt += 1
                continue  # re-dump right after scrolling, no backoff
            attempt += 1
            time.sleep(min(poll_interval_s * (1.5 ** max(attempt - 1, 0)), 2.0))
        raise AdbError(
            f"Element not found within {timeout_s}s ({criteria.describe()}). "
            f"Last tree had {last_tree.get('count', 0)} nodes"
            + (f", after {scrolls_used} scroll(s)" if scroll_to_find else "")
            + "."
        )

    def find_and_tap(
        self,
        *,
        text: Optional[Any] = None,
        content_desc: Optional[Any] = None,
        resource_id: Optional[Any] = None,
        class_name: Optional[Any] = None,
        require_all: bool = False,
        exact: bool = False,
        index: int = 0,
        timeout_s: float = 10,
        poll_interval_s: float = 0.4,
        scroll_to_find: int = 0,
        verify: bool = False,
    ) -> dict:
        criteria = NodeCriteria(
            text=text,
            content_desc=content_desc,
            resource_id=resource_id,
            class_name=class_name,
            require_all=require_all,
            exact=exact,
        )
        node, _ = self._poll_for_node(
            criteria,
            timeout_s=timeout_s,
            poll_interval_s=poll_interval_s,
            index=index,
            scroll_to_find=scroll_to_find,
        )
        if not node.get("center"):
            raise AdbError(f"Matched node has no tappable bounds ({criteria.describe()})")
        x, y = node["center"]
        return {
            "ok": True,
            "matched": node,
            "tap": self.tap(x, y, verify=verify),
        }

    def wait_for_text(
        self,
        text: Any,
        *,
        timeout_s: float = 10,
        poll_interval_s: float = 0.4,
    ) -> dict:
        node, tree = self._poll_for_node(
            NodeCriteria(text=text), timeout_s=timeout_s, poll_interval_s=poll_interval_s
        )
        serial = tree.get("serial") or (self._client.serial if self._client else "")
        return {"ok": True, "found": node, "serial": serial}

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


def _screenshot_change_score(before: dict, after: dict) -> float:
    before_png = before.get("png_bytes")
    after_png = after.get("png_bytes")
    if not isinstance(before_png, bytes) or not isinstance(after_png, bytes):
        return 0.0
    try:
        with Image.open(io.BytesIO(before_png)) as before_image, Image.open(
            io.BytesIO(after_png)
        ) as after_image:
            sample_size = (72, 128)
            before_sample = before_image.convert("RGB").resize(sample_size)
            after_sample = after_image.convert("RGB").resize(sample_size)
            diff = ImageChops.difference(before_sample, after_sample)
            changed = sum(
                1
                for red, green, blue in diff.getdata()
                if max(red, green, blue) >= 20
            )
            return changed / (sample_size[0] * sample_size[1])
    except (OSError, ValueError):
        return 0.0


def _as_list(value: Optional[Any]) -> list[str]:
    if value is None:
        return []
    if isinstance(value, (list, tuple)):
        return [str(v) for v in value if v not in (None, "")]
    return [str(value)] if value != "" else []


class NodeCriteria:
    """A UI-node selector supporting multiple attributes and any-of alternatives.

    text / content_desc / resource_id / class_name match by substring (or by
    equality with exact=True). Any field may be a single string or a list of
    alternatives (a node matching *any* alternative wins). By default a node
    matching *any* specified attribute matches; require_all=True demands every
    specified attribute hit (AND), which disambiguates e.g. text + resource_id.
    Disabled (greyed-out) nodes are skipped unless enabled_only=False.
    """

    def __init__(
        self,
        *,
        text: Optional[Any] = None,
        content_desc: Optional[Any] = None,
        resource_id: Optional[Any] = None,
        class_name: Optional[Any] = None,
        require_all: bool = False,
        exact: bool = False,
        clickable_only: bool = False,
        enabled_only: bool = True,
    ):
        self.text = _as_list(text)
        self.content_desc = _as_list(content_desc)
        self.resource_id = _as_list(resource_id)
        self.class_name = _as_list(class_name)
        self.require_all = require_all
        self.exact = exact
        self.clickable_only = clickable_only
        self.enabled_only = enabled_only
        if not any((self.text, self.content_desc, self.resource_id, self.class_name)):
            raise AdbError("NodeCriteria requires at least one attribute")

    def _group(self, needles: list[str], value: Optional[str]) -> Optional[bool]:
        # Attribute not specified -> None (excluded); specified -> hit or miss.
        return _any_match(needles, value, exact=self.exact) if needles else None

    def matches(self, node: dict) -> bool:
        if self.enabled_only and node.get("enabled") is False:
            return False
        if self.clickable_only and not node.get("clickable"):
            return False
        groups = [
            self._group(self.text, node.get("text")),
            self._group(self.content_desc, node.get("content_desc")),
            self._group(self.resource_id, node.get("resource_id")),
            self._group(self.class_name, node.get("class")),
        ]
        specified = [g for g in groups if g is not None]
        if not specified:
            return False
        return all(specified) if self.require_all else any(specified)

    def describe(self) -> str:
        parts = []
        for name, values in (
            ("text", self.text),
            ("content_desc", self.content_desc),
            ("resource_id", self.resource_id),
            ("class", self.class_name),
        ):
            if values:
                parts.append(f"{name}={values!r}")
        if self.require_all:
            parts.append("require_all=True")
        if self.exact:
            parts.append("exact=True")
        return ", ".join(parts)


def _any_match(needles: list[str], haystack: Optional[str], *, exact: bool) -> bool:
    if not needles:
        return False
    text = haystack or ""
    if exact:
        return any(needle == text for needle in needles)
    return any(needle and needle in text for needle in needles)


def _any_substring(needles: list[str], haystack: Optional[str]) -> bool:
    return _any_match(needles, haystack, exact=False)


def _find_nodes(nodes: list[dict], criteria: NodeCriteria) -> list[dict]:
    return [node for node in nodes if criteria.matches(node)]


def _find_node(
    nodes: list[dict], criteria: NodeCriteria, index: int = 0
) -> Optional[dict]:
    matches = _find_nodes(nodes, criteria)
    if index < 0 or index >= len(matches):
        return None
    return matches[index]


def json_result(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=False, indent=2)
