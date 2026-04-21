import Foundation
import Network
import AVFoundation

/// Orchestrates a single scrcpy-server session:
///   1. adb push scrcpy-server.jar
///   2. adb forward tcp:<local> -> localabstract:scrcpy_<scid>
///   3. adb shell app_process ... com.genymobile.scrcpy.Server <version> <args>
///   4. connect NWConnection to 127.0.0.1:<local> (video socket)
///   5. read 1-byte handshake + 64-byte device name + 12-byte codec header
///
/// Keeps video-stream reading out of B2 — that lands in B3 with VideoToolbox.
@MainActor
final class ScrcpySession: ObservableObject {
    enum State: Equatable {
        case idle
        case pushing
        case forwarding
        case startingServer
        case handshaking
        case connected(ScrcpyDeviceMeta)
        case failed(String)

        static func == (lhs: State, rhs: State) -> Bool {
            switch (lhs, rhs) {
            case (.idle, .idle), (.pushing, .pushing),
                 (.forwarding, .forwarding), (.startingServer, .startingServer),
                 (.handshaking, .handshaking):
                return true
            case let (.connected(a), .connected(b)):
                return a.deviceName == b.deviceName && a.rawCodecId == b.rawCodecId
                    && a.width == b.width && a.height == b.height
            case let (.failed(a), .failed(b)):
                return a == b
            default:
                return false
            }
        }
    }

    @Published var state: State = .idle
    @Published var log: String = ""

    private let toolchain: Toolchain
    private let adb: AdbBridge

    private var serverProcess: Process?
    private var videoConnection: NWConnection?
    private var controlConnection: NWConnection?
    private var receivedBuffer = Data()
    private var videoPumpTask: Task<Void, Never>?
    private let decoder = H264Decoder()
    private var localPort: UInt16 = 0
    private var serial: String = ""
    private var scid: UInt32 = 0

    init(toolchain: Toolchain) {
        self.toolchain = toolchain
        self.adb = AdbBridge(adbURL: toolchain.adbURL)
    }

    func start(serial: String) {
        guard case .idle = state else { return }
        self.serial = serial
        self.scid = UInt32.random(in: 0x0000_0001...0x7FFF_FFFF)
        self.localPort = UInt16.random(in: 27183...27199)
        self.receivedBuffer = Data()
        Task { await self.run() }
    }

    func stop() {
        appendLog("[session] stop requested")
        videoPumpTask?.cancel()
        videoPumpTask = nil
        serverProcess?.terminate()
        videoConnection?.cancel()
        videoConnection = nil
        controlConnection?.cancel()
        controlConnection = nil
        decoder.reset()
        Task { await self.cleanupAdbForward() }
        state = .idle
    }

    /// Called by the SwiftUI mirror view as soon as the backing layer is live.
    func attach(displayLayer: AVSampleBufferDisplayLayer) {
        decoder.displayLayer = displayLayer
    }

    /// Forward a pointer event from the mirror view. `viewPoint` is in the
    /// view's coordinate system (top-left origin, isFlipped view); `viewSize`
    /// is the view's current bounds. We linearly map to device pixel space
    /// using the resolution reported by the handshake.
    func sendPointerEvent(
        action: ControlMessage.TouchAction,
        viewPoint p: CGPoint,
        viewSize vs: CGSize,
        buttonsPressed: Bool
    ) {
        guard case let .connected(meta) = state else { return }
        let dx = Int32((Double(p.x) / Double(vs.width)) * Double(meta.width))
        let dy = Int32((Double(p.y) / Double(vs.height)) * Double(meta.height))
        let clampedX = max(0, min(Int32(meta.width - 1), dx))
        let clampedY = max(0, min(Int32(meta.height - 1), dy))
        let data = ControlMessage.injectTouch(
            action: action,
            x: clampedX, y: clampedY,
            screenWidth: UInt16(meta.width), screenHeight: UInt16(meta.height),
            buttonsPressed: buttonsPressed
        )
        send(control: data)
    }

    func sendScroll(viewPoint p: CGPoint, viewSize vs: CGSize,
                    hScroll: Float, vScroll: Float) {
        guard case let .connected(meta) = state else { return }
        let dx = Int32((Double(p.x) / Double(vs.width)) * Double(meta.width))
        let dy = Int32((Double(p.y) / Double(vs.height)) * Double(meta.height))
        let data = ControlMessage.injectScroll(
            x: dx, y: dy,
            screenWidth: UInt16(meta.width), screenHeight: UInt16(meta.height),
            hScroll: hScroll, vScroll: vScroll
        )
        send(control: data)
    }

    private func send(control data: Data) {
        guard let conn = controlConnection else { return }
        conn.send(content: data, completion: .contentProcessed { error in
            if let error {
                NSLog("[control] send failed: %@", "\(error)")
            }
        })
    }

