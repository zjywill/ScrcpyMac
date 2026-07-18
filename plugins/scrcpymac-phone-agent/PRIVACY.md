# Privacy Policy — ScrcpyMac Phone Agent

**Last updated:** 2026-07-18

## Summary

ScrcpyMac Phone Agent runs **entirely on your computer**. It does not send your phone screen, messages, or personal data to ScrcpyMac servers.

## What runs locally

- **adb** communication with your Android device over USB or Wi-Fi
- **MCP server** (Python) started by Cursor, Codex, or Claude on your machine
- **Screenshots and UI trees** passed only to the AI client you configured (e.g. Cursor)

## What we do not collect

- No analytics or telemetry in the plugin (v0.2)
- No cloud account required
- No upload of screen content to third-party servers by this plugin

## Third parties

When you use Cursor, Codex, or Claude, their privacy policies apply to how your AI provider handles tool outputs (including screenshots you authorize the agent to capture).

## Permissions

The plugin requires:

- USB debugging on your Android device (your explicit consent on the phone)
- Local execution of `adb` and Python on your Mac

## Contact

Issues and source code: https://github.com/zjywill/scrcpyMac
