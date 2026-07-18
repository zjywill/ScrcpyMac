import Foundation
import AVFoundation

// MARK: - ScrcpyMac Agent Service (localhost HTTP API for Phone Agent plugin)

extension ScrcpySession {
    private static let agentKeycodes: [String: Int32] = [
        "back": 4,
        "home": 3,
        "recents": 187,
        "enter": 66,
        "delete": 67,
        "tab": 61,
        "menu": 82,
        "power": 26,
    ]

    func agentHealth() -> [String: Any] {
        var info: [String: Any] = [
            "ok": true,
            "service": "scrcpymac-agent",
            "version": "0.4.0",
            "agent_running": AgentService.shared.isRunning,
        ]
        if case let .connected(meta) = state {
            info["connected"] = true
            info["serial"] = serial
            info["width"] = meta.width
            info["height"] = meta.height
            info["device_name"] = meta.deviceName
        } else {
            info["connected"] = false
        }
        return info
    }

    func agentDeviceInfo() -> [String: Any]? {
        guard case let .connected(meta) = state else { return nil }
        return [
            "serial": serial,
            "device_name": meta.deviceName,
            "screen": ["width": meta.width, "height": meta.height],
        ]
    }

    func captureScreenshotPNG() -> Data? {
        guard case .connected = state else { return nil }
        if let png = captureDecoderFramePNG() {
            return png
        }
        guard let layer = mirrorDisplayLayer else { return nil }
        return LayerSnapshot.png(from: layer)
    }

    func agentTap(x: Int32, y: Int32) {
        guard case let .connected(meta) = state else { return }
        let cx = clamp(x, max: Int32(meta.width - 1))
        let cy = clamp(y, max: Int32(meta.height - 1))
        let w = UInt16(meta.width), h = UInt16(meta.height)
        send(control: ControlMessage.injectTouch(
            action: .down, x: cx, y: cy,
            screenWidth: w, screenHeight: h, buttonsPressed: true
        ))
        send(control: ControlMessage.injectTouch(
            action: .up, x: cx, y: cy,
            screenWidth: w, screenHeight: h, buttonsPressed: false
        ))
    }

    func agentSwipe(x1: Int32, y1: Int32, x2: Int32, y2: Int32, durationMs: Int) {
        guard case let .connected(meta) = state else { return }
        let w = UInt16(meta.width), h = UInt16(meta.height)
        let steps = max(3, min(20, durationMs / 20))
        let cx1 = clamp(x1, max: Int32(meta.width - 1))
        let cy1 = clamp(y1, max: Int32(meta.height - 1))
        let cx2 = clamp(x2, max: Int32(meta.width - 1))
        let cy2 = clamp(y2, max: Int32(meta.height - 1))

        send(control: ControlMessage.injectTouch(
            action: .down, x: cx1, y: cy1,
            screenWidth: w, screenHeight: h, buttonsPressed: true
        ))
        for step in 1..<steps {
            let t = Float(step) / Float(steps)
            let mx = Int32(Float(cx1) + Float(cx2 - cx1) * t)
            let my = Int32(Float(cy1) + Float(cy2 - cy1) * t)
            send(control: ControlMessage.injectTouch(
                action: .move, x: mx, y: my,
                screenWidth: w, screenHeight: h, buttonsPressed: true
            ))
        }
        send(control: ControlMessage.injectTouch(
            action: .up, x: cx2, y: cy2,
            screenWidth: w, screenHeight: h, buttonsPressed: false
        ))
    }

    func agentKey(name: String) throws {
        guard case .connected = state else { return }
        let key = name.lowercased()
        guard let code = Self.agentKeycodes[key] else {
            throw AgentServiceError.unknownKey(name)
        }
        send(control: ControlMessage.injectKeycode(action: .down, keycode: code))
        send(control: ControlMessage.injectKeycode(action: .up, keycode: code))
    }

    private func clamp(_ value: Int32, max: Int32) -> Int32 {
        Swift.max(0, Swift.min(max, value))
    }
}
