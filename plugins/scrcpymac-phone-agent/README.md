# ScrcpyMac Phone Agent

Control your Android phone from **Cursor**, **Codex**, or **Claude** — one plugin install, no hunting for MCP configs.

## What's included

| Component | Description |
|-----------|-------------|
| **MCP server** | 17 tools: screenshot, tap, paste, UI tree, WeChat recipe, … |
| **Skills** | `phone-setup`, `wechat`, `android-nav`, `scrcpymac-link` |
| **Launcher** | `bin/phone-agent` — single entry for MCP, doctor, devices |
| **Scripts** | `install.sh`, `doctor.sh` |

## Requirements

- macOS 13+ (primary target)
- Python 3.10+
- Android 10+ with USB debugging
- `adb` on PATH (or bundled adb in `bin/darwin/*/adb` — optional)

## Quick install

```bash
cd plugins/scrcpymac-phone-agent
./scripts/install.sh
```

### Cursor (local test)

```bash
ln -sf "$(pwd)" ~/.cursor/plugins/local/scrcpymac-phone-agent
```

Reload Cursor → **Customize** → enable **scrcpymac-phone-agent** MCP.

### Codex

From the repository root:

```bash
codex plugin marketplace add .
# or: codex plugin marketplace add zjywill/scrcpyMac
```

Open Codex → Plugins → install **ScrcpyMac Phone Agent**.

### Manual MCP config

`~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "scrcpymac-phone-agent": {
      "command": "/absolute/path/to/plugins/scrcpymac-phone-agent/bin/phone-agent",
      "args": ["mcp"]
    }
  }
}
```

## Usage examples

In Cursor / Codex chat:

```
Check my connected Android phone
Take a screenshot of my phone
Send a WeChat message to 张三: 明天见
Open Android Settings and take a screenshot
```

Or call tools directly: `phone_doctor`, `phone_screenshot`, `phone_send_wechat`.

## CLI

```bash
./bin/phone-agent doctor    # environment check
./bin/phone-agent devices   # list adb devices
./bin/phone-agent mcp       # start MCP server (stdio)
./bin/phone-agent version
```

## MCP tools

| Tool | Description |
|------|-------------|
| `phone_doctor` | Environment diagnostics |
| `phone_list_devices` | List adb devices |
| `phone_device_info` | Screen size + foreground app |
| `phone_screenshot` | PNG screenshot (+ image for vision) |
| `phone_tap` | Tap coordinates |
| `phone_swipe` | Swipe gesture |
| `phone_long_press` | Long press |
| `phone_key` | back / home / enter / … |
| `phone_type` | ASCII text only |
| `phone_paste` | Chinese + emoji via clipboard |
| `phone_launch_app` | Launch by package name |
| `phone_current_app` | Foreground app |
| `phone_ui_tree` | Accessibility tree |
| `phone_find_and_tap` | Find element and tap |
| `phone_wait_for_text` | Wait for UI text |
| `phone_shell` | adb shell command |
| `phone_send_wechat` | Send WeChat message recipe |

## Bundled adb (optional)

Place platform-tools `adb` at:

```
bin/darwin/arm64/adb
bin/darwin/x86_64/adb
```

The launcher prefers bundled adb when present. See `bin/darwin/README.md`.

## Environment variables

| Variable | Description |
|----------|-------------|
| `ADB_PATH` | Override adb binary |
| `PHONE_AGENT_SERIAL` | Target device serial |
| `PHONE_AGENT_ROOT` | Set automatically by launcher |

## ScrcpyMac app

This plugin works standalone. For visual mirroring, use [ScrcpyMac](https://github.com/zjywill/scrcpyMac) alongside the agent.

## License

MIT — see LICENSE. adb is subject to Google Platform Tools terms when bundled.