    // MARK: - Stages

    private func run() async {
        do {
            try await pushServerJar()
            try await setupForward()
            try await launchServer()
            // The server needs time to set up its LocalServerSocket after
            // app_process starts. adb-forward will happily accept our connect
            // before the tunnel can really reach the device, then drop us,
            // so we retry the whole video+handshake dance on failure.
            let meta = try await openSocketsAndHandshakeWithRetry()
            appendLog("[session] connected: \(meta.deviceName) · \(meta.width)x\(meta.height) · codec=\(meta.videoCodec?.label ?? String(format: "0x%08x", meta.rawCodecId))")
            state = .connected(meta)
            startVideoPump()
        } catch {
            let msg = (error as? LocalizedError)?.errorDescription ?? "\(error)"
            appendLog("[session] FAILED: \(msg)")
            state = .failed(msg)
            await cleanupAdbForward()
            serverProcess?.terminate()
            videoConnection?.cancel()
            controlConnection?.cancel()
        }
    }

    private func pushServerJar() async throws {
        state = .pushing
        appendLog("[adb] push \(toolchain.scrcpyServerJarURL.lastPathComponent)")
        _ = try await adb.run([
            "push",
            toolchain.scrcpyServerJarURL.path,
            "/data/local/tmp/scrcpy-server.jar",
        ], serial: serial)
    }

    private func setupForward() async throws {
        state = .forwarding
        let socketName = String(format: "localabstract:scrcpy_%08x", scid)
        appendLog("[adb] forward tcp:\(localPort) \(socketName)")
        _ = try await adb.run([
            "forward",
            "tcp:\(localPort)",
            socketName,
        ], serial: serial)
    }

    private func launchServer() async throws {
        state = .startingServer

        // tunnel_forward=true: server acts as a server inside the adb tunnel;
        //   we connect out to localhost:<localPort> for each channel.
        // audio=false, control=true: minimal MVP (no audio yet, input later).
        // cleanup=false: server shouldn't auto-run cleanup hooks that expect
        //   scrcpy-cli behavior.
        let serverArgs = [
            "shell",
            "CLASSPATH=/data/local/tmp/scrcpy-server.jar",
            "app_process", "/", "com.genymobile.scrcpy.Server",
            ScrcpyProtocol.version,
            "scid=\(String(format: "%08x", scid))",
            "log_level=info",
            "tunnel_forward=true",
            "audio=false",
            "control=true",
            "cleanup=false",
        ]
        appendLog("[adb] shell app_process ... (scid=\(String(format: "%08x", scid)))")
        serverProcess = try adb.spawn(
            serverArgs,
            serial: serial,
            onStdout: { [weak self] s in
                Task { @MainActor in self?.appendLog("[server] \(s.trimmingCharacters(in: .newlines))") }
            },
            onStderr: { [weak self] s in
                Task { @MainActor in self?.appendLog("[server] \(s.trimmingCharacters(in: .newlines))") }
            },
            onExit: { [weak self] code in
                Task { @MainActor in self?.appendLog("[server] exited code=\(code)") }
            }
        )

        // The server takes ~1–2s to spin up and create its LocalServerSocket.
        // We retry connect() with backoff in connectVideoSocket rather than
        // sleeping a fixed duration here.
    }

    private func openSocketsAndHandshakeWithRetry() async throws -> ScrcpyDeviceMeta {
        state = .handshaking
        var lastError: Error?
        for attempt in 1...40 {
            do {
                return try await openSocketsAndHandshake()
            } catch {
                lastError = error
                // Connection got torn down before handshake: close whatever we
                // opened, wait, and try again until ~8s have passed.
                videoConnection?.cancel()
                videoConnection = nil
                controlConnection?.cancel()
                controlConnection = nil
                receivedBuffer = Data()
                if attempt == 40 { break }
                try? await Task.sleep(nanoseconds: 200_000_000)
            }
        }
        throw lastError ?? SessionError.connectFailed
    }

