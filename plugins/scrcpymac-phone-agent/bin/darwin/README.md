# Bundled adb (optional)

The plugin can ship macOS `adb` binaries here for zero-config setup.

## Layout

```
bin/darwin/
├── arm64/adb    # Apple Silicon
└── x86_64/adb   # Intel Mac
```

## How to add

1. Download [Android platform-tools](https://developer.android.com/tools/releases/platform-tools) for macOS
2. Copy `adb` into the arch folders above
3. `chmod +x bin/darwin/*/adb`

The launcher (`bin/phone-agent`) auto-detects architecture and sets `ADB_PATH`.

If no bundled adb is present, the plugin falls back to system adb
(Homebrew, Android SDK, or `$PATH`).

## Note

scrcpy desktop binary is **not** bundled. Phase 1 uses adb-only control.
See `docs/phone-agent-plugin-plan.md` for the scrcpy fast-path roadmap.
