"""WeChat automation recipes."""

from __future__ import annotations

import time

from phone_agent.actions import PhoneActions
from phone_agent.adb import AdbError

WECHAT_PACKAGE = "com.tencent.mm"
SEARCH_LABELS = ["搜索", "Search"]
SEND_LABELS = ["发送", "Send"]
HOME_MARKERS = ["微信", "通讯录", "发现", "我", "WeChat", "Chats", "Contacts"]


def send_message(
    actions: PhoneActions,
    contact: str,
    message: str,
    *,
    timeout_s: float = 15,
) -> dict:
    """Open WeChat, find a contact, and send a message."""
    if not contact.strip():
        raise AdbError("contact must not be empty")
    if not message.strip():
        raise AdbError("message must not be empty")

    steps: list[dict] = []

    try:
        launch = actions.launch_app(WECHAT_PACKAGE)
        steps.append({"step": "launch_wechat", "result": launch})

        _wait_for_wechat_ready(actions, steps, timeout_s=min(timeout_s, 8))

        # One selector call with bilingual alternatives plus WeChat's search
        # resource-id, instead of four sequential taps and a blind coordinate
        # fallback that could mis-tap the wrong element on notched/tablet layouts.
        tap = actions.find_and_tap(
            text=SEARCH_LABELS,
            content_desc=SEARCH_LABELS,
            resource_id="menu_search",
            timeout_s=min(timeout_s, 6),
        )
        steps.append({"step": "open_search", "result": tap})

        _wait_for_search_input(actions, steps, timeout_s=4)

        actions.paste(contact)
        steps.append({"step": "type_contact", "contact": contact})

        try:
            open_chat = actions.find_and_tap(text=contact, timeout_s=timeout_s)
            steps.append({"step": "open_chat", "result": open_chat})
        except AdbError:
            actions.key("enter")
            steps.append({"step": "open_chat_fallback", "key": "enter"})
            time.sleep(0.8)

        actions.paste(message)
        steps.append({"step": "type_message", "length": len(message)})

        try:
            tap = actions.find_and_tap(
                text=SEND_LABELS,
                content_desc=SEND_LABELS,
                timeout_s=3,
            )
            steps.append({"step": "tap_send", "result": tap})
        except AdbError:
            actions.key("enter")
            steps.append({"step": "send_fallback", "key": "enter"})

        time.sleep(0.5)
        screenshot = actions.screenshot()

        return {
            "ok": True,
            "recipe": "wechat_send_message",
            "contact": contact,
            "message": message,
            "steps": steps,
            "verification": {
                "width": screenshot["width"],
                "height": screenshot["height"],
                "size_bytes": screenshot["size_bytes"],
            },
        }
    except AdbError as exc:
        failure_shot = _safe_screenshot(actions)
        raise AdbError(
            f"{exc}. Steps completed: {len(steps)}. "
            f"Last screenshot: {failure_shot.get('width', 0)}x{failure_shot.get('height', 0)}"
        ) from exc


def _wait_for_wechat_ready(
    actions: PhoneActions,
    steps: list[dict],
    *,
    timeout_s: float,
) -> None:
    # One wait against all markers at once — the previous per-marker loop gave
    # each marker only timeout/len(markers) seconds, so a slow cold start could
    # fail every short wait and proceed into the splash screen.
    try:
        found = actions.wait_for_text(HOME_MARKERS, timeout_s=timeout_s)
        steps.append({"step": "wait_wechat_ready", "result": found})
    except AdbError:
        steps.append({"step": "wait_wechat_ready", "skipped": True})


def _wait_for_search_input(actions: PhoneActions, steps: list[dict], *, timeout_s: float) -> None:
    try:
        found = actions.wait_for_text(SEARCH_LABELS, timeout_s=timeout_s)
        steps.append({"step": "wait_search_input", "result": found})
    except AdbError:
        time.sleep(0.5)
        steps.append({"step": "wait_search_input", "skipped": True})


def _safe_screenshot(actions: PhoneActions) -> dict:
    try:
        return actions.screenshot()
    except AdbError:
        return {"width": 0, "height": 0, "size_bytes": 0}
