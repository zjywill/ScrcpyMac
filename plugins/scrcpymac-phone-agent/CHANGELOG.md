# Changelog

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
