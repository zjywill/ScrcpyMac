# Architecture

A native SwiftUI Mac app that mirrors an Android device by speaking the
[scrcpy](https://github.com/Genymobile/scrcpy) server protocol directly — no
bundled desktop scrcpy binary, no SDL, no ffmpeg. Only the `scrcpy-server.jar`
(pushed to the device) and `adb` are reused.

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

## Key files

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

## Milestones

- [x] B1 — clean SwiftUI skeleton
- [x] B2 — adb push / forward / shell launch of scrcpy-server; TCP handshake
      (device name, resolution, codec)
- [x] B3 — H.264 decode + render via `AVSampleBufferDisplayLayer` inside
      SwiftUI (single window, single Dock icon)
- [x] B4 — mouse + scroll events encoded as scrcpy control messages
- [x] B5 — keyboard input
- [x] B6 — stop / reconnect hardening, clipboard paste, and minimal raw PCM audio playback
- [x] B7 — bundle `adb` + `scrcpy-server.jar` into the `.app` DMG (`scripts/package-dmg.sh`)

## Codex plugin boundary

The Phone Agent Codex plugin is a separate runtime. It independently starts its
own scrcpy-server and sockets and does not call this App, expose an App HTTP
service, or ship inside the App DMG. The two products may target the same device
at different times, but they do not share process state or protocol sessions.

See [mcp-apps-scrcpy-integration-plan.md](./mcp-apps-scrcpy-integration-plan.md) for the
plugin's independent H.264/WebCodecs architecture.
