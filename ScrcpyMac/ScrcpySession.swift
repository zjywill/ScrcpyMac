import Foundation
import Network
import AVFoundation

struct SessionOptions {
    var audioOnly: Bool = false
    var videoOnly: Bool = false
    var stayAwake: Bool = false
    var turnScreenOff: Bool = false

    var wantsVideo: Bool { !audioOnly }
    var wantsAudio: Bool { !videoOnly }
}

/// Minimal PCM player for scrcpy raw audio: 48kHz, stereo, 16-bit little-endian.
final class AudioPlayer {
    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()
    private let format = AVAudioFormat(commonFormat: .pcmFormatInt16,
                                       sampleRate: 48_000,
                                       channels: 2,
                                       interleaved: true)!
    private var started = false

    init() {
        engine.attach(player)
        engine.connect(player, to: engine.mainMixerNode, format: format)
    }

    func start() throws {
        guard !started else { return }
        try engine.start()
        player.play()
        started = true
    }

    func stop() {
        player.stop()
        engine.stop()
        engine.reset()
        started = false
    }

    func enqueuePCM(_ data: Data) {
        guard started, !data.isEmpty else { return }
        let bytesPerFrame = Int(format.streamDescription.pointee.mBytesPerFrame)
        guard bytesPerFrame > 0, data.count % bytesPerFrame == 0 else { return }

        let frameCapacity = AVAudioFrameCount(data.count / bytesPerFrame)
        guard let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: frameCapacity) else {
            return
        }
        buffer.frameLength = frameCapacity

        data.withUnsafeBytes { rawBuffer in
            guard let src = rawBuffer.baseAddress,
                  let channelData = buffer.int16ChannelData else {
                return
            }
            memcpy(channelData.pointee, src, data.count)
        }

