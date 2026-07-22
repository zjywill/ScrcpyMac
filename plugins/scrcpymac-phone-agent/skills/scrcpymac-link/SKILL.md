---
name: scrcpymac-link
description: Explain the standalone ScrcpyMac Phone Agent runtime and how it differs from the separate ScrcpyMac mirror app.
---

# ScrcpyMac Link

The Codex plugin and `ScrcpyMac.app` are separate products. The plugin does not
launch, call, or require the App.

## Plugin runtime

The plugin owns:

- adb discovery, push, and port forwarding
- bundled scrcpy-server 3.3.4
- continuous H.264 video socket
- scrcpy control socket
- token-protected loopback WebSocket
- Codex WebCodecs canvas
- teardown of child processes and adb forwards

Open `open_scrcpymac`, choose a device, and start the preview. `phone_doctor`
should report `plugin-h264-ready`; the active Widget reports `plugin-h264`.

## Streaming modes

| Mode | Use |
|------|-----|
| H.264 | Default continuous preview, 30/60 FPS, 50%/75%/100% resolution |
| JPEG | Compatibility fallback when WebCodecs or loopback streaming is unavailable |

Tap, swipe, navigation, and paste use the plugin's control socket while H.264 is
active. Accessibility trees, shell commands, Wi-Fi setup, and explicit model
screenshots continue to use adb.

## Separate App

`ScrcpyMac.app` can still be used independently as a native mirror. Starting or
stopping it does not select a plugin backend and is not part of plugin setup.

Check backend:

```
phone_backend
phone_doctor
```
