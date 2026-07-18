---
name: scrcpymac-link
description: Relate ScrcpyMac Phone Agent plugin to the ScrcpyMac mirror app. Use when the user mentions ScrcpyMac, wants fast agent control, or asks about mirror vs agent mode.
---

# ScrcpyMac Link — Fast Agent Path

ScrcpyMac's differentiator: the **plugin + App share one scrcpy session** for fast screenshots and input.

## Two components

| Component | Role |
|-----------|------|
| **ScrcpyMac.app** | Visual mirror + **Agent service** on `127.0.0.1:9477` |
| **Phone Agent plugin** | MCP tools for Cursor/Codex/Claude |

## Enable fast path (recommended)

1. Open **ScrcpyMac.app**
2. Click **Connect** to mirror the phone
3. Enable **Agent service** toggle in the sidebar
4. In Cursor/Codex, run `phone_doctor` — should show `backend: scrcpymac-agent`

## What gets faster

| Action | adb fallback | ScrcpyMac Agent |
|--------|--------------|-----------------|
| Screenshot | ~300–800ms | scrcpy frame (~fast) |
| Tap / paste | ~100–300ms | scrcpy control (~5ms) |
| Chinese paste | clipboard cmd | scrcpy SET_CLIPBOARD |

UI tree, shell, Wi-Fi adb still use **adb** even when Agent is active.

## Fallback

If ScrcpyMac is not running or Agent service is off, the plugin automatically uses **adb** — no configuration needed.

Check backend:

```
phone_backend
phone_doctor
```

## Workflow: automation + visual debug

1. Open ScrcpyMac + enable Agent service
2. Run automation in Cursor (e.g. send WeChat)
3. Watch the mirror window to verify each step

## API (for reference)

Agent HTTP API at `http://127.0.0.1:9477`:

- `GET /health` — service + connection status
- `GET /screenshot` — PNG
- `POST /tap` `{"x": 540, "y": 1200}`
- `POST /paste` `{"text": "你好"}`

Only bound to localhost.
