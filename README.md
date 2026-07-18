# ScrcpyMac

A native SwiftUI Mac app that mirrors an Android device by speaking the
[scrcpy](https://github.com/Genymobile/scrcpy) server protocol directly — no
bundled desktop scrcpy binary, no SDL, no ffmpeg. Only the `scrcpy-server.jar`
(pushed to the device) and `adb` are reused.

## Architecture

```
┌─────────────────────────────────────────────┐
│ ScrcpyMac.app (SwiftUI)                     │
│                                             │
│ ┌──────────┐    ┌────────────────────────┐  │
│ │ Controls │    │ AVSampleBufferDisplay- │  │
│ │ + state  │    │ Layer (inside an NSView│  │
│ │          │    │ inside SwiftUI)        │  │
│ └──────────┘    └────────────────────────┘  │
│      │                     ▲                │
│      │ ControlMessage      │ CMSampleBuffer │
│      │ (touch/scroll)      │                │
│      │                     │ H264Decoder    │
│      ▼                     │ (VideoToolbox) │
│ NWConnection (control)     NWConnection     │
│      │                   (video)            │
└──────┼─────────────────────┼────────────────┘
       │   adb forward       │
       └──────────┬──────────┘
                  ▼
         scrcpy-server.jar on device
         (H.264 via MediaCodec)
```

Key files:

- `ScrcpySession.swift` — lifecycle (push jar, adb forward, launch server,
  handshake, frame pump).
- `ScrcpyProtocol.swift` — wire-format constants + handshake / frame-header
  parsers for scrcpy v3.3.4.
- `H264Decoder.swift` — Annex-B → AVCC → `CMSampleBuffer`, fed to the display
  layer; format description built from the config packet's SPS+PPS.
- `MirrorView.swift` — `NSViewRepresentable` hosting
  `AVSampleBufferDisplayLayer`; also the capture surface for mouse+scroll.
- `ControlMessage.swift` — encoder for scrcpy's client→device protocol
  (touch, scroll, keyboard, and text injection).
- `AdbBridge.swift` — thin wrapper over the `adb` CLI.

## Build

```sh
brew install xcodegen                # one-time
xcodegen generate                    # produces ScrcpyMac.xcodeproj
open ScrcpyMac.xcodeproj             # ⌘R to run, or use xcodebuild
```

## Requirements

- macOS 13+
- `adb` at `~/Library/Android/sdk/platform-tools/adb` or on `$PATH`
- `scrcpy-server.jar` (taken from Homebrew's `scrcpy` install at
  `/opt/homebrew/share/scrcpy/scrcpy-server` during development; bundling
  inside the `.app` for distribution is TODO)

## Status

- [x] B1 — clean SwiftUI skeleton
- [x] B2 — adb push / forward / shell launch of scrcpy-server; TCP handshake
      (device name, resolution, codec)
- [x] B3 — H.264 decode + render via `AVSampleBufferDisplayLayer` inside
      SwiftUI (single window, single Dock icon)
- [x] B4 — mouse + scroll events encoded as scrcpy control messages
- [x] B5 — keyboard input
- [x] B6 — stop / reconnect hardening, clipboard paste, and minimal raw PCM audio playback
- [ ] B7 — bundle `adb` + `scrcpy-server.jar` into the `.app` for sharing

## Phone Agent plugin

Use your phone from Cursor, Codex, or Claude:

```sh
cd plugins/scrcpymac-phone-agent
./scripts/install.sh
```

See [plugins/scrcpymac-phone-agent/README.md](plugins/scrcpymac-phone-agent/README.md) and
[docs/phone-agent-plugin-plan.md](docs/phone-agent-plugin-plan.md).

## License notes

This project reuses scrcpy's server jar (Apache-2.0) on the Android side but
does not link against the scrcpy desktop client. The Swift client here is an
independent implementation of the scrcpy wire protocol.
