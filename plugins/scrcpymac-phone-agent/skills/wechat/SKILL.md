---
name: wechat
description: Send WeChat messages or navigate WeChat on a connected Android phone. Use when the user asks to message someone on WeChat (微信), open WeChat, or check WeChat.
---

# WeChat Automation

Package name: `com.tencent.mm`

## Rules

- **Always use `phone_paste` for Chinese text** — never `phone_type` for Chinese
- Prefer `phone_find_and_tap` over hard-coded coordinates
- Take `phone_screenshot` after important steps to verify UI state
- Ask for confirmation before sending messages to wrong contacts

## Send a message (step by step)

1. `phone_doctor` — confirm a device is ready
2. `phone_launch_app` with package `com.tencent.mm`
3. `phone_wait_for_text` with text `微信` (or screenshot to confirm main screen)
4. `phone_find_and_tap` with `content_desc` `搜索` or `text` `搜索`
5. `phone_paste` the contact name
6. `phone_wait_for_text` with the contact name
7. `phone_find_and_tap` with `text` matching the contact
8. `phone_paste` the message body
9. `phone_find_and_tap` with `text` `发送` or `content_desc` `发送`
10. `phone_screenshot` to verify the message bubble appears

## Shortcut

For a simple send when the contact is easy to find:

```
phone_send_wechat(contact="联系人", message="消息内容")
```

Then verify with `phone_screenshot`.

## Common issues

| Problem | Fix |
|---------|-----|
| Search not found | `phone_screenshot`, tap search area manually with coordinates from image |
| Contact not in results | Confirm exact contact name; scroll list with `phone_swipe` |
| Paste fails | Device must be Android 10+; try `phone_key` `back` and retry |
| WeChat not logged in | Ask user to log in manually first |
