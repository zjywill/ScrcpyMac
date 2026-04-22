import SwiftUI
import AppKit

struct ContentView: View {
    @StateObject private var deviceManager: DeviceManager
    @StateObject private var session: ScrcpySession
    private let toolchain: Toolchain

    @State private var selectedSerial: String?
    @State private var stayAwake: Bool = false
    @State private var turnScreenOff: Bool = false
    @State private var audioOnly: Bool = false
    @State private var videoOnly: Bool = false

    init() {
        let tc = Toolchain.detect()
        self.toolchain = tc
        _deviceManager = StateObject(wrappedValue: DeviceManager(toolchain: tc))
        _session = StateObject(wrappedValue: ScrcpySession(toolchain: tc))
    }

    var body: some View {
        HStack(alignment: .top, spacing: 0) {
            VStack(alignment: .leading, spacing: 10) {
                header
                deviceRow
                optionsCompact
                Spacer(minLength: 0)
                actionBar
            }
            .padding(14)
            .frame(width: 260)

            mirrorPane
                .frame(width: 360, height: 700)
        }
        .frame(width: 620, height: 700)
        .task { await deviceManager.refresh() }
    }

    private var header: some View {
        HStack(alignment: .center, spacing: 8) {
            Image(systemName: "iphone.gen3.radiowaves.left.and.right")
                .foregroundStyle(.tint)
            Text("ScrcpyMac").font(.headline)
            Spacer()
        }
    }

    private var deviceRow: some View {
        HStack(spacing: 6) {
            Picker("", selection: $selectedSerial) {
                Text("— device —").tag(String?.none)
                ForEach(deviceManager.devices) { d in
                    Text("\(d.displayName)").tag(String?.some(d.serial))
                }
            }
            .labelsHidden()
            .frame(maxWidth: .infinity)

            Button {
                Task { await deviceManager.refresh() }
            } label: {
                if deviceManager.isRefreshing {
                    ProgressView().controlSize(.small)
                } else {
                    Image(systemName: "arrow.clockwise")
                }
            }
            .disabled(deviceManager.isRefreshing)
        }
        .onChange(of: deviceManager.devices) { newValue in
            if selectedSerial == nil, let first = newValue.first(where: { $0.state == "device" }) {
                selectedSerial = first.serial
            }
        }
    }

    private var optionsCompact: some View {
        VStack(alignment: .leading, spacing: 4) {
            Toggle("Stay awake", isOn: $stayAwake)
            Toggle("Turn screen off", isOn: $turnScreenOff)
                .disabled(audioOnly)
            Toggle("Audio only (disable video)", isOn: $audioOnly)
                .disabled(videoOnly)
            Toggle("Video only (disable audio)", isOn: $videoOnly)
                .disabled(audioOnly)
        }
        .toggleStyle(.checkbox)
        .controlSize(.small)
    }

    private var actionBar: some View {
        HStack {
            if isSessionActive {
                Button {
                    pasteClipboard()
                } label: {
                    Label("Paste Clipboard", systemImage: "doc.on.clipboard")
                        .frame(maxWidth: .infinity)
                }
                .disabled(!isConnected)

                Button(role: .destructive) {
                    session.stop()
                } label: {
                    Label("Stop", systemImage: "stop.fill")
                        .frame(maxWidth: .infinity)
                }
            } else {
                Button {
                    if let serial = selectedSerial {
                        session.start(
                            serial: serial,
                            options: SessionOptions(
                                audioOnly: audioOnly,
                                videoOnly: videoOnly,
                                stayAwake: stayAwake,
                                turnScreenOff: turnScreenOff
                            )
                        )
                    }
                } label: {
                    Label("Connect", systemImage: "play.fill")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .disabled(selectedSerial == nil)
            }
        }
    }

    private var isSessionActive: Bool {
        switch session.state {
        case .idle, .failed: return false
        default: return true
        }
    }

    private var mirrorPane: some View {
        ZStack {
            // Always-present sample-buffer surface; decoder pushes frames to
            // its layer as soon as the session is connected.
            MirrorView(
                onLayerReady: { layer in session.attach(displayLayer: layer) },
                onPointerEvent: { event in handlePointerEvent(event) },
                onKeyEvent: { event in handleKeyEvent(event) }
            )

            // Overlay status + log until the first frame lands. We hide the
            // overlay once we're connected so the video surface takes over.
            if !isConnected {
                ZStack {
                    Color.black
                    VStack(alignment: .leading, spacing: 6) {
                        Text(stateLabel)
                            .font(.caption)
                            .foregroundStyle(.white.opacity(0.9))
                        Divider().overlay(.white.opacity(0.1))
                        ScrollView {
                            Text(session.log.isEmpty ? "(no activity)" : session.log)
                                .font(.system(.caption2, design: .monospaced))
                                .foregroundStyle(.white.opacity(0.65))
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .textSelection(.enabled)
                        }
                    }
                    .padding(10)
                }
            }
        }
    }

    private var isConnected: Bool {
        if case .connected = session.state { return true }
        return false
    }

    private func handlePointerEvent(_ event: SampleBufferHostView.PointerEvent) {
        switch event.kind {
        case .down:
            session.sendPointerEvent(action: .down, viewPoint: event.location,
                                     viewSize: event.viewSize, buttonsPressed: true)
        case .move:
            session.sendPointerEvent(action: .move, viewPoint: event.location,
                                     viewSize: event.viewSize, buttonsPressed: true)
        case .up:
            session.sendPointerEvent(action: .up, viewPoint: event.location,
                                     viewSize: event.viewSize, buttonsPressed: false)
        case .scroll(let h, let v):
            session.sendScroll(viewPoint: event.location, viewSize: event.viewSize,
                               hScroll: h, vScroll: v)
        }
    }

    private func handleKeyEvent(_ keyEvent: SampleBufferHostView.KeyEvent) {
        // Cmd+V on Mac should paste the Mac clipboard into the device. Do this
        // on keyDown only so the keyUp doesn't trigger a second paste attempt.
        if AndroidKeycode.isMacPasteShortcut(keyEvent.event) {
            if keyEvent.kind == .down {
                pasteClipboard()
            }
            return
        }
        guard let mapped = AndroidKeycode.map(event: keyEvent.event) else { return }
        let action: ControlMessage.KeyAction = keyEvent.kind == .down ? .down : .up
        session.sendKeyEvent(action: action, keycode: mapped.keycode, metaState: mapped.metaState)
    }

    private func pasteClipboard() {
        let pasteboard = NSPasteboard.general
        guard let text = pasteboard.string(forType: .string), !text.isEmpty else { return }
        session.pasteClipboardText(text)
    }

    private var stateLabel: String {
        switch session.state {
        case .idle: return "idle — select device and click Connect (default: video + audio)"
        case .pushing: return "pushing scrcpy-server.jar…"
        case .forwarding: return "setting up adb forward…"
        case .startingServer: return "starting server on device…"
        case .handshaking: return "handshaking…"
        case .connected(let meta):
            let codec = meta.videoCodec?.label ?? String(format: "0x%08x", meta.rawCodecId)
            return "✅ \(meta.deviceName) · \(meta.width)×\(meta.height) · \(codec)"
        case .failed(let msg): return "❌ \(msg)"
        }
    }
}
