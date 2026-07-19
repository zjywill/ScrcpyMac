import Foundation

/// Routes localhost HTTP requests to the active `ScrcpySession`.
@MainActor
final class AgentService: ObservableObject {
    static let shared = AgentService()

    @Published private(set) var isRunning = false

    private var server: AgentHTTPServer?
    private weak var session: ScrcpySession?

    private init() {}

    func attach(session: ScrcpySession) {
        self.session = session
    }

    func start() throws {
        guard !isRunning else { return }
        let server = AgentHTTPServer { [weak self] request in
            await AgentService.route(request: request, session: self?.session)
        }
        server.onFailure = { [weak self] in
            Task { @MainActor in self?.stop() }
        }
        try server.start()
        self.server = server
        isRunning = true
        session?.setAgentCaptureEnabled(true)
    }

    func stop() {
        server?.stop()
        server = nil
        isRunning = false
        session?.setAgentCaptureEnabled(false)
    }

    private static func route(request: AgentHTTPRequest, session: ScrcpySession?) async -> AgentHTTPResponse {
        switch (request.method, request.path) {
        case ("GET", "/health"), ("GET", "/"):
            return await MainActor.run {
                guard let session else { return .error(503, "no session attached") }
                return health(session: session)
            }
        case ("GET", "/device"):
            return await MainActor.run {
                guard let session else { return .error(503, "no session attached") }
                return device(session: session)
            }
        case ("GET", "/screenshot"):
            return await screenshot(session: session)
        case ("GET", "/ui-tree"):
            return await uiTree(session: session)
        case ("GET", "/foreground"):
            return await foreground(session: session)
        case ("POST", "/tap"):
            return await MainActor.run {
                guard let session else { return .error(503, "no session attached") }
                return tap(session: session, body: request.body)
            }
        case ("POST", "/swipe"):
            return await swipe(session: session, body: request.body)
        case ("POST", "/key"):
            return await MainActor.run {
                guard let session else { return .error(503, "no session attached") }
                return key(session: session, body: request.body)
            }
        case ("POST", "/paste"):
            return await MainActor.run {
                guard let session else { return .error(503, "no session attached") }
                return paste(session: session, body: request.body)
            }
        default:
            return .error(404, "not found")
        }
    }

    private static func health(session: ScrcpySession) -> AgentHTTPResponse {
        let info = session.agentHealth()
        return .json(200, object: info)
    }

    private static func device(session: ScrcpySession) -> AgentHTTPResponse {
        guard let info = session.agentDeviceInfo() else {
            return .error(503, "not connected")
        }
        return .json(200, object: info.merging(["ok": true]) { current, _ in current })
    }

    private static func screenshot(session: ScrcpySession?) async -> AgentHTTPResponse {
        guard let session else { return .error(503, "no session attached") }
        guard let meta = await MainActor.run(body: { session.screenshotMetadata() }) else {
            return .error(503, "not connected")
        }
        // Encode the native-resolution frame off the main actor so agent
        // screenshot loops don't freeze the UI. Fall back to a main-actor
        // layer snapshot only when the decoder cache is empty.
        var png = session.captureDecoderFramePNG()
        if png == nil {
            png = await MainActor.run(body: { session.captureLayerSnapshotPNG() })
        }
        guard let png else {
            return .error(503, "screenshot unavailable")
        }
        return .png(png, headers: [
            "X-ScrcpyMac-Serial": meta.serial,
            "X-ScrcpyMac-Width": String(meta.width),
            "X-ScrcpyMac-Height": String(meta.height),
        ])
    }

    private static func foreground(session: ScrcpySession?) async -> AgentHTTPResponse {
        guard let session else { return .error(503, "no session attached") }
        do {
            let app = try await session.agentForegroundApp()
            var object: [String: Any] = ["ok": true, "foreground": app]
            if let serial = await MainActor.run(body: { session.connectedSerial }) {
                object["serial"] = serial
            }
            return .json(200, object: object)
        } catch AgentServiceError.notConnected {
            return .error(503, "not connected")
        } catch {
            return .error(500, error.localizedDescription)
        }
    }

