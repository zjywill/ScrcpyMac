# Changelog

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