    private func openSocketsAndHandshake() async throws -> ScrcpyDeviceMeta {
        let host = NWEndpoint.Host("127.0.0.1")
        let port = NWEndpoint.Port(rawValue: localPort)!

        // 1. Open video socket.
        let video = try await openConnection(host: host, port: port)
        self.videoConnection = video

        // 2. Read the 1-byte handshake. If adb's tunnel hasn't reached the
        //    server yet, this fails with ENODATA / connection reset, which is
        //    our signal to retry the whole thing.
        let dummy = try await receiveExactly(from: video, count: 1)
        if dummy[0] != 0 {
            appendLog("[warn] unexpected dummy byte: 0x\(String(format: "%02x", dummy[0]))")
        }

        // 3. Now that the tunnel is truly established, open the control socket
        //    so the server will proceed to send device metadata on video.
        let control = try await openConnection(host: host, port: port)
        self.controlConnection = control

        // 4. Read the 64-byte device name + 12-byte codec header on video.
        let nameAndCodec = try await receiveExactly(
            from: video,
            count: ScrcpyProtocol.deviceNameFieldLength + 12
        )

        let nameEnd = ScrcpyProtocol.deviceNameFieldLength
        let nameBytes = nameAndCodec[0..<nameEnd]
        let terminated = nameBytes.prefix { $0 != 0 }
        let deviceName = String(data: terminated, encoding: .utf8) ?? "?"

        let codecId = nameAndCodec.readBigEndian(UInt32.self, at: nameEnd)
        let width = nameAndCodec.readBigEndian(UInt32.self, at: nameEnd + 4)
        let height = nameAndCodec.readBigEndian(UInt32.self, at: nameEnd + 8)

        return ScrcpyDeviceMeta(
            deviceName: deviceName,
            videoCodec: ScrcpyProtocol.VideoCodec(rawValue: codecId),
            rawCodecId: codecId,
            width: Int(width),
            height: Int(height)
        )
    }

    private func openConnection(host: NWEndpoint.Host, port: NWEndpoint.Port) async throws -> NWConnection {
        let conn = NWConnection(host: host, port: port, using: .tcp)
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
            var resumed = false
            conn.stateUpdateHandler = { newState in
                guard !resumed else { return }
                switch newState {
                case .ready:
                    resumed = true
                    cont.resume()
                case .failed(let err):
                    resumed = true
                    cont.resume(throwing: err)
                case .cancelled:
                    resumed = true
                    cont.resume(throwing: SessionError.connectFailed)
                default:
                    break
                }
            }
            conn.start(queue: .main)
        }
        return conn
    }

    /// Read exactly `count` bytes from `conn`, or throw if the peer closes first.
    private func receiveExactly(from conn: NWConnection, count: Int) async throws -> Data {
        var acc = Data()
        acc.reserveCapacity(count)
        while acc.count < count {
            let chunk = try await receiveOnce(from: conn, maxLength: count - acc.count)
            if chunk.isEmpty { throw SessionError.shortRead }
            acc.append(chunk)
        }
        return acc
    }

    private func receiveOnce(from conn: NWConnection, maxLength: Int) async throws -> Data {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Data, Error>) in
            conn.receive(minimumIncompleteLength: 1, maximumLength: maxLength) { data, _, isComplete, error in
                if let error {
                    cont.resume(throwing: error)
                    return
                }
                if (data == nil || data!.isEmpty) && isComplete {
                    cont.resume(throwing: SessionError.shortRead)
                    return
                }
                cont.resume(returning: data ?? Data())
            }
        }
    }

    private func startVideoPump() {
        guard let conn = videoConnection else { return }
        videoPumpTask = Task { [weak self, decoder] in
            await self?.pumpVideoFrames(conn: conn, decoder: decoder)
        }
    }

    private func pumpVideoFrames(conn: NWConnection, decoder: H264Decoder) async {
        var frameCount = 0
        while !Task.isCancelled {
            do {
                let header = try await receiveExactly(from: conn, count: ScrcpyProtocol.frameHeaderLength)
                guard let info = ScrcpyVideoPacketHeader.parse(header) else {
                    await MainActor.run { self.appendLog("[video] bad header") }
                    return
                }
                let payload = try await receiveExactly(from: conn, count: info.size)
                if info.isConfig {
                    await MainActor.run {
                        self.appendLog("[video] config packet · \(payload.count) bytes")
                    }
                    decoder.ingestConfig(payload)
                } else {
                    decoder.ingestFrame(payload, pts: info.pts, keyFrame: info.isKeyFrame)
                    frameCount += 1
                    if frameCount == 1 || frameCount % 120 == 0 {
                        let snapshot = frameCount
                        await MainActor.run {
                            self.appendLog("[video] \(snapshot) frames decoded")
                        }
                    }
                }
            } catch {
                if Task.isCancelled { return }
                await MainActor.run {
                    self.appendLog("[video] stream ended: \(error)")
                }
                return
            }
        }
    }

    private func cleanupAdbForward() async {
        _ = try? await adb.run(["forward", "--remove", "tcp:\(localPort)"], serial: serial)
    }

    private func appendLog(_ line: String) {
        let ts = DateFormatter.logTs.string(from: Date())
        log += "[\(ts)] \(line)\n"
    }
}

enum SessionError: Error, LocalizedError {
    case connectFailed
    case shortRead

    var errorDescription: String? {
        switch self {
        case .connectFailed: return "TCP connection to scrcpy-server failed"
        case .shortRead: return "Short read from scrcpy-server"
        }
    }
}

private extension DateFormatter {
    static let logTs: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "HH:mm:ss.SSS"
        return f
    }()
}
