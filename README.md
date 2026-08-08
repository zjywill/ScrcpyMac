<p align="center">
  <img src="ScrcpyMac/Assets.xcassets/AppIcon.appiconset/icon_256x256.png" width="128" alt="ScrcpyMac icon">
</p>

<h1 align="center">ScrcpyMac</h1>

<p align="center">
  Mirror and control your Android phone from your Mac.
</p>

---

ScrcpyMac puts your phone's screen in a native Mac window. Click to tap, scroll
with your trackpad, type with your Mac keyboard, paste from your Mac clipboard.

## What can you do with it?

**Use your phone without picking it up**

- Low-latency screen mirroring with audio, in a normal Mac window
- Mouse click = tap, drag = swipe, trackpad scrolling just works
- Type into phone apps with your Mac keyboard
- Paste your Mac clipboard straight into the phone — Chinese and emoji included
- Go wireless: one click switches a USB-connected phone to Wi-Fi mirroring
- Privacy modes: keep the phone screen off while mirroring, keep it awake, or
  stream audio-only / video-only

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

Or build a shareable DMG (includes adb and scrcpy-server):

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

## Want your phone in an AI client instead?

**ScrcpyMac Phone Agent** is a separate MCP plugin that opens the same phone
stream inside Codex, Cursor, or Claude and lets a model drive it — taps,
screenshots, UI automation, WeChat workflows. It lives in its own repository:

**→ [zjywill/scrcpy-phone-agent](https://github.com/zjywill/scrcpy-phone-agent)**

```sh
codex plugin marketplace add zjywill/scrcpy-phone-agent
```

The plugin owns its own scrcpy-server, H.264 stream, control socket, and
lifecycle. It does not call, launch, install from, or share a session with this
App — installing one has no effect on the other.

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| Device shows *unauthorized* | Unlock the phone and accept the USB-debugging prompt |
| No devices listed | Check the cable supports data, and USB debugging is on |
| Gatekeeper blocks the app | Right-click → Open (first launch only), or `xattr -cr` |
| Wi-Fi connect fails | Keep USB plugged in until the switch completes; phone and Mac must share a network |
| Chinese text won't type | Use the App's Paste Clipboard command instead of key input |

## Under the hood

A native SwiftUI implementation of the scrcpy wire protocol — VideoToolbox
H.264 decoding into an `AVSampleBufferDisplayLayer`, no SDL/ffmpeg, no desktop
scrcpy binary. Curious? See [docs/architecture.md](docs/architecture.md).

## License

[MIT](LICENSE).

ScrcpyMac reuses scrcpy's server jar (Apache-2.0) on the Android side but does
not link against the scrcpy desktop client. The Swift client here is an
independent implementation of the scrcpy wire protocol.
