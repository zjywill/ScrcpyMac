---
name: android-nav
description: Navigate Android apps, settings, and home screen using phone agent tools. Use for opening apps, going back, scrolling lists, or generic Android UI tasks.
---

# Android Navigation

## Common keys

| Action | Tool |
|--------|------|
| Back | `phone_key` name `back` |
| Home | `phone_key` name `home` |
| Recents | `phone_key` name `recents` |
| Enter | `phone_key` name `enter` |

## Launch apps

```
phone_launch_app(package="com.android.settings")   # Settings
phone_launch_app(package="com.tencent.mm")         # WeChat
phone_current_app()                                 # verify foreground
```

## Find and interact

1. `phone_ui_tree` — get clickable nodes with text and center coordinates
2. `phone_find_and_tap` — match by `text` or `content_desc`
3. `phone_swipe` — scroll lists (e.g. swipe up: large y1 to smaller y2)

## Scroll pattern

To scroll down a list on a 1080×2400 screen:

```
phone_swipe(x1=540, y1=1800, x2=540, y2=600, duration_ms=400)
```

Adjust coordinates using `phone_device_info` screen size.

## Input

- ASCII only: `phone_type`
- Chinese / emoji / long text: `phone_paste`
- Shell fallback: `phone_shell`

## Workflow template

1. `phone_screenshot` — understand current screen
2. `phone_ui_tree` or `phone_find_and_tap` — locate target
3. Act (tap, paste, key)
4. `phone_screenshot` — confirm result
