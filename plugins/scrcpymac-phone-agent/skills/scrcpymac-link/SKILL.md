---
name: scrcpymac-link
description: Relate ScrcpyMac Phone Agent plugin to the ScrcpyMac mirror app. Use when the user mentions ScrcpyMac, wants to see the phone screen visually, or asks about mirror vs agent mode.
---

# ScrcpyMac Link

## Two modes

| Mode | What | When |
|------|------|------|
| **Phone Agent plugin** | Headless MCP tools via adb | Codex / Claude / Cursor automation |
| **ScrcpyMac.app** | Visual mirror + manual control | User wants to see and touch the screen |

They complement each other. The plugin does not require the app.

## Recommendations

- **Automation** (send WeChat, batch tasks): use Phone Agent MCP tools
- **Visual debugging**: open ScrcpyMac.app to watch what the agent is doing
- **First setup**: use ScrcpyMac or `phone_doctor` to confirm adb works

## ScrcpyMac app

- Repo: https://github.com/zjywill/scrcpyMac
- Native macOS SwiftUI mirror using scrcpy protocol
- USB and Wi-Fi adb supported

## Future: local Agent Service (planned)

ScrcpyMac may expose a local socket for faster screenshots and input. When available, the plugin will prefer that path and fall back to adb.

## Install plugin locally (Cursor)

```bash
ln -sf /path/to/plugins/scrcpymac-phone-agent ~/.cursor/plugins/local/scrcpymac-phone-agent
```

Then reload Cursor and enable the MCP server in Customize.
