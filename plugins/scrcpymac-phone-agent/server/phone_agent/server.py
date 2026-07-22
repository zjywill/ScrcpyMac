"""MCP server exposing Android phone control tools."""

from __future__ import annotations

import atexit
import signal
from typing import Optional

from mcp.server.fastmcp import FastMCP, Image

from phone_agent.actions import PhoneActions, json_result
from phone_agent.adb import AdbError
from phone_agent.doctor import run_doctor
from phone_agent.mcp_ui import register_scrcpymac_app
from phone_agent.recipes.wechat import send_message
from phone_agent.scrcpy_runtime import ScrcpyRuntime

mcp = FastMCP(
    "scrcpymac-phone-agent",
    instructions=(
        "Control a connected Android phone. The plugin owns its standalone "
        "scrcpy H.264 and control session; ScrcpyMac.app is not required. "
        "ADB remains available for screenshots and accessibility automation. "
        "For Chinese text, use phone_paste. WeChat: com.tencent.mm."
    ),
)

_actions: Optional[PhoneActions] = None
_runtime = ScrcpyRuntime()
atexit.register(_runtime.close)


def _stop_on_sigterm(_signum, _frame) -> None:
    _runtime.close()
    raise SystemExit(0)


signal.signal(signal.SIGTERM, _stop_on_sigterm)


def _get_actions() -> PhoneActions:
    global _actions
    if _actions is None:
        _actions = PhoneActions(runtime=_runtime)
    return _actions


def _ok(payload: dict) -> str:
    payload.setdefault("ok", True)
    return json_result(payload)


def _err(exc: Exception) -> str:
    return json_result({"ok": False, "error": str(exc)})


register_scrcpymac_app(mcp, _get_actions, _runtime)


@mcp.tool()
def phone_backend() -> str:
    """Report whether the standalone H.264 runtime or adb fallback is active."""
    try:
        return _ok({"backend": _get_actions().backend()})
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_doctor() -> str:
    """Run environment diagnostics (adb, device, ScrcpyMac Agent, Python dependencies)."""
    return json_result(run_doctor())


