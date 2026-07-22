# Changelog

## 0.7.2 — 2026-07-22

- Keep the plugin loopback video server alive for the lifetime of the MCP process
- Publish the exact loopback HTTP and WebSocket origins in the MCP App CSP before the Widget loads
- Prevent Codex from rejecting the H.264 transport and silently dropping to the sub-1 FPS JPEG compatibility path

## 0.7.1 — 2026-07-22

### Fixed

- Loopback bind failures now transition the standalone runtime to a clean error state
- MCP `SIGTERM` now stops the scrcpy process and removes its adb forward

### Changed

- ScrcpyMac.app removed its Agent HTTP service, plugin installer, plugin DMG payload,
  and Agent-only duplicate frame decoder so App and plugin are independent both ways

## 0.7.0 — 2026-07-22

### Added

- Standalone plugin-owned scrcpy 3.3.4 runtime and bundled scrcpy-server
- Continuous token-protected loopback H.264 stream for the Codex Widget
- WebCodecs canvas decoding with hardware preference and decode-backlog dropping
- 30/60 FPS and 50%/75%/100% stream controls
- Shared scrcpy control socket for Widget tap, swipe, navigation, and paste
- Explicit backend, encoding, measured FPS, resolution, and device lease status

### Changed

- ScrcpyMac.app is no longer consulted, launched, or required by the plugin
- JPEG screenshot polling is now compatibility fallback only
- Default phone stream is encoded on-device at approximately 50% resolution

### Removed

- ScrcpyMac.app Agent HTTP client and all `127.0.0.1:9477` runtime coupling

## 0.6.1 — 2026-07-22

### Added

- Low-latency Agent preview endpoint backed by the latest decoded H.264 pixel buffer
- Actual preview FPS in the Widget frame status

### Changed

- Preview frames default to 50% width (540px on a 1080px device) and fast JPEG encoding
- Widget polling targets a 100ms frame interval without adding delay after slow frames
- ADB fallback uses bilinear resize and skips expensive JPEG optimization

### Fixed

- Active previews now fully hide the stale “Ready to connect” empty state

## 0.6.0 — 2026-07-22

### Added

- Native Codex MCP App opened through `open_scrcpymac`
- Fullscreen ScrcpyMac workspace with device discovery and selection
- Interactive screenshot preview with tap and swipe coordinate mapping
- Widget controls for Android navigation, text paste, and Wi-Fi adb
- App-only structured tools so widget polling does not clutter model-visible tools
- Single-file Vite build embedded as `text/html;profile=mcp-app`
- MCP resource, metadata, frame compression, and packaging tests
- Runtime bootstrap skips broken Python installations and selects a usable Python 3.10+

### Changed

- Plugin product direction now treats the Codex widget as the primary ScrcpyMac UI
- Localhost Agent Service remains an optional fast path instead of a widget requirement

## 0.5.2 — 2026-07-20

### Fixed

- Codex now starts the MCP server from the plugin root instead of the active project directory
- Packaging tests verify both MCP manifests stay identical and include a portable working directory
- Touch release events now use zero pressure, matching the scrcpy 3.3.4 control protocol
- Mirror clicks account for aspect-fit letterboxing instead of mapping across black bars
- Visual taps can use normalized or resized-image coordinates and verify screen changes with nearby retries

## 0.5.1 — 2026-07-20

### Fixed

- Codex marketplace manifest now uses the supported `ON_INSTALL` authentication policy
- First MCP or doctor launch bootstraps dependencies into a plugin-local `.venv`
- Runtime setup no longer mutates or accidentally targets a different system Python
- Concurrent first launches share a lock and cannot race while creating the virtual environment
- Launcher finds bundled adb on both macOS and Linux and downloads it on first MCP use when needed
- Python package metadata now matches the plugin version

## 0.5.0 — 2026-07-19

### Added

- GitHub Actions CI: unit tests + `phone-agent doctor` smoke test
- Agent API `GET /foreground` — foreground app via session adb
- Screenshot response headers: `X-ScrcpyMac-Serial`, `X-ScrcpyMac-Width`, `X-ScrcpyMac-Height`
- Unit tests for `AgentClient` cache/TTL and `PhoneActions` UI tree cache

### Changed

- `phone_current_app` and `device_info.foreground` use Agent fast path when available
- `find_and_tap` / `wait_for_text` use exponential backoff between UI dumps
- UI tree cached until tap/swipe/key/paste invalidates it
- WeChat recipe waits for home/search markers; failures include last screenshot size

## 0.4.0 — 2026-07-18

### Added

- Agent API `GET /ui-tree` — accessibility XML via session adb
- App **Install Phone Agent plugin** button (runs bundled `install.sh`)
- App preference **Auto-enable on Connect** for Agent service
- Marketplace screenshot placeholders under `assets/screenshots/`

### Changed

- **Full-resolution screenshots** from H264 decoder (not mirror-layer scale)
- Agent tap/swipe/key/paste responses include `serial` (fewer HTTP roundtrips)
- `AgentClient` caches device info + availability with TTL; invalidates on failure
- DMG packaging bundles Phone Agent plugin alongside adb + scrcpy-server (B7)

## 0.3.0 — 2026-07-18

### Added

- **ScrcpyMac App Agent Service** integration (Phase 5)
- `AgentService` HTTP API in ScrcpyMac.app on `127.0.0.1:9477`
- Plugin `agent_client.py` — auto-prefers fast scrcpy path when App is connected
- `phone_backend` MCP tool
- App UI toggle: **Agent service** in sidebar when mirroring

### Changed

- Screenshot/tap/swipe/key/paste use ScrcpyMac Agent when available, else adb
- `scrcpymac-link` skill documents fast path setup

## 0.2.0 — 2026-07-18

### Added

- `scripts/download-adb.sh` — auto-download Google platform-tools adb (macOS/Linux)
- `scripts/configure.sh` — Cursor local plugin symlink + MCP config helper
- `mcp-server.sh` — stable MCP entrypoint relative to plugin root
- Wi-Fi adb tools: `phone_enable_wifi_adb`, `phone_get_device_ip`, `phone_connect_wifi`, `phone_disconnect_wifi`
- `PRIVACY.md`, `MARKETPLACE.md`, `assets/logo.svg`

### Changed

- `install.sh` downloads bundled adb on macOS/Linux when missing
- MCP config uses `./mcp-server.sh` instead of relative bash wrapper
- Version bumped to 0.2.0

## 0.1.0 — 2026-07-18

### Added

- Cursor + Codex dual plugin manifests
- MCP server with 17 adb-based tools
- Skills: phone-setup, wechat, android-nav, scrcpymac-link
- Unified launcher `bin/phone-agent`
- install.sh and doctor.sh scripts
- Codex marketplace entry at `.agents/plugins/marketplace.json`
