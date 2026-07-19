import SwiftUI
import AppKit

// MARK: - Motion tokens

private extension Animation {
    /// Strong ease-out — starts fast so the UI feels like it responds instantly.
    static let uiEaseOut = Animation.timingCurve(0.23, 1, 0.32, 1, duration: 0.22)
    /// Button press feedback.
    static let press = Animation.timingCurve(0.23, 1, 0.32, 1, duration: 0.16)
}

// MARK: - Button styles

private struct PrimaryButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var isEnabled
    var tint: Color = .accentColor

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 13, weight: .semibold))
            .foregroundStyle(.white)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 8)
            .background(
                RoundedRectangle(cornerRadius: 8, style: .continuous)
                    .fill(tint)
                    .brightness(configuration.isPressed ? -0.08 : 0)
            )
            .opacity(isEnabled ? 1 : 0.4)
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(.press, value: configuration.isPressed)
            .contentShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

private struct SecondaryButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 13, weight: .medium))
            .frame(maxWidth: .infinity)
            .padding(.vertical, 8)
            .background(
                RoundedRectangle(cornerRadius: 8, style: .continuous)
                    .fill(Color.primary.opacity(configuration.isPressed ? 0.12 : 0.06))
            )
            .opacity(isEnabled ? 1 : 0.4)
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(.press, value: configuration.isPressed)
            .contentShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
    }
}

private struct IconButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .frame(width: 28, height: 28)
            .background(
                RoundedRectangle(cornerRadius: 7, style: .continuous)
                    .fill(Color.primary.opacity(configuration.isPressed ? 0.12 : 0.06))
            )
            .scaleEffect(configuration.isPressed ? 0.94 : 1)
            .animation(.press, value: configuration.isPressed)
            .contentShape(RoundedRectangle(cornerRadius: 7, style: .continuous))
    }
}

// MARK: - ContentView

struct ContentView: View {
    @StateObject private var deviceManager: DeviceManager
    @StateObject private var session: ScrcpySession
    private let toolchain: Toolchain

    @State private var selectedSerial: String?
    @State private var stayAwake: Bool = false
    @State private var turnScreenOff: Bool = false
    @State private var audioOnly: Bool = false
    @State private var videoOnly: Bool = false
    @State private var showLog: Bool = false
    @State private var showWifiPopover: Bool = false
    @State private var wifiAddress: String = ""
    @State private var wifiError: String?
    @AppStorage("agentServiceAutoEnable") private var agentServiceAutoEnable: Bool = true
    @State private var agentServiceEnabled: Bool = false
    @StateObject private var pluginInstaller = PluginInstaller()

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    init() {
        let tc = Toolchain.detect()
        self.toolchain = tc
        _deviceManager = StateObject(wrappedValue: DeviceManager(toolchain: tc))
        _session = StateObject(wrappedValue: ScrcpySession(toolchain: tc))
    }

    var body: some View {
        HStack(alignment: .top, spacing: 0) {
            sidebar
                .frame(width: 260)

            mirrorPane
                .frame(width: 360, height: 700)
        }
        .frame(width: 620, height: 700)
        .task { await deviceManager.refresh() }
        .onChange(of: isConnected) { connected in
            AgentService.shared.attach(session: session)
            if connected {
                if agentServiceAutoEnable {
                    agentServiceEnabled = true
                }
                if agentServiceEnabled {
                    startAgentService()
                }
            } else {
                agentServiceEnabled = false
                AgentService.shared.stop()
            }
        }
        .onChange(of: agentServiceEnabled) { enabled in
            AgentService.shared.attach(session: session)
            if enabled && isConnected {
                startAgentService()
            } else {
                AgentService.shared.stop()
            }
        }
        .onDisappear {
            AgentService.shared.stop()
        }
    }

    private func startAgentService() {
        do {
            try AgentService.shared.start()
        } catch {
            NSLog("[agent] failed to start service: %@", "\(error)")
            agentServiceEnabled = false
        }
    }

