"""Codex MCP App resource and app-only phone controls."""

from __future__ import annotations

import base64
import io
from pathlib import Path
from typing import Any, Callable

from mcp.server.fastmcp import FastMCP
from mcp.types import CallToolResult, TextContent, ToolAnnotations
from PIL import Image

from phone_agent.actions import PhoneActions
from phone_agent.adb import AdbError
from phone_agent.scrcpy_runtime import ScrcpyRuntime, ScrcpyRuntimeError

SCRCPYMAC_APP_URI = "ui://widget/scrcpymac/app.html"
STATIC_APP_PATH = Path(__file__).resolve().parent / "static" / "scrcpymac-app.html"

def _resource_meta(connect_domains: list[str]) -> dict[str, Any]:
    return {
        "ui": {
            "prefersBorder": False,
            "csp": {
                "connectDomains": connect_domains,
                "resourceDomains": ["data:", "blob:"],
            },
        },
        "openai/widgetDescription": "Full ScrcpyMac phone control workspace.",
        "openai/widgetPrefersBorder": False,
        "openai/widgetCSP": {
            "connect_domains": connect_domains,
            "resource_domains": ["data:", "blob:"],
        },
    }

OPEN_TOOL_META: dict[str, Any] = {
    "ui": {
        "resourceUri": SCRCPYMAC_APP_URI,
        "visibility": ["model", "app"],
    },
    "ui/resourceUri": SCRCPYMAC_APP_URI,
    "openai/outputTemplate": SCRCPYMAC_APP_URI,
    "openai/widgetAccessible": True,
    "openai/toolInvocation/invoking": "Opening ScrcpyMac...",
    "openai/toolInvocation/invoked": "ScrcpyMac ready",
}

APP_ONLY_META: dict[str, Any] = {
    "ui": {
        "visibility": ["app"],
    },
}


def _result(
    payload: dict[str, Any],
    text: str,
    *,
    is_error: bool = False,
) -> CallToolResult:
    return CallToolResult(
        content=[TextContent(type="text", text=text)],
        structuredContent=payload,
        isError=is_error,
    )


def _error(exc: Exception) -> CallToolResult:
    return _result(
        {"ok": False, "error": str(exc)},
        str(exc),
        is_error=True,
    )


def _preview_frame(shot: dict[str, Any], max_width: int, quality: int) -> dict[str, Any]:
    encoded = shot.get("image_bytes")
    if encoded is not None:
        return {
            "ok": True,
            "serial": shot["serial"],
            "backend": shot.get("backend", "adb"),
            "deviceWidth": int(shot["width"]),
            "deviceHeight": int(shot["height"]),
            "frameWidth": int(shot["frame_width"]),
            "frameHeight": int(shot["frame_height"]),
            "mimeType": shot.get("mime_type", "image/jpeg"),
            "dataBase64": base64.b64encode(encoded).decode("ascii"),
            "sizeBytes": len(encoded),
        }

    source = Image.open(io.BytesIO(shot["png_bytes"])).convert("RGB")
    source_width, source_height = source.size
    if source_width > max_width:
        target_height = max(1, round(source_height * (max_width / source_width)))
        source = source.resize((max_width, target_height), Image.Resampling.BILINEAR)

    output = io.BytesIO()
    source.save(output, format="JPEG", quality=quality, optimize=False)
    encoded = output.getvalue()
    return {
        "ok": True,
        "serial": shot["serial"],
        "backend": shot.get("backend", "adb"),
        "deviceWidth": int(shot["width"]),
        "deviceHeight": int(shot["height"]),
        "frameWidth": source.width,
        "frameHeight": source.height,
        "mimeType": "image/jpeg",
        "dataBase64": base64.b64encode(encoded).decode("ascii"),
        "sizeBytes": len(encoded),
    }


