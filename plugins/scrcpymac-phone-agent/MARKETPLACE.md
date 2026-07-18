# Marketplace submission guide

Checklist for publishing **ScrcpyMac Phone Agent** to Cursor and Codex marketplaces.

## Pre-submission

- [ ] Run `./scripts/install.sh` on a clean Mac
- [ ] `./bin/phone-agent doctor` passes with a real device
- [ ] Test `phone_screenshot`, `phone_paste`, `phone_send_wechat` in Cursor
- [ ] Test plugin install via Codex `marketplace add`
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

## Version bump workflow

1. Update `server/phone_agent/__init__.py`
2. Update `.cursor-plugin/plugin.json` and `.codex-plugin/plugin.json`
3. Add entry to `CHANGELOG.md`
4. Tag release: `phone-agent-v0.2.0`
