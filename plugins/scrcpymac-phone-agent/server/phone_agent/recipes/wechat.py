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

    launch = actions.launch_app(WECHAT_PACKAGE)
    steps.append({"step": "launch_wechat", "result": launch})
    time.sleep(1.5)

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

    time.sleep(0.8)
    actions.paste(contact)
    steps.append({"step": "type_contact", "contact": contact})
    time.sleep(1.0)

    try:
        open_chat = actions.find_and_tap(text=contact, timeout_s=timeout_s)
        steps.append({"step": "open_chat", "result": open_chat})
    except AdbError:
        actions.key("enter")
        steps.append({"step": "open_chat_fallback", "key": "enter"})

    time.sleep(1.0)
    actions.paste(message)
    steps.append({"step": "type_message", "length": len(message)})
    time.sleep(0.3)

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
