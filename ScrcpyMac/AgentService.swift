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
        try server.start()
        self.server = server
        isRunning = true
    }

    func stop() {
        server?.stop()
        server = nil
        isRunning = false
    }

    private static func route(request: AgentHTTPRequest, session: ScrcpySession?) async -> AgentHTTPResponse {
        await MainActor.run {
            guard let session else {
                return .error(503, "no session attached")
            }
            switch (request.method, request.path) {
            case ("GET", "/health"), ("GET", "/"):
                return health(session: session)
            case ("GET", "/device"):
                return device(session: session)
            case ("GET", "/screenshot"):
                return screenshot(session: session)
            case ("POST", "/tap"):
                return tap(session: session, body: request.body)
            case ("POST", "/swipe"):
                return swipe(session: session, body: request.body)
            case ("POST", "/key"):
                return key(session: session, body: request.body)
            case ("POST", "/paste"):
                return paste(session: session, body: request.body)
            default:
                return .error(404, "not found")
            }
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
        return .json(200, object: ["ok": true] + info)
    }

    private static func screenshot(session: ScrcpySession) -> AgentHTTPResponse {
        guard let png = session.captureScreenshotPNG() else {
            return .error(503, "screenshot unavailable")
        }
        return .png(png)
    }

    private static func tap(session: ScrcpySession, body: Data) -> AgentHTTPResponse {
        guard let json = parseJSON(body),
              let x = json["x"] as? Int,
              let y = json["y"] as? Int else {
            return .error(400, "expected JSON {x, y}")
        }
        session.agentTap(x: Int32(x), y: Int32(y))
        return .json(200, object: ["ok": true, "action": "tap", "x": x, "y": y])
    }

    private static func swipe(session: ScrcpySession, body: Data) -> AgentHTTPResponse {
        guard let json = parseJSON(body),
              let x1 = json["x1"] as? Int,
              let y1 = json["y1"] as? Int,
              let x2 = json["x2"] as? Int,
              let y2 = json["y2"] as? Int else {
            return .error(400, "expected JSON {x1,y1,x2,y2}")
        }
        let duration = json["duration_ms"] as? Int ?? 300
        session.agentSwipe(
            x1: Int32(x1), y1: Int32(y1),
            x2: Int32(x2), y2: Int32(y2),
            durationMs: duration
        )
        return .json(200, object: [
            "ok": true, "action": "swipe",
            "from": [x1, y1], "to": [x2, y2], "duration_ms": duration,
        ])
    }

    private static func key(session: ScrcpySession, body: Data) -> AgentHTTPResponse {
        guard let json = parseJSON(body),
              let name = json["name"] as? String else {
            return .error(400, "expected JSON {name}")
        }
        do {
            try session.agentKey(name: name)
            return .json(200, object: ["ok": true, "action": "key", "name": name])
        } catch {
            return .error(400, error.localizedDescription)
        }
    }

    private static func paste(session: ScrcpySession, body: Data) -> AgentHTTPResponse {
        guard let json = parseJSON(body),
              let text = json["text"] as? String, !text.isEmpty else {
            return .error(400, "expected JSON {text}")
        }
        session.pasteClipboardText(text)
        return .json(200, object: ["ok": true, "action": "paste", "length": text.count])
    }

    private static func parseJSON(_ body: Data) -> [String: Any]? {
        guard !body.isEmpty else { return [:] }
        return (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
    }
}

enum AgentServiceError: Error, LocalizedError {
    case unknownKey(String)

    var errorDescription: String? {
        switch self {
        case .unknownKey(let name): return "unknown key: \(name)"
        }
    }
}