    private static func uiTree(session: ScrcpySession?) async -> AgentHTTPResponse {
        guard let session else { return .error(503, "no session attached") }
        do {
            let xml = try await session.agentUITreeXML()
            var object: [String: Any] = ["ok": true, "xml": xml]
            if let serial = await MainActor.run(body: { session.connectedSerial }) {
                object["serial"] = serial
            }
            return .json(200, object: object)
        } catch AgentServiceError.notConnected {
            return .error(503, "not connected")
        } catch {
            return .error(500, error.localizedDescription)
        }
    }

    private static func tap(session: ScrcpySession, body: Data) -> AgentHTTPResponse {
        guard let json = parseJSON(body),
              let x = int32Value(json["x"]),
              let y = int32Value(json["y"]) else {
            return .error(400, "expected JSON {x, y} with in-range integers")
        }
        guard session.agentTap(x: x, y: y) else {
            return .error(503, "not connected")
        }
        return actionResponse(session: session, action: "tap", extra: ["x": Int(x), "y": Int(y)])
    }

    private static func swipe(session: ScrcpySession?, body: Data) async -> AgentHTTPResponse {
        guard let session else { return .error(503, "no session attached") }
        guard let json = parseJSON(body),
              let x1 = int32Value(json["x1"]),
              let y1 = int32Value(json["y1"]),
              let x2 = int32Value(json["x2"]),
              let y2 = int32Value(json["y2"]) else {
            return .error(400, "expected JSON {x1,y1,x2,y2} with in-range integers")
        }
        let duration = min(max(json["duration_ms"] as? Int ?? 300, 0), 10_000)
        guard await session.agentSwipe(x1: x1, y1: y1, x2: x2, y2: y2, durationMs: duration) else {
            return .error(503, "not connected")
        }
        return actionResponse(
            session: session,
            action: "swipe",
            extra: ["from": [Int(x1), Int(y1)], "to": [Int(x2), Int(y2)], "duration_ms": duration]
        )
    }

    private static func int32Value(_ value: Any?) -> Int32? {
        guard let intValue = value as? Int else { return nil }
        return Int32(exactly: intValue)
    }

    private static func key(session: ScrcpySession, body: Data) -> AgentHTTPResponse {
        guard let json = parseJSON(body),
              let name = json["name"] as? String else {
            return .error(400, "expected JSON {name}")
        }
        do {
            try session.agentKey(name: name)
            return actionResponse(session: session, action: "key", extra: ["name": name])
        } catch AgentServiceError.notConnected {
            return .error(503, "not connected")
        } catch {
            return .error(400, error.localizedDescription)
        }
    }

    private static func paste(session: ScrcpySession, body: Data) -> AgentHTTPResponse {
        guard let json = parseJSON(body),
              let text = json["text"] as? String, !text.isEmpty else {
            return .error(400, "expected JSON {text}")
        }
        guard session.connectedSerial != nil else {
            return .error(503, "not connected")
        }
        session.pasteClipboardText(text)
        return actionResponse(session: session, action: "paste", extra: ["length": text.count])
    }

    private static func actionResponse(
        session: ScrcpySession,
        action: String,
        extra: [String: Any]
    ) -> AgentHTTPResponse {
        var object: [String: Any] = ["ok": true, "action": action]
        for (key, value) in extra {
            object[key] = value
        }
        if let serial = session.connectedSerial {
            object["serial"] = serial
        }
        return .json(200, object: object)
    }

    private static func parseJSON(_ body: Data) -> [String: Any]? {
        guard !body.isEmpty else { return [:] }
        return (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
    }
}

enum AgentServiceError: Error, LocalizedError {
    case unknownKey(String)
    case notConnected

    var errorDescription: String? {
        switch self {
        case .unknownKey(let name): return "unknown key: \(name)"
        case .notConnected: return "not connected"
        }
    }
}
