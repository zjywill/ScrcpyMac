# ScrcpyMac Phone Agent

Control your Android phone from **Cursor**, **Codex**, or **Claude** — one plugin install, no hunting for MCP configs.

## What's included

| Component | Description |
|-----------|-------------|
| **MCP server** | 24 tools: screenshot, calibrated taps, paste, UI tree, Wi-Fi adb, WeChat recipe, … |
| **Skills** | `phone-setup`, `wechat`, `android-nav`, `scrcpymac-link` |
| **Launcher** | `bin/phone-agent` + `mcp-server.sh` |
| **Scripts** | `install.sh`, `configure.sh`, `download-adb.sh`, `doctor.sh` |

## Requirements

- macOS 13+ (primary target)
- Python 3.10+
- Android 10+ with USB debugging
- `adb` auto-downloaded on install (or use system adb)

## Quick install

```bash
cd plugins/scrcpymac-phone-agent
./scripts/install.sh    # installs deps, downloads adb, links Cursor local plugin
```

### Cursor (local test)

`install.sh` symlinks the plugin automatically. Or manually:

```bash
./scripts/configure.sh
```

Reload Cursor → **Customize** → enable **scrcpymac-phone-agent** MCP.

### Codex

From the repository root:

```bash
codex plugin marketplace add .
# or: codex plugin marketplace add zjywill/scrcpyMac
codex plugin add scrcpymac-phone-agent@scrcpymac
```

Alternatively, open Codex → Plugins and install **ScrcpyMac Phone Agent**.

On the first MCP or `doctor` launch, the plugin creates its own `.venv` and
installs the Python dependencies there. It does not modify Homebrew, system, or
Conda Python environments. If adb is not already available, the first MCP
launch downloads the bundled platform-tools binary.

### Manual MCP config

`~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "scrcpymac-phone-agent": {
      "command": "/absolute/path/to/plugins/scrcpymac-phone-agent/mcp-server.sh",
      "args": []
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
| `phone_tap` | Tap native device pixels, then verify and retry nearby |
| `phone_tap_relative` | Tap normalized screenshot coordinates (`0...1`) |
| `phone_tap_image` | Map coordinates from a displayed/resized screenshot |
| `phone_swipe` | Swipe gesture |
| `phone_long_press` | Long press |
| `phone_key` | back / home / enter / … |
| `phone_type` | ASCII text only |
| `phone_paste` | Chinese + emoji via clipboard |
| `phone_launch_app` | Launch by package name |
| `phone_current_app` | Foreground app |
| `phone_ui_tree` | Accessibility tree with per-node `index` and state flags (`scrollable`, `enabled: false`, `focused`, `checked`…); `degraded: true` means the tree is incomplete (WebView/Compose) — fall back to `phone_screenshot` |
| `phone_find_and_tap` | Find element and tap. `require_all=true` ANDs the given selectors (e.g. `text` + `resource_id` to disambiguate); `exact=true` matches whole strings; `index=1` taps the 2nd match; `scroll_to_find=3` scrolls down up to 3 times for off-screen items |
| `phone_wait_for_text` | Wait for UI text |
| `phone_shell` | adb shell command |
| `phone_send_wechat` | Send WeChat message recipe |
| `phone_enable_wifi_adb` | Enable TCP/IP adb (USB required) |
| `phone_get_device_ip` | Device Wi-Fi IP |
| `phone_connect_wifi` | Connect over Wi-Fi |
| `phone_disconnect_wifi` | Disconnect Wi-Fi adb |

## Bundled adb

`install.sh` downloads Google platform-tools adb automatically on macOS/Linux.

Manual download:

```bash
./scripts/download-adb.sh darwin   # macOS
./scripts/download-adb.sh linux    # Linux dev
```

Placed at `bin/darwin/adb` (and arch mirrors). See `bin/darwin/README.md`.

## Environment variables

| Variable | Description |
|----------|-------------|
| `ADB_PATH` | Override adb binary |
| `PHONE_AGENT_SERIAL` | Target device serial |
| `PHONE_AGENT_ROOT` | Set automatically by launcher |
| `PHONE_AGENT_PYTHON` | Use an explicitly managed Python instead of the plugin `.venv` |
| `PHONE_AGENT_BOOTSTRAP_PYTHON` | Base Python used to create the plugin `.venv` |
| `PHONE_AGENT_AUTO_DOWNLOAD_ADB` | Set to `0` to disable first-launch adb download |

## Marketplace

See [MARKETPLACE.md](./MARKETPLACE.md) for Cursor/Codex submission checklist.

## ScrcpyMac App fast path (differentiator)

When **ScrcpyMac.app** is mirroring your phone with **Agent service** enabled:

1. Plugin auto-uses `http://127.0.0.1:9477` for screenshot/tap/paste/ui-tree (scrcpy speed, full-res frames)
2. `phone_doctor` shows `backend: scrcpymac-agent`
3. Shell / Wi-Fi adb still use adb

App sidebar (v0.4+): **Auto-enable on Connect**, **Install Phone Agent plugin** one-click setup.

Without the App, the plugin works standalone via adb.

## ScrcpyMac app

This plugin works standalone. For visual mirroring, use [ScrcpyMac](https://github.com/zjywill/scrcpyMac) alongside the agent.

## License

MIT — see LICENSE. adb is subject to Google Platform Tools terms when bundled.
