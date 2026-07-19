# Bundled adb

The plugin downloads Google **platform-tools** adb on install.

## Layout (after `scripts/download-adb.sh`)

```
bin/darwin/adb              # macOS universal binary
bin/darwin/arm64/adb        # mirror (same file)
bin/darwin/x86_64/adb       # mirror (same file)
bin/linux/x86_64/adb        # Linux dev/CI
```

## Download manually

```bash
./scripts/download-adb.sh darwin
./scripts/download-adb.sh linux
```

The launcher (`bin/phone-agent`) prefers bundled adb when `PHONE_AGENT_ROOT` is set.

Override with `ADB_PATH` or install system platform-tools.

## Note

scrcpy desktop binary is **not** bundled. Phase 1 uses adb-only control.