        player.scheduleBuffer(buffer, completionHandler: nil)
    }
}

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
    private var audioConnection: NWConnection?
    private var controlConnection: NWConnection?
    private var receivedBuffer = Data()
    private var videoPumpTask: Task<Void, Never>?
    private var audioPumpTask: Task<Void, Never>?
    private let decoder = H264Decoder()
    private let audioPlayer = AudioPlayer()
    private var localPort: UInt16 = 0
    private(set) var serial: String = ""
    private var scid: UInt32 = 0
    private var runToken: UInt64 = 0
    private var options = SessionOptions()

    init(toolchain: Toolchain) {
        self.toolchain = toolchain
        self.adb = AdbBridge(adbURL: toolchain.adbURL)
    }

    func start(serial: String, options: SessionOptions = SessionOptions()) {
        switch state {
        case .idle, .failed:
            break
        default:
            return
        }

        runToken &+= 1
        self.serial = serial
        self.options = options
        self.scid = UInt32.random(in: 0x0000_0001...0x7FFF_FFFF)
        self.localPort = UInt16.random(in: 27183...27199)
        self.receivedBuffer = Data()
        self.log = ""
        let token = runToken
        Task { await self.run(token: token) }
    }

    func stop() {
        appendLog("[session] stop requested")
        runToken &+= 1
        let port = localPort
        let serialToCleanup = serial
        teardownRuntime()
        state = .idle
        Task { await self.cleanupAdbForward(port: port, serial: serialToCleanup) }
    }

    /// Called by the SwiftUI mirror view as soon as the backing layer is live.
    func attach(displayLayer: AVSampleBufferDisplayLayer) {
        decoder.displayLayer = displayLayer
    }

    /// Latest mirror surface for Agent Service screenshots (low-res fallback).
    var mirrorDisplayLayer: AVSampleBufferDisplayLayer? {
        decoder.displayLayer
    }

    /// Device serial when connected (for Agent HTTP responses).
    var connectedSerial: String? {
        guard case .connected = state else { return nil }
        return serial
    }

    /// Full-resolution PNG from the latest decoded H.264 frame. Marked
    /// `nonisolated` so the CIImage render + PNG encode run off the main actor.
    nonisolated func captureDecoderFramePNG() -> Data? {
        decoder.latestFramePNG()
    }

    /// Toggle the decoder's screenshot cache. Enabled only while the Agent
    /// service is running so mirroring alone doesn't pay for a second decode.
    func setAgentCaptureEnabled(_ enabled: Bool) {
        decoder.captureEnabled = enabled
    }

    /// Connected device metadata for screenshot HTTP headers.
    func screenshotMetadata() -> (serial: String, width: Int, height: Int)? {
        guard case let .connected(meta) = state else { return nil }
        return (serial, meta.width, meta.height)
    }

    /// Accessibility tree XML via adb uiautomator (Agent `/ui-tree`).
    func agentUITreeXML() async throws -> String {
        guard case .connected = state, !serial.isEmpty else {
            throw AgentServiceError.notConnected
        }
        // One adb round trip instead of three (dump + cat + rm).
        let remote = "/sdcard/window_dump.xml"
        let command = "uiautomator dump \(remote) >/dev/null 2>&1 && cat \(remote); rm -f \(remote)"
        return try await adb.run(["shell", command], serial: serial)
    }

    /// Foreground app via session adb (`GET /foreground`).
    func agentForegroundApp() async throws -> [String: String] {
        guard case .connected = state, !serial.isEmpty else {
            throw AgentServiceError.notConnected
        }
        let output = try await adb.run(
            ["shell", "dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -1"],
            serial: serial
        )
        var package = ""
        var activity = ""
        if let regex = try? NSRegularExpression(pattern: #"([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)"#),
           let match = regex.firstMatch(in: output, range: NSRange(output.startIndex..., in: output)),
           match.numberOfRanges >= 3,
           let pkgRange = Range(match.range(at: 1), in: output),
           let actRange = Range(match.range(at: 2), in: output) {
            package = String(output[pkgRange])
            activity = String(output[actRange])
        }
        return ["package": package, "activity": activity, "raw": output.trimmingCharacters(in: .whitespacesAndNewlines)]
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
        guard let point = mapAspectFitPoint(
            p,
            viewSize: vs,
            contentSize: CGSize(width: meta.width, height: meta.height)
        ) else {
            return
        }
        let data = ControlMessage.injectTouch(
            action: action,
            x: point.x, y: point.y,
            screenWidth: UInt16(meta.width), screenHeight: UInt16(meta.height),
            buttonsPressed: buttonsPressed
        )
        send(control: data)
    }

    func sendScroll(viewPoint p: CGPoint, viewSize vs: CGSize,
                    hScroll: Float, vScroll: Float) {
        guard case let .connected(meta) = state else { return }
        guard let point = mapAspectFitPoint(
            p,
            viewSize: vs,
            contentSize: CGSize(width: meta.width, height: meta.height)
        ) else {
            return
        }
        let data = ControlMessage.injectScroll(
            x: point.x, y: point.y,
            screenWidth: UInt16(meta.width), screenHeight: UInt16(meta.height),
            hScroll: hScroll, vScroll: vScroll
        )
        send(control: data)
    }

    func sendKeyEvent(action: ControlMessage.KeyAction, keycode: Int32, metaState: Int32) {
        guard case .connected = state else { return }
        let data = ControlMessage.injectKeycode(
            action: action,
            keycode: keycode,
            metaState: metaState
        )
        send(control: data)
    }

    func sendText(_ text: String) {
        guard case .connected = state else { return }
        guard !text.isEmpty else { return }
        send(control: ControlMessage.injectText(text))
    }

    func pasteClipboardText(_ text: String) {
        guard case .connected = state else { return }
        guard !text.isEmpty else { return }
        appendLog("[control] set clipboard + paste · \(text.utf8.count) bytes")
        // sequence=0 (SEQUENCE_INVALID): we do not read the control socket,
        // so we don't want the server to send back an AckClipboard.
        let data = ControlMessage.setClipboard(sequence: 0, text: text, paste: true)
        send(control: data)
    }

    func send(control data: Data) {
        guard let conn = controlConnection else { return }
        conn.send(content: data, completion: .contentProcessed { error in
            if let error {
                NSLog("[control] send failed: %@", "\(error)")
            }
        })
    }

    /// Convert a point from an aspect-fit mirror view into native device
    /// coordinates. Points in the letterbox bars are ignored.
    private func mapAspectFitPoint(
        _ point: CGPoint,
        viewSize: CGSize,
        contentSize: CGSize
    ) -> (x: Int32, y: Int32)? {
        guard viewSize.width > 0, viewSize.height > 0,
              contentSize.width > 0, contentSize.height > 0 else {
            return nil
        }

        let scale = min(
            viewSize.width / contentSize.width,
            viewSize.height / contentSize.height
        )
        let renderedSize = CGSize(
            width: contentSize.width * scale,
            height: contentSize.height * scale
        )
        let offsetX = (viewSize.width - renderedSize.width) / 2
        let offsetY = (viewSize.height - renderedSize.height) / 2

        guard point.x >= offsetX, point.x < offsetX + renderedSize.width,
              point.y >= offsetY, point.y < offsetY + renderedSize.height else {
            return nil
        }

        let deviceX = Int32((point.x - offsetX) / scale)
        let deviceY = Int32((point.y - offsetY) / scale)
        return (
            max(0, min(Int32(contentSize.width - 1), deviceX)),
            max(0, min(Int32(contentSize.height - 1), deviceY))
        )
    }

    // MARK: - Stages

    private func run(token: UInt64) async {
        do {
            try Task.checkCancellation()
            try await pushServerJar()
            try ensureCurrentRun(token)
            try await setupForward()
            try ensureCurrentRun(token)
            try await launchServer(token: token)
            try ensureCurrentRun(token)
            // The server needs time to set up its LocalServerSocket after
            // app_process starts. adb-forward will happily accept our connect
            // before the tunnel can really reach the device, then drop us,
            // so we retry the whole video+handshake dance on failure.
            let meta = try await openSocketsAndHandshakeWithRetry(token: token)
            try ensureCurrentRun(token)
            appendLog("[session] connected: \(meta.deviceName) · \(meta.width)x\(meta.height) · codec=\(meta.videoCodec?.label ?? String(format: "0x%08x", meta.rawCodecId))")
            state = .connected(meta)
            if options.turnScreenOff && !options.audioOnly {
                appendLog("[control] turn device screen off")
                send(control: ControlMessage.setDisplayPower(on: false))
            }
            if options.wantsVideo {
                startVideoPump(token: token)
            }
            if options.wantsAudio {
                try startAudioPumpIfNeeded(token: token)
            }
        } catch {
            guard token == runToken else { return }
            let msg = (error as? LocalizedError)?.errorDescription ?? "\(error)"
            appendLog("[session] FAILED: \(msg)")
            let port = localPort
            let serialToCleanup = serial
            teardownRuntime()
            state = .failed(msg)
            await cleanupAdbForward(port: port, serial: serialToCleanup)
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

    private func launchServer(token: UInt64) async throws {
        state = .startingServer

        // tunnel_forward=true: server acts as a server inside the adb tunnel;
        //   we connect out to localhost:<localPort> for each enabled channel.
        // cleanup=false: server shouldn't auto-run cleanup hooks that expect
        //   scrcpy-cli behavior.
        let audioEnabled = options.wantsAudio
        let videoEnabled = options.wantsVideo
        let controlEnabled = !options.audioOnly

        let serverArgs = [
            "shell",
            "CLASSPATH=/data/local/tmp/scrcpy-server.jar",
            "app_process", "/", "com.genymobile.scrcpy.Server",
            ScrcpyProtocol.version,
            "scid=\(String(format: "%08x", scid))",
            "log_level=info",
            "tunnel_forward=true",
            "video=\(videoEnabled)",
            "audio=\(audioEnabled)",
            "audio_codec=raw",
            "control=\(controlEnabled)",
            // We never read the control socket back-channel, so don't let the
            // server push device-clipboard changes to us — they would just
            // fill the socket buffer and eventually block the sender thread.
            "clipboard_autosync=false",
            "cleanup=false",
        ] + (options.stayAwake ? ["stay_awake=true"] : [])
        appendLog("[adb] shell app_process ... (scid=\(String(format: "%08x", scid)))")
        serverProcess = try adb.spawn(
            serverArgs,
            serial: serial,
            onStdout: { [weak self] s in
                Task { @MainActor in
                    guard let self, token == self.runToken else { return }
                    self.appendLog("[server] \(s.trimmingCharacters(in: .newlines))")
                }
            },
            onStderr: { [weak self] s in
                Task { @MainActor in
                    guard let self, token == self.runToken else { return }
                    self.appendLog("[server] \(s.trimmingCharacters(in: .newlines))")
                }
            },
            onExit: { [weak self] code in
                Task { @MainActor in
                    guard let self, token == self.runToken else { return }
                    self.appendLog("[server] exited code=\(code)")
                }
            }
        )

        // The server takes ~1–2s to spin up and create its LocalServerSocket.
        // We retry connect() with backoff in connectVideoSocket rather than
        // sleeping a fixed duration here.
    }

    private func openSocketsAndHandshakeWithRetry(token: UInt64) async throws -> ScrcpyDeviceMeta {
        state = .handshaking
        var lastError: Error?
        for attempt in 1...40 {
            try ensureCurrentRun(token)
            do {
                return try await openSocketsAndHandshake()
            } catch {
                lastError = error
                // Connection got torn down before handshake: close whatever we
                // opened, wait, and try again until ~8s have passed.
                videoConnection?.cancel()
                videoConnection = nil
                audioConnection?.cancel()
                audioConnection = nil
                controlConnection?.cancel()
                controlConnection = nil
                receivedBuffer = Data()
                if attempt == 40 { break }
                try await Task.sleep(nanoseconds: 200_000_000)
            }
        }
        throw lastError ?? SessionError.connectFailed
    }

    private func openSocketsAndHandshake() async throws -> ScrcpyDeviceMeta {
        let host = NWEndpoint.Host("127.0.0.1")
        let port = NWEndpoint.Port(rawValue: localPort)!

        // Server accepts sockets in order: video, audio, control — and writes
        // the 1-byte dummy only on the FIRST accepted socket. Device metadata
        // and codec headers are written by async processors that do not start
        // until every enabled socket has been accepted. So we must open all
        // sockets before attempting to read any payload; otherwise we deadlock
        // against localServerSocket.accept() on the server side.

        var video: NWConnection?
        if options.wantsVideo {
            video = try await openConnection(host: host, port: port)
            self.videoConnection = video
        }

        var audio: NWConnection?
        if options.wantsAudio {
            audio = try await openConnection(host: host, port: port)
            self.audioConnection = audio
        }

        if !options.audioOnly {
            let control = try await openConnection(host: host, port: port)
            self.controlConnection = control
        }

        // Read the 1-byte handshake from the first opened socket. A failure
        // here (ENODATA / reset) means adb's tunnel hasn't reached the server
        // yet — our signal to retry the whole thing.
        let firstSocket = video ?? audio ?? controlConnection!
        let dummy = try await receiveExactly(from: firstSocket, count: 1)
        if dummy[0] != 0 {
            appendLog("[warn] unexpected dummy byte: 0x\(String(format: "%02x", dummy[0]))")
        }

        if let audio {
            let codecHeader = try await receiveExactly(from: audio, count: 4)
            let codecId = codecHeader.readBigEndian(UInt32.self, at: 0)
            appendLog("[audio] codec=\(String(format: "0x%08x", codecId))")
        }

        guard let video else {
            return ScrcpyDeviceMeta(
                deviceName: serial,
                videoCodec: nil,
                rawCodecId: 0,
                width: 1,
                height: 1
            )
        }

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

    private func startVideoPump(token: UInt64) {
        guard let conn = videoConnection else { return }
        videoPumpTask = Task { [weak self, decoder] in
            await self?.pumpVideoFrames(conn: conn, decoder: decoder, token: token)
        }
    }

    private func startAudioPumpIfNeeded(token: UInt64) throws {
        guard let conn = audioConnection else { return }
        try audioPlayer.start()
        audioPumpTask = Task { [weak self] in
            await self?.pumpAudioFrames(conn: conn, token: token)
        }
    }

    private func pumpVideoFrames(conn: NWConnection, decoder: H264Decoder, token: UInt64) async {
        var frameCount = 0
        while !Task.isCancelled {
            do {
                let header = try await receiveExactly(from: conn, count: ScrcpyProtocol.frameHeaderLength)
                guard let info = ScrcpyVideoPacketHeader.parse(header) else {
                    await MainActor.run {
                        guard token == self.runToken else { return }
                        self.appendLog("[video] bad header")
                    }
                    return
                }
                let payload = try await receiveExactly(from: conn, count: info.size)
                if info.isConfig {
                    await MainActor.run {
                        guard token == self.runToken else { return }
                        self.appendLog("[video] config packet · \(payload.count) bytes")
                    }
                    decoder.ingestConfig(payload)
                } else {
                    decoder.ingestFrame(payload, pts: info.pts, keyFrame: info.isKeyFrame)
                    frameCount += 1
                    if frameCount == 1 || frameCount % 120 == 0 {
                        let snapshot = frameCount
                        await MainActor.run {
                            guard token == self.runToken else { return }
                            self.appendLog("[video] \(snapshot) frames decoded")
                        }
                    }
                }
            } catch {
                if Task.isCancelled { return }
                await MainActor.run {
                    guard token == self.runToken else { return }
                    self.appendLog("[video] stream ended: \(error)")
                    let msg = (error as? LocalizedError)?.errorDescription ?? "\(error)"
                    let port = self.localPort
                    let serialToCleanup = self.serial
                    self.runToken &+= 1
                    self.teardownRuntime()
                    self.state = .failed("Video stream ended: \(msg)")
                    Task { await self.cleanupAdbForward(port: port, serial: serialToCleanup) }
                }
                return
            }
        }
    }

    private func pumpAudioFrames(conn: NWConnection, token: UInt64) async {
        while !Task.isCancelled {
            do {
                let header = try await receiveExactly(from: conn, count: ScrcpyProtocol.frameHeaderLength)
                guard let info = ScrcpyVideoPacketHeader.parse(header) else {
                    await MainActor.run {
                        guard token == self.runToken else { return }
                        self.appendLog("[audio] bad header")
                    }
                    return
                }
                let payload = try await receiveExactly(from: conn, count: info.size)
                if info.isConfig {
                    continue
                }
                audioPlayer.enqueuePCM(payload)
            } catch {
                if Task.isCancelled { return }
                await MainActor.run {
                    guard token == self.runToken else { return }
                    self.appendLog("[audio] stream ended: \(error)")
                }
                return
            }
        }
    }

    private func teardownRuntime() {
        videoPumpTask?.cancel()
        videoPumpTask = nil
        audioPumpTask?.cancel()
        audioPumpTask = nil
        serverProcess?.terminate()
        serverProcess = nil
        videoConnection?.cancel()
        videoConnection = nil
        audioConnection?.cancel()
        audioConnection = nil
        controlConnection?.cancel()
        controlConnection = nil
        receivedBuffer = Data()
        decoder.reset()
        audioPlayer.stop()
    }

    private func cleanupAdbForward(port: UInt16, serial: String) async {
        guard port != 0, !serial.isEmpty else { return }
        _ = try? await adb.run(["forward", "--remove", "tcp:\(port)"], serial: serial)
    }

    private func ensureCurrentRun(_ token: UInt64) throws {
        if Task.isCancelled || token != runToken {
            throw CancellationError()
        }
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
