import Foundation
import Network

struct AgentHTTPRequest {
    var method: String
    var path: String
    var body: Data
}

struct AgentHTTPResponse {
    var status: Int
    var contentType: String
    var body: Data
    var extraHeaders: [String: String] = [:]

    static func json(_ status: Int, object: [String: Any]) -> AgentHTTPResponse {
        let data = (try? JSONSerialization.data(withJSONObject: object, options: [])) ?? Data()
        return AgentHTTPResponse(status: status, contentType: "application/json; charset=utf-8", body: data)
    }

    static func png(_ data: Data, headers: [String: String] = [:]) -> AgentHTTPResponse {
        AgentHTTPResponse(status: 200, contentType: "image/png", body: data, extraHeaders: headers)
    }

    static func error(_ status: Int, message: String) -> AgentHTTPResponse {
        .json(status, object: ["ok": false, "error": message])
    }
}

/// Minimal HTTP/1.1 server for localhost agent control (127.0.0.1).
final class AgentHTTPServer {
    static let defaultPort: UInt16 = 9477
    /// Upper bounds on untrusted input so a local client cannot exhaust memory.
    static let maxHeaderBytes = 65_536
    static let maxBodyBytes = 1_048_576

    private let port: UInt16
    private let handler: @Sendable (AgentHTTPRequest) async -> AgentHTTPResponse
    /// Invoked when the listener fails after start (e.g. port already in use).
    var onFailure: (@Sendable () -> Void)?
    private var listener: NWListener?
    private let queue = DispatchQueue(label: "com.zjywill.scrcpyMac.agent-http")

    init(
        port: UInt16 = AgentHTTPServer.defaultPort,
        handler: @escaping @Sendable (AgentHTTPRequest) async -> AgentHTTPResponse
    ) {
        self.port = port
        self.handler = handler
    }

    func start() throws {
        guard listener == nil else { return }
        let params = NWParameters.tcp
        params.allowLocalEndpointReuse = true
        params.requiredInterfaceType = .loopback
        let nwPort = NWEndpoint.Port(rawValue: port)!
        let listener = try NWListener(using: params, on: nwPort)
        listener.newConnectionHandler = { [weak self] connection in
            self?.handle(connection: connection)
        }
        listener.stateUpdateHandler = { [weak self] state in
            if case .failed(let err) = state {
                NSLog("[agent] listener failed: %@", "\(err)")
                self?.onFailure?()
            }
        }
        listener.start(queue: queue)
        self.listener = listener
        NSLog("[agent] listening on 127.0.0.1:%d", port)
    }

    func stop() {
        listener?.cancel()
        listener = nil
    }

    private func handle(connection: NWConnection) {
        connection.start(queue: queue)
        receive(on: connection, accumulated: Data())
    }

    private func receive(on connection: NWConnection, accumulated: Data) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 1_048_576) { [weak self] data, _, isComplete, error in
            guard let self else {
                connection.cancel()
                return
            }
            if let error {
                NSLog("[agent] receive error: %@", "\(error)")
                connection.cancel()
                return
            }
            var buffer = accumulated
            if let data { buffer.append(data) }

            guard let headerEnd = buffer.range(of: Data([13, 10, 13, 10])) else {
                if buffer.count > Self.maxHeaderBytes {
                    self.send(connection: connection, response: .error(431, "headers too large"))
                    return
                }
                if isComplete {
                    connection.cancel()
                    return
                }
                self.receive(on: connection, accumulated: buffer)
                return
            }

            let headerData = buffer.subdata(in: 0..<headerEnd.lowerBound)
            guard let headerText = String(data: headerData, encoding: .utf8) else {
                self.send(connection: connection, response: .error(400, "bad request"))
                return
            }

            let lines = headerText.split(separator: "\r\n", omittingEmptySubsequences: false)
            guard let requestLine = lines.first else {
                self.send(connection: connection, response: .error(400, "bad request"))
                return
            }
            let parts = requestLine.split(separator: " ")
            guard parts.count >= 2 else {
                self.send(connection: connection, response: .error(400, "bad request"))
                return
            }
            let method = String(parts[0])
            // Route on the path only; ignore any query string.
            let path = String(parts[1].split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false)[0])

            var contentLength = 0
            for line in lines.dropFirst() {
                let lower = line.lowercased()
                if lower.hasPrefix("content-length:") {
                    let value = lower.split(separator: ":", maxSplits: 1).last?
                        .trimmingCharacters(in: .whitespaces) ?? "0"
                    contentLength = Int(value) ?? 0
                }
            }

            if contentLength < 0 || contentLength > Self.maxBodyBytes {
                self.send(connection: connection, response: .error(413, "body too large"))
                return
            }

            let bodyStart = headerEnd.upperBound
            let available = buffer.count - bodyStart
            if available < contentLength {
                if isComplete {
                    self.send(connection: connection, response: .error(400, "short body"))
                    return
                }
                self.receive(on: connection, accumulated: buffer)
                return
            }

            let body = buffer.subdata(in: bodyStart..<(bodyStart + contentLength))
            let request = AgentHTTPRequest(method: method, path: path, body: body)

            Task {
                let response = await self.handler(request)
                self.send(connection: connection, response: response)
            }
        }
    }

    private func send(connection: NWConnection, response: AgentHTTPResponse) {
        var headers = "HTTP/1.1 \(response.status) \(statusText(response.status))\r\n"
        headers += "Content-Type: \(response.contentType)\r\n"
        headers += "Content-Length: \(response.body.count)\r\n"
        for (key, value) in response.extraHeaders {
            headers += "\(key): \(value)\r\n"
        }
        headers += "Connection: close\r\n\r\n"
        var data = Data(headers.utf8)
        data.append(response.body)
        connection.send(content: data, completion: .contentProcessed { _ in
            connection.cancel()
        })
    }

    private func statusText(_ code: Int) -> String {
        switch code {
        case 200: return "OK"
        case 400: return "Bad Request"
        case 404: return "Not Found"
        case 413: return "Payload Too Large"
        case 431: return "Request Header Fields Too Large"
        case 503: return "Service Unavailable"
        default: return "Error"
        }
    }
}