    // MARK: Sidebar

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 20) {
            header
            deviceSection
            optionsSection
            Spacer(minLength: 0)
            statusChip
            actionBar
        }
        .padding(16)
    }

    private var header: some View {
        HStack(spacing: 10) {
            Image(systemName: "iphone.gen3.radiowaves.left.and.right")
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(.tint)
                .frame(width: 32, height: 32)
                .background(
                    RoundedRectangle(cornerRadius: 8, style: .continuous)
                        .fill(Color.accentColor.opacity(0.12))
                )
            VStack(alignment: .leading, spacing: 1) {
                Text("ScrcpyMac")
                    .font(.system(size: 14, weight: .semibold))
                Text("Android mirroring")
                    .font(.system(size: 11))
                    .foregroundStyle(.secondary)
            }
            Spacer()
        }
    }

    private var deviceSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            sectionLabel("Device")

            HStack(spacing: 8) {
                HStack(spacing: 8) {
                    Circle()
                        .fill(deviceDotColor)
                        .frame(width: 7, height: 7)
                        .animation(.uiEaseOut, value: deviceDotColor)

                    Picker("", selection: $selectedSerial) {
                        if deviceManager.devices.isEmpty {
                            Text("No devices").tag(String?.none)
                        } else {
                            Text("Choose a device").tag(String?.none)
                        }
                        ForEach(deviceManager.devices) { d in
                            Text(d.displayName).tag(String?.some(d.serial))
                        }
                    }
                    .labelsHidden()
                    .disabled(isSessionActive)
                }
                .padding(.leading, 10)
                .padding(.vertical, 2)
                .background(
                    RoundedRectangle(cornerRadius: 7, style: .continuous)
                        .fill(Color.primary.opacity(0.05))
                )

                Button {
                    Task { await deviceManager.refresh() }
                } label: {
                    ZStack {
                        Image(systemName: "arrow.clockwise")
                            .font(.system(size: 12, weight: .medium))
                            .opacity(deviceManager.isRefreshing ? 0 : 1)
                        ProgressView()
                            .controlSize(.small)
                            .opacity(deviceManager.isRefreshing ? 1 : 0)
                    }
                    .animation(.uiEaseOut, value: deviceManager.isRefreshing)
                }
                .buttonStyle(IconButtonStyle())
                .disabled(deviceManager.isRefreshing)
                .help("Refresh device list")

                Button {
                    wifiError = nil
                    showWifiPopover = true
                } label: {
                    Image(systemName: "wifi")
                        .font(.system(size: 12, weight: .medium))
                }
                .buttonStyle(IconButtonStyle())
                .help("Connect over Wi-Fi")
                .popover(isPresented: $showWifiPopover, arrowEdge: .bottom) {
                    wifiPopover
                }
            }

            if deviceManager.devices.isEmpty && !deviceManager.isRefreshing {
                Text("Connect a device over USB and enable USB debugging.")
                    .font(.system(size: 11))
                    .foregroundStyle(.tertiary)
                    .fixedSize(horizontal: false, vertical: true)
                    .transition(.opacity)
            }
        }
        .onChange(of: deviceManager.devices) { newValue in
            if selectedSerial == nil, let first = newValue.first(where: { $0.state == "device" }) {
                selectedSerial = first.serial
            }
        }
    }

    private var wifiPopover: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Connect over Wi-Fi")
                .font(.system(size: 12, weight: .semibold))

            if let serial = selectedUsbSerial {
                Button {
                    Task {
                        wifiError = nil
                        switch await deviceManager.switchToWifi(serial: serial) {
                        case .success(let newSerial):
                            selectedSerial = newSerial
                            showWifiPopover = false
                        case .failure(let error):
                            wifiError = error.message
                        }
                    }
                } label: {
                    Label("Switch selected device to Wi-Fi", systemImage: "arrow.right.arrow.left")
                }
                .buttonStyle(SecondaryButtonStyle())
                .disabled(deviceManager.isConnectingWifi)

                Text("Enables TCP mode on the USB device, then reconnects wirelessly. Keep the cable until it's done.")
                    .font(.system(size: 10))
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)

                Divider()
            }

            Text("Or connect to a device already in TCP mode:")
                .font(.system(size: 10))
                .foregroundStyle(.secondary)

            HStack(spacing: 6) {
                TextField("192.168.1.42:5555", text: $wifiAddress)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 11, design: .monospaced))
                    .onSubmit { connectToManualAddress() }

                Button("Connect") { connectToManualAddress() }
                    .disabled(wifiAddress.isEmpty || deviceManager.isConnectingWifi)
            }

            if deviceManager.isConnectingWifi {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.small)
                    Text("Connecting…")
                        .font(.system(size: 10))
                        .foregroundStyle(.secondary)
                }
                .transition(.opacity)
            }

            if let wifiError {
                Text(wifiError)
                    .font(.system(size: 10))
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
                    .textSelection(.enabled)
                    .transition(.opacity)
            }
        }
        .animation(.uiEaseOut, value: deviceManager.isConnectingWifi)
        .padding(14)
        .frame(width: 260)
    }

    /// Serial of the selected device if it's a USB connection (wireless
    /// serials look like "ip:port").
    private var selectedUsbSerial: String? {
        guard let serial = selectedSerial, !serial.contains(":") else { return nil }
        return serial
    }

    private func connectToManualAddress() {
        let address = wifiAddress.trimmingCharacters(in: .whitespaces)
        guard !address.isEmpty else { return }
        Task {
            wifiError = nil
            switch await deviceManager.connect(to: address) {
            case .success(let serial):
                selectedSerial = serial
                showWifiPopover = false
            case .failure(let error):
                wifiError = error.message
            }
        }
    }

    private var deviceDotColor: Color {
        guard let serial = selectedSerial,
              let device = deviceManager.devices.first(where: { $0.serial == serial }) else {
            return Color.secondary.opacity(0.4)
        }
        switch device.state {
        case "device": return .green
        case "unauthorized": return .orange
        default: return .red
        }
    }

    private var optionsSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            sectionLabel("Options")

            VStack(spacing: 0) {
                optionRow("Stay awake", icon: "sun.max", isOn: $stayAwake)
                optionDivider
                optionRow("Turn screen off", icon: "moon", isOn: $turnScreenOff)
                    .disabled(audioOnly)
                optionDivider
                optionRow("Audio only", icon: "speaker.wave.2", isOn: $audioOnly)
                    .disabled(videoOnly)
                optionDivider
                optionRow("Video only", icon: "video", isOn: $videoOnly)
                    .disabled(audioOnly)
                optionDivider
                agentServiceRow
                pluginInstallRow
            }
            .background(
                RoundedRectangle(cornerRadius: 9, style: .continuous)
                    .fill(Color.primary.opacity(0.05))
            )
        }
    }

    private var optionDivider: some View {
        Divider().opacity(0.4).padding(.leading, 34)
    }

    private var agentServiceRow: some View {
        VStack(alignment: .leading, spacing: 8) {
            Toggle(isOn: $agentServiceEnabled) {
                HStack(spacing: 8) {
                    Image(systemName: "antenna.radiowaves.left.and.right")
                        .font(.system(size: 12))
                        .foregroundStyle(.secondary)
                        .frame(width: 18)
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Agent service")
                            .font(.system(size: 12))
                        Text("Cursor/Codex plugin · :9477")
                            .font(.system(size: 9))
                            .foregroundStyle(.tertiary)
                    }
                }
            }
            .toggleStyle(.switch)
            .disabled(!isConnected)

            Toggle(isOn: $agentServiceAutoEnable) {
                Text("Auto-enable on Connect")
                    .font(.system(size: 11))
                    .foregroundStyle(.secondary)
            }
            .toggleStyle(.switch)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    private var pluginInstallRow: some View {
        VStack(alignment: .leading, spacing: 6) {
            Button {
                Task { await pluginInstaller.install() }
            } label: {
                HStack(spacing: 8) {
                    if pluginInstaller.isRunning {
                        ProgressView()
                            .controlSize(.small)
                            .frame(width: 18)
                    } else {
                        Image(systemName: "puzzlepiece.extension")
                            .font(.system(size: 12))
                            .foregroundStyle(.secondary)
                            .frame(width: 18)
                    }
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Install Phone Agent plugin")
                            .font(.system(size: 12))
                        Text("Cursor / Codex · install.sh")
                            .font(.system(size: 9))
                            .foregroundStyle(.tertiary)
                    }
                    Spacer()
                }
            }
            .buttonStyle(.plain)
            .disabled(pluginInstaller.isRunning)

            if !pluginInstaller.lastMessage.isEmpty {
                Text(pluginInstaller.lastMessage)
                    .font(.system(size: 9))
                    .foregroundStyle(pluginInstaller.lastSuccess == false ? .red : .secondary)
                    .lineLimit(4)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    private func optionRow(_ title: String, icon: String, isOn: Binding<Bool>) -> some View {
        HStack(spacing: 10) {
            Image(systemName: icon)
                .font(.system(size: 12))
                .foregroundStyle(.secondary)
                .frame(width: 16)
            Text(title)
                .font(.system(size: 12))
            Spacer()
            Toggle("", isOn: isOn)
                .labelsHidden()
                .toggleStyle(.switch)
                .controlSize(.mini)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 7)
        .contentShape(Rectangle())
    }

    private func sectionLabel(_ text: String) -> some View {
        Text(text.uppercased())
            .font(.system(size: 10, weight: .semibold))
            .foregroundStyle(.secondary)
            .kerning(0.6)
    }

    private var statusChip: some View {
        Group {
            if case .connected(let meta) = session.state {
                HStack(spacing: 8) {
                    Circle()
                        .fill(Color.green)
                        .frame(width: 7, height: 7)
                    VStack(alignment: .leading, spacing: 1) {
                        Text(meta.deviceName)
                            .font(.system(size: 12, weight: .medium))
                            .lineLimit(1)
                        Text("\(String(meta.width))×\(String(meta.height)) · \(meta.videoCodec?.label ?? "unknown codec")")
                            .font(.system(size: 10))
                            .foregroundStyle(.secondary)
                    }
                    Spacer()
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 8)
                .background(
                    RoundedRectangle(cornerRadius: 9, style: .continuous)
                        .fill(Color.green.opacity(0.08))
                )
                .transition(reduceMotion ? .opacity : .opacity.combined(with: .scale(scale: 0.97)))
            }
        }
        .animation(.uiEaseOut, value: isConnected)
    }

    private var actionBar: some View {
        VStack(spacing: 8) {
            if isSessionActive {
                Button {
                    pasteClipboard()
                } label: {
                    Label("Paste Clipboard", systemImage: "doc.on.clipboard")
                }
                .buttonStyle(SecondaryButtonStyle())
                .disabled(!isConnected)

                Button {
                    session.stop()
                } label: {
                    Label("Stop", systemImage: "stop.fill")
                }
                .buttonStyle(PrimaryButtonStyle(tint: .red))
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
                }
                .buttonStyle(PrimaryButtonStyle())
                .disabled(selectedSerial == nil)
            }
        }
        .animation(reduceMotion ? nil : .uiEaseOut, value: isSessionActive)
    }

    private var isSessionActive: Bool {
        switch session.state {
        case .idle, .failed: return false
        default: return true
        }
    }

    // MARK: Mirror pane

    private var mirrorPane: some View {
        ZStack {
            // Always-present sample-buffer surface; decoder pushes frames to
            // its layer as soon as the session is connected.
            MirrorView(
                onLayerReady: { layer in session.attach(displayLayer: layer) },
                onPointerEvent: { event in handlePointerEvent(event) },
                onKeyEvent: { event in handleKeyEvent(event) }
            )

            if !isConnected {
                overlay
                    .transition(reduceMotion ? .opacity : .opacity)
            }
        }
        .animation(.easeOut(duration: 0.35), value: isConnected)
    }

    @ViewBuilder
    private var overlay: some View {
        ZStack {
            LinearGradient(
                colors: [Color(white: 0.09), Color(white: 0.05)],
                startPoint: .top, endPoint: .bottom
            )

            VStack(spacing: 0) {
                Spacer()

                switch session.state {
                case .idle:
                    idleState
                case .failed(let message):
                    failedState(message)
                default:
                    connectingState
                }

                Spacer()

                logDisclosure
            }
            .padding(16)
        }
    }

    private var idleState: some View {
        VStack(spacing: 12) {
            Image(systemName: "iphone.gen3")
                .font(.system(size: 40, weight: .thin))
                .foregroundStyle(.white.opacity(0.25))
            Text("Ready to mirror")
                .font(.system(size: 14, weight: .medium))
                .foregroundStyle(.white.opacity(0.85))
            Text("Select a device and press Connect")
                .font(.system(size: 11))
                .foregroundStyle(.white.opacity(0.45))
        }
        .transition(stateTransition)
    }

    private func failedState(_ message: String) -> some View {
        VStack(spacing: 12) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 32, weight: .light))
                .foregroundStyle(.orange)
            Text("Connection failed")
                .font(.system(size: 14, weight: .medium))
                .foregroundStyle(.white.opacity(0.85))
            Text(message)
                .font(.system(size: 11))
                .foregroundStyle(.white.opacity(0.55))
                .multilineTextAlignment(.center)
                .lineLimit(4)
                .textSelection(.enabled)
        }
        .padding(.horizontal, 12)
        .transition(stateTransition)
    }

    private static let connectionSteps = [
        "Pushing server to device",
        "Setting up port forward",
        "Starting server",
        "Handshaking",
    ]

    private var currentStepIndex: Int {
        switch session.state {
        case .pushing: return 0
        case .forwarding: return 1
        case .startingServer: return 2
        case .handshaking: return 3
        default: return -1
        }
    }

    private var connectingState: some View {
        VStack(alignment: .leading, spacing: 14) {
            ForEach(Array(Self.connectionSteps.enumerated()), id: \.offset) { index, title in
                HStack(spacing: 10) {
                    ZStack {
                        if index < currentStepIndex {
                            Image(systemName: "checkmark.circle.fill")
                                .font(.system(size: 14))
                                .foregroundStyle(.green)
                        } else if index == currentStepIndex {
                            ProgressView()
                                .controlSize(.small)
                                .tint(.white)
                        } else {
                            Circle()
                                .strokeBorder(.white.opacity(0.2), lineWidth: 1.5)
                                .frame(width: 13, height: 13)
                        }
                    }
                    .frame(width: 18, height: 18)

                    Text(title)
                        .font(.system(size: 12))
                        .foregroundStyle(
                            index <= currentStepIndex ? .white.opacity(0.85) : .white.opacity(0.35)
                        )
                }
                .animation(.uiEaseOut, value: currentStepIndex)
            }
        }
        .transition(stateTransition)
    }

    private var stateTransition: AnyTransition {
        reduceMotion ? .opacity : .opacity.combined(with: .scale(scale: 0.97))
    }

    private var logDisclosure: some View {
        VStack(alignment: .leading, spacing: 6) {
            Button {
                withAnimation(reduceMotion ? nil : .uiEaseOut) { showLog.toggle() }
            } label: {
                HStack(spacing: 5) {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 8, weight: .bold))
                        .rotationEffect(.degrees(showLog ? 90 : 0))
                    Text("Details")
                        .font(.system(size: 10, weight: .medium))
                }
                .foregroundStyle(.white.opacity(0.4))
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)

            if showLog {
                ScrollView {
                    Text(session.log.isEmpty ? "(no activity)" : session.log)
                        .font(.system(size: 9, design: .monospaced))
                        .foregroundStyle(.white.opacity(0.55))
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                }
                .frame(height: 140)
                .padding(8)
                .background(
                    RoundedRectangle(cornerRadius: 7, style: .continuous)
                        .fill(.white.opacity(0.04))
                )
                .transition(.opacity)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var isConnected: Bool {
        if case .connected = session.state { return true }
        return false
    }

    // MARK: Input forwarding

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
        // Punctuation and shifted symbols (!, @, #, …) have no direct Android
        // keycode; inject them as text on keyDown only.
        if let text = AndroidKeycode.textForInjection(keyEvent.event) {
            if keyEvent.kind == .down {
                session.sendText(text)
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
}
