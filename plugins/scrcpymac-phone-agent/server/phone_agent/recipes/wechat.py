"""WeChat automation recipes."""

from __future__ import annotations

import time

from phone_agent.actions import PhoneActions
from phone_agent.adb import AdbError

WECHAT_PACKAGE = "com.tencent.mm"


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

        search_targets = [
            {"content_desc": "搜索"},
            {"text": "搜索"},
            {"content_desc": "Search"},
            {"text": "Search"},
        ]
        search_tapped = False
        for target in search_targets:
            try:
                tap = actions.find_and_tap(timeout_s=3, **target)
                steps.append({"step": "open_search", "target": target, "result": tap})
                search_tapped = True
                break
            except AdbError:
                continue

        if not search_tapped:
            info = actions.device_info()
            w = info["screen"]["width"]
            tap = actions.tap(int(w * 0.88), 180)
            steps.append({"step": "open_search_fallback", "result": tap})

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

        send_targets = [
            {"text": "发送"},
            {"content_desc": "发送"},
            {"text": "Send"},
            {"content_desc": "Send"},
        ]
        sent = False
        for target in send_targets:
            try:
                tap = actions.find_and_tap(timeout_s=3, **target)
                steps.append({"step": "tap_send", "target": target, "result": tap})
                sent = True
                break
            except AdbError:
                continue

        if not sent:
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
    markers = ["微信", "通讯录", "发现", "我"]
    for marker in markers:
        try:
            found = actions.wait_for_text(marker, timeout_s=timeout_s / len(markers))
            steps.append({"step": "wait_wechat_ready", "marker": marker, "result": found})
            return
        except AdbError:
            continue
    steps.append({"step": "wait_wechat_ready", "skipped": True})


def _wait_for_search_input(actions: PhoneActions, steps: list[dict], *, timeout_s: float) -> None:
    markers = ["搜索", "Search"]
    for marker in markers:
        try:
            found = actions.wait_for_text(marker, timeout_s=timeout_s)
            steps.append({"step": "wait_search_input", "marker": marker, "result": found})
            return
        except AdbError:
            continue
    time.sleep(0.5)
    steps.append({"step": "wait_search_input", "skipped": True})


def _safe_screenshot(actions: PhoneActions) -> dict:
    try:
        return actions.screenshot()
    except AdbError:
        return {"width": 0, "height": 0, "size_bytes": 0}