def register_scrcpymac_app(
    mcp: FastMCP,
    actions_factory: Callable[[], PhoneActions],
    runtime: ScrcpyRuntime,
) -> None:
    """Register the native Codex widget and its app-only tool surface."""
    resource_meta = _resource_meta(runtime.stream_connect_domains())

    @mcp.resource(
        SCRCPYMAC_APP_URI,
        name="scrcpymac-app",
        title="ScrcpyMac",
        description="Interactive Android mirroring and control workspace.",
        mime_type="text/html;profile=mcp-app",
        meta=resource_meta,
    )
    def scrcpymac_app_resource() -> str:
        if not STATIC_APP_PATH.is_file():
            raise FileNotFoundError(
                "ScrcpyMac widget build is missing. Run scripts/build-ui.sh."
            )
        return STATIC_APP_PATH.read_text(encoding="utf-8")

    @mcp.tool(
        name="open_scrcpymac",
        title="Open ScrcpyMac",
        annotations=ToolAnnotations(
            readOnlyHint=True,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
        meta=OPEN_TOOL_META,
    )
    def open_scrcpymac(display_mode: str = "fullscreen") -> CallToolResult:
        """Open the complete ScrcpyMac phone workspace in Codex."""
        normalized_mode = display_mode if display_mode in {"inline", "fullscreen"} else "fullscreen"
        return _result(
            {
                "ok": True,
                "widget": "scrcpymac-app",
                "preferredDisplayMode": normalized_mode,
                "phase": "standalone-h264-stream",
            },
            "Opened the ScrcpyMac widget.",
        )

    @mcp.tool(
        name="scrcpymac_ui_state",
        title="Read ScrcpyMac UI state",
        annotations=ToolAnnotations(
            readOnlyHint=True,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_state() -> CallToolResult:
        """Return device discovery and backend state for the ScrcpyMac widget."""
        actions = actions_factory()
        try:
            devices = actions.devices()
            return _result(
                {
                    "ok": True,
                    "backend": runtime.backend_name(),
                    "selectedSerial": actions.selected_serial(),
                    "devices": devices,
                    "stream": runtime.status(),
                },
                "Read ScrcpyMac device state.",
            )
        except (AdbError, OSError) as exc:
            return _result(
                {
                    "ok": False,
                    "backend": runtime.backend_name(),
                    "selectedSerial": actions.selected_serial(),
                    "devices": [],
                    "stream": runtime.status(),
                    "error": str(exc),
                },
                str(exc),
            )

    @mcp.tool(
        name="scrcpymac_ui_select_device",
        title="Select ScrcpyMac device",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_select_device(serial: str) -> CallToolResult:
        """Select the Android serial used by subsequent widget actions."""
        try:
            payload = actions_factory().select_device(serial)
            return _result(payload, f"Selected Android device {serial}.")
        except (AdbError, OSError) as exc:
            return _error(exc)

    @mcp.tool(
        name="scrcpymac_ui_start_stream",
        title="Start standalone ScrcpyMac stream",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=False,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_start_stream(
        serial: str,
        max_fps: int = 60,
        resolution_percent: int = 50,
    ) -> CallToolResult:
        """Start the plugin-owned scrcpy H.264 and control session."""
        try:
            actions_factory().select_device(serial)
            payload = runtime.start(
                serial,
                max_fps=max_fps,
                resolution_percent=resolution_percent,
            )
            return _result(payload, "Started the standalone ScrcpyMac H.264 stream.")
        except (AdbError, OSError, ScrcpyRuntimeError) as exc:
            return _error(exc)

    @mcp.tool(
        name="scrcpymac_ui_stream_status",
        title="Read standalone ScrcpyMac stream status",
        annotations=ToolAnnotations(
            readOnlyHint=True,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_stream_status() -> CallToolResult:
        """Read the plugin-owned H.264 stream state and measured FPS."""
        payload = runtime.status()
        return _result(payload, "Read the standalone ScrcpyMac stream status.")

    @mcp.tool(
        name="scrcpymac_ui_stop_stream",
        title="Stop standalone ScrcpyMac stream",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_stop_stream() -> CallToolResult:
        """Stop the plugin-owned scrcpy session and release its adb forward."""
        payload = runtime.stop()
        return _result(payload, "Stopped the standalone ScrcpyMac stream.")

    @mcp.tool(
        name="scrcpymac_ui_snapshot",
        title="Capture ScrcpyMac preview frame",
        annotations=ToolAnnotations(
            readOnlyHint=True,
            destructiveHint=False,
            idempotentHint=False,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_snapshot(
        max_width: int = 540,
        quality: int = 60,
    ) -> CallToolResult:
        """Capture a compressed frame for the ScrcpyMac widget."""
        max_width = max(320, min(int(max_width), 1200))
        quality = max(45, min(int(quality), 90))
        try:
            actions = actions_factory()
            preview_frame = getattr(actions, "preview_frame", None)
            shot = (
                preview_frame(max_width=max_width, quality=quality)
                if callable(preview_frame)
                else actions.screenshot()
            )
            payload = _preview_frame(shot, max_width, quality)
            return _result(payload, "Captured a ScrcpyMac preview frame.")
        except (AdbError, OSError, ValueError) as exc:
            return _error(exc)

    @mcp.tool(
        name="scrcpymac_ui_tap",
        title="Tap ScrcpyMac preview",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=False,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_tap(x: float, y: float) -> CallToolResult:
        """Tap normalized preview coordinates."""
        try:
            payload = (
                runtime.tap_relative(x, y)
                if runtime.is_active()
                else actions_factory().tap_relative(x, y, verify=False, retries=0)
            )
            return _result(payload, "Tapped the Android screen.")
        except (AdbError, OSError, ScrcpyRuntimeError) as exc:
            return _error(exc)

    @mcp.tool(
        name="scrcpymac_ui_swipe",
        title="Swipe ScrcpyMac preview",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=False,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_swipe(
        x1: float,
        y1: float,
        x2: float,
        y2: float,
        duration_ms: int = 300,
    ) -> CallToolResult:
        """Swipe between normalized preview coordinates."""
        try:
            if not all(0.0 <= value <= 1.0 for value in (x1, y1, x2, y2)):
                raise AdbError("swipe coordinates must be between 0 and 1")
            if runtime.is_active():
                payload = runtime.swipe_relative(
                    x1,
                    y1,
                    x2,
                    y2,
                    duration_ms=max(0, min(int(duration_ms), 10_000)),
                )
            else:
                actions = actions_factory()
                info = actions.device_info()
                screen = info["screen"]
                width = max(1, int(screen["width"]))
                height = max(1, int(screen["height"]))
                payload = actions.swipe(
                    round(x1 * (width - 1)),
                    round(y1 * (height - 1)),
                    round(x2 * (width - 1)),
                    round(y2 * (height - 1)),
                    duration_ms=max(0, min(int(duration_ms), 10_000)),
                )
            return _result(payload, "Swiped the Android screen.")
        except (
            AdbError,
            OSError,
            ScrcpyRuntimeError,
            KeyError,
            TypeError,
            ValueError,
        ) as exc:
            return _error(exc)

    @mcp.tool(
        name="scrcpymac_ui_key",
        title="Press ScrcpyMac navigation key",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=False,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_key(name: str) -> CallToolResult:
        """Press an Android navigation or hardware key."""
        try:
            payload = runtime.key(name) if runtime.is_active() else actions_factory().key(name)
            return _result(payload, f"Pressed Android key {name}.")
        except (AdbError, OSError, ScrcpyRuntimeError) as exc:
            return _error(exc)

    @mcp.tool(
        name="scrcpymac_ui_paste",
        title="Paste text through ScrcpyMac",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=False,
            openWorldHint=False,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_paste(text: str) -> CallToolResult:
        """Paste text into the focused Android field."""
        try:
            payload = runtime.paste(text) if runtime.is_active() else actions_factory().paste(text)
            return _result(payload, "Pasted text into Android.")
        except (AdbError, OSError, ScrcpyRuntimeError) as exc:
            return _error(exc)

    @mcp.tool(
        name="scrcpymac_ui_connect_wifi",
        title="Connect ScrcpyMac over Wi-Fi",
        annotations=ToolAnnotations(
            readOnlyHint=False,
            destructiveHint=False,
            idempotentHint=False,
            openWorldHint=True,
        ),
        meta=APP_ONLY_META,
    )
    def scrcpymac_ui_connect_wifi(host: str, port: int = 5555) -> CallToolResult:
        """Connect adb to an Android device over Wi-Fi."""
        try:
            payload = actions_factory().connect_wifi(host.strip(), port=port)
            return _result(payload, "Connected to Android over Wi-Fi.")
        except (AdbError, OSError) as exc:
            return _error(exc)