@mcp.tool()
def phone_list_devices() -> str:
    """List Android devices visible to adb."""
    try:
        return _ok({"devices": _get_actions().devices()})
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_device_info() -> str:
    """Get connected device serial, screen size, and foreground app."""
    try:
        return _ok(_get_actions().device_info())
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_screenshot(include_image: bool = True):
    """Capture the device screen. Returns metadata JSON and optionally a PNG image."""
    try:
        shot = _get_actions().screenshot()
        payload = {
            "serial": shot["serial"],
            "width": shot["width"],
            "height": shot["height"],
            "format": shot["format"],
            "size_bytes": shot["size_bytes"],
        }
        if include_image:
            # The PNG is returned as an image content block; skip the base64
            # copy so the payload isn't duplicated.
            return _ok(payload), Image(data=shot["png_bytes"], format="png")
        payload["base64"] = shot["base64"]
        return _ok(payload)
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_tap(
    x: int,
    y: int,
    verify: bool = True,
    retries: int = 2,
) -> str:
    """Tap native device pixels. By default verifies a screen change and retries nearby."""
    try:
        return _ok(
            _get_actions().tap(
                x,
                y,
                verify=verify,
                retries=retries,
            )
        )
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_tap_relative(
    x: float,
    y: float,
    verify: bool = True,
    retries: int = 2,
) -> str:
    """Tap normalized screenshot coordinates where x and y are between 0 and 1."""
    try:
        return _ok(
            _get_actions().tap_relative(
                x,
                y,
                verify=verify,
                retries=retries,
            )
        )
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_tap_image(
    x: int,
    y: int,
    image_width: int,
    image_height: int,
    verify: bool = True,
    retries: int = 2,
) -> str:
    """Tap a point measured on a displayed/resized screenshot.

    Pass the exact width and height of the image coordinate space used to
    choose x/y. The tool maps it into current native device pixels.
    """
    try:
        return _ok(
            _get_actions().tap_image(
                x,
                y,
                image_width,
                image_height,
                verify=verify,
                retries=retries,
            )
        )
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_swipe(
    x1: int,
    y1: int,
    x2: int,
    y2: int,
    duration_ms: int = 300,
) -> str:
    """Swipe from (x1,y1) to (x2,y2)."""
    try:
        return _ok(_get_actions().swipe(x1, y1, x2, y2, duration_ms=duration_ms))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_long_press(x: int, y: int, duration_ms: int = 1000) -> str:
    """Long press at coordinates."""
    try:
        return _ok(_get_actions().long_press(x, y, duration_ms=duration_ms))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_key(name: str) -> str:
    """Press a key: back, home, recents, enter, delete, tab, menu, power, paste."""
    try:
        return _ok(_get_actions().key(name))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_type(text: str) -> str:
    """Type short ASCII text. Do not use for Chinese — use phone_paste instead."""
    try:
        return _ok(_get_actions().type_text(text))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_paste(text: str) -> str:
    """Paste text via device clipboard (supports Chinese and emoji)."""
    try:
        return _ok(_get_actions().paste(text))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_launch_app(package: str, activity: str = "") -> str:
    """Launch an Android app by package name. Example WeChat: com.tencent.mm."""
    try:
        return _ok(
            _get_actions().launch_app(package, activity=activity or None)
        )
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_current_app() -> str:
    """Get the foreground app package and activity."""
    try:
        return _ok(_get_actions().current_app())
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_ui_tree(compact: bool = True) -> str:
    """Dump the UI accessibility tree as JSON nodes or raw XML.
    Nodes carry state flags (scrollable, enabled=false, focused, checked...)
    only when noteworthy. If the result has "degraded": true, the tree is
    incomplete (WebView/Compose/game) — call phone_screenshot and use vision
    instead."""
    try:
        return _ok(_get_actions().ui_tree(compact=compact))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_find_and_tap(
    text: str = "",
    content_desc: str = "",
    resource_id: str = "",
    class_name: str = "",
    require_all: bool = False,
    exact: bool = False,
    index: int = 0,
    scroll_to_find: int = 0,
    timeout_s: float = 10,
    verify: bool = True,
) -> str:
    """Find a UI element by visible text, content-desc, resource-id, or class,
    then tap it. Provide at least one selector. require_all=True demands every
    given selector to hit (use text + resource_id to disambiguate); exact=True
    matches whole strings instead of substrings; index=N picks the Nth match;
    scroll_to_find=N scrolls down up to N times when the element is off-screen."""
    try:
        if not any((text, content_desc, resource_id, class_name)):
            raise AdbError("Provide text, content_desc, resource_id, or class_name")
        return _ok(
            _get_actions().find_and_tap(
                text=text or None,
                content_desc=content_desc or None,
                resource_id=resource_id or None,
                class_name=class_name or None,
                require_all=require_all,
                exact=exact,
                index=index,
                scroll_to_find=scroll_to_find,
                timeout_s=timeout_s,
                verify=verify,
            )
        )
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_wait_for_text(text: str, timeout_s: float = 10) -> str:
    """Wait until the given text appears in the UI tree."""
    try:
        return _ok(_get_actions().wait_for_text(text, timeout_s=timeout_s))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_shell(command: str) -> str:
    """Execute an adb shell command on the device."""
    try:
        return _ok(_get_actions().shell(command))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_send_wechat(contact: str, message: str) -> str:
    """High-level recipe: open WeChat, find a contact, and send a message."""
    try:
        return _ok(send_message(_get_actions(), contact, message))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_enable_wifi_adb(port: int = 5555) -> str:
    """Enable TCP/IP adb on a USB-connected device (required before Wi-Fi connect)."""
    try:
        return _ok(_get_actions().enable_wifi_adb(port=port))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_get_device_ip() -> str:
    """Get the device's Wi-Fi IP address (for wireless adb)."""
    try:
        return _ok(_get_actions().get_device_ip())
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_connect_wifi(host: str, port: int = 5555) -> str:
    """Connect to a device over Wi-Fi adb. Example host: 192.168.1.100."""
    try:
        return _ok(_get_actions().connect_wifi(host, port=port))
    except (AdbError, OSError) as exc:
        return _err(exc)


@mcp.tool()
def phone_disconnect_wifi(host: str = "") -> str:
    """Disconnect a Wi-Fi adb session. Leave host empty to disconnect all."""
    try:
        return _ok(_get_actions().disconnect_wifi(host))
    except (AdbError, OSError) as exc:
        return _err(exc)


def main() -> None:
    # Lazy adb connect: allow MCP handshake even when no device is plugged in yet.
    mcp.run()


if __name__ == "__main__":
    main()
