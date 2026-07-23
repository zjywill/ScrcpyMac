# Marketplace submission guide

Checklist for publishing **ScrcpyMac Phone Agent** to Cursor and Codex marketplaces.

## Pre-submission

- [ ] Run `./scripts/install.sh` on a clean Mac
- [ ] `./bin/phone-agent doctor` passes with a real device
- [ ] Test `phone_screenshot`, `phone_paste`, `phone_send_wechat` in Cursor
- [ ] Test a clean Codex `marketplace add` + `plugin add` with no runtime bootstrap
- [ ] Open `open_scrcpymac` in Codex and verify the fullscreen widget with a real device
- [ ] Verify H.264 is active at 30+ FPS with ScrcpyMac.app quit or uninstalled
- [ ] Verify 50%/75%/100% resolution and 30/60 FPS controls restart the stream
- [ ] Verify stop/teardown removes the plugin adb forward and child server
- [ ] Run `./scripts/build-ui.sh` and verify the committed single-file widget is unchanged
- [ ] Review [PRIVACY.md](./PRIVACY.md)
- [x] Placeholder screenshots in `assets/screenshots/` (replace with real 1280×800 captures before submit)

## Cursor Marketplace

1. Ensure repo is public and plugin lives at `plugins/scrcpymac-phone-agent/`
2. Manifest: `.cursor-plugin/plugin.json`
3. Submit: https://cursor.com/marketplace/publish
4. Local test before submit:

```bash
ln -sf "$(pwd)" ~/.cursor/plugins/local/scrcpymac-phone-agent
# Reload Cursor → Customize → verify skills + MCP
```

## Codex Marketplace

1. Repo includes `.agents/plugins/marketplace.json`
2. Plugin manifest: `.codex-plugin/plugin.json`
3. Users install:

```bash
codex plugin marketplace add zjywill/scrcpyMac
codex plugin add scrcpymac-phone-agent@scrcpymac
```

4. For OpenAI curated directory: follow https://developers.openai.com/codex/plugins/build

## Required metadata (included)

| Field | Location |
|-------|----------|
| `name` | plugin.json |
| `version` | plugin.json, CHANGELOG |
| `description` | plugin.json |
| `privacyPolicyURL` | `.codex-plugin/plugin.json` → `interface.privacyPolicyURL` |
| `logo` | `assets/logo.svg` |
| `skills` | `skills/*/SKILL.md` |
| `mcpServers` | `mcp.json` → `mcp-server.sh` |
| MCP App UI | `ui/` source → `go/internal/widget/assets/scrcpymac-app.html` |
| Native runtime | `bin/darwin/{arm64,x86_64}/phone-agent` |

## Version bump workflow

1. Update `go/internal/version/version.go` and `ui/package.json`
2. Update `.cursor-plugin/plugin.json`, `.codex-plugin/plugin.json`, and marketplace metadata
3. Run `./scripts/build-ui.sh` and `./scripts/build-go.sh`
4. Add entry to `CHANGELOG.md`
5. Tag release: `phone-agent-v0.7.2`
