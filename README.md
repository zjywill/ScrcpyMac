<p align="center">
  <img src="ScrcpyMac/Assets.xcassets/AppIcon.appiconset/icon_256x256.png" width="128" alt="ScrcpyMac icon">
</p>

<h1 align="center">ScrcpyMac</h1>

<p align="center">
  Mirror and control your Android phone from your Mac — and let AI agents drive it for you.
</p>

---

ScrcpyMac puts your phone's screen in a native Mac window. Click to tap, scroll
with your trackpad, type with your Mac keyboard, paste from your Mac clipboard.
Then go one step further: flip on the built-in **Agent service** and your AI
coding assistant (Claude Code, Cursor, or Codex) can see and operate the phone
too — in plain language.

## What can you do with it?

**Use your phone without picking it up**

- Low-latency screen mirroring with audio, in a normal Mac window
- Mouse click = tap, drag = swipe, trackpad scrolling just works
- Type into phone apps with your Mac keyboard
- Paste your Mac clipboard straight into the phone — Chinese and emoji included
- Go wireless: one click switches a USB-connected phone to Wi-Fi mirroring
- Privacy modes: keep the phone screen off while mirroring, keep it awake, or
  stream audio-only / video-only

**Let an AI agent use your phone**

The bundled **Phone Agent plugin** exposes 24 phone-control tools (screenshot,
tap-by-text, UI tree, app launch, clipboard paste, WeChat recipe, …) over MCP.
Tell your assistant things like:

```
Take a screenshot of my phone
Open Settings and turn on airplane mode
Send a WeChat message to 张三: 明天见
Open my app, go to the assets page, and check if the history list is empty
```

Real example: this repo's tooling was used to verify a bug report end-to-end —
the agent launched the app, navigated to the page, walked every date filter,
and read the API responses from logcat to prove the backend was returning empty
data. Your phone becomes something the AI can test on.

## Getting started

**You need**

- macOS 13 or newer
- An Android 10+ phone with **USB debugging** enabled
  (Settings → About phone → tap *Build number* 7 times → Developer options → USB debugging)

**Build and run**

```sh
brew install xcodegen        # one-time
xcodegen generate
open ScrcpyMac.xcodeproj     # ⌘R to run
```

Or build a shareable DMG (includes adb, scrcpy-server, and the plugin):

```sh
./scripts/package-dmg.sh     # output: dist/ScrcpyMac-<version>.dmg
```

> Self-built DMGs are not notarized — first launch needs right-click → Open,
> or `xattr -cr /Applications/ScrcpyMac.app`.

**Connect your phone**

1. Plug in via USB and accept the "Allow USB debugging?" prompt on the phone.
2. Open ScrcpyMac, pick your device in the sidebar, press **Connect**.
3. That's it — the mirror window appears. Use **Paste Clipboard** in the
   toolbar to send your Mac clipboard to the phone.

**Go wireless (optional)**

- With the phone still on USB, click **Switch selected device to Wi-Fi** —
  ScrcpyMac enables TCP mode and reconnects wirelessly. Unplug when done.
- Or connect directly to a device already in TCP mode by entering its
  `ip:port`.

## Let your AI assistant control the phone

1. In the ScrcpyMac sidebar, enable **Agent service** (`127.0.0.1:9477`) — or
   turn on *Auto-enable on Connect*. This gives agents fast screenshots and
   input through the live mirror session. (Without it, the plugin falls back
   to plain adb — slower, but works even when the app is closed.)
2. Install the plugin:

   ```sh
   cd plugins/scrcpymac-phone-agent
   ./scripts/install.sh
   ```

   - **Cursor**: `install.sh` links it automatically — reload and enable
     *scrcpymac-phone-agent* under MCP.
   - **Codex**: `codex plugin marketplace add zjywill/scrcpyMac`, then install
     *ScrcpyMac Phone Agent* from Plugins.
   - **Claude Code / anything MCP**: point your MCP config at
     `plugins/scrcpymac-phone-agent/mcp-server.sh`.

3. Ask for something: *"Check my connected Android phone"* → the agent runs
   `phone_doctor` and takes it from there.

Full tool list, skills, and troubleshooting:
[plugins/scrcpymac-phone-agent/README.md](plugins/scrcpymac-phone-agent/README.md)

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| Device shows *unauthorized* | Unlock the phone and accept the USB-debugging prompt |
| No devices listed | Check the cable supports data, and USB debugging is on |
| Gatekeeper blocks the app | Right-click → Open (first launch only), or `xattr -cr` |
| Wi-Fi connect fails | Keep USB plugged in until the switch completes; phone and Mac must share a network |
| Chinese text won't type | Use clipboard paste (`phone_paste` / Paste Clipboard) instead of key input |

## Under the hood

A native SwiftUI implementation of the scrcpy wire protocol — VideoToolbox
H.264 decoding into an `AVSampleBufferDisplayLayer`, no SDL/ffmpeg, no desktop
scrcpy binary. Curious? See [docs/architecture.md](docs/architecture.md).

## License

[MIT](LICENSE).

ScrcpyMac reuses scrcpy's server jar (Apache-2.0) on the Android side but does
not link against the scrcpy desktop client. The Swift client here is an
independent implementation of the scrcpy wire protocol.
