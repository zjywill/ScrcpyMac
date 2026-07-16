import Foundation

struct AdbDevice: Identifiable, Hashable {
    let id: String
    let serial: String
    let state: String
    let model: String?

    var displayName: String {
        if let model, !model.isEmpty {
            return "\(model) · \(serial)"
        }
        return serial
    }
}

@MainActor
final class DeviceManager: ObservableObject {
    @Published var devices: [AdbDevice] = []
    @Published var isRefreshing = false
    @Published var lastError: String?
    @Published var isConnectingWifi = false

    private let toolchain: Toolchain

    init(toolchain: Toolchain) {
        self.toolchain = toolchain
    }

    /// Restart adbd in TCP mode on the given USB device, then connect to it
    /// over Wi-Fi. Returns the new wireless serial ("ip:5555") on success.
    func switchToWifi(serial: String) async -> Result<String, WifiError> {
        isConnectingWifi = true
        defer { isConnectingWifi = false }

        guard let ip = await deviceWifiAddress(serial: serial) else {
            return .failure(.noWifiAddress)
        }
        do {
            _ = try await runAdb(["-s", serial, "tcpip", "5555"])
        } catch {
            return .failure(.adbFailed(error.localizedDescription))
        }
        // adbd restarts in TCP mode; give it a moment before connecting.
        try? await Task.sleep(nanoseconds: 1_500_000_000)
        return await connect(to: "\(ip):5555")
    }

    /// `adb connect` to an explicit "ip[:port]" address.
    func connect(to address: String) async -> Result<String, WifiError> {
        isConnectingWifi = true
        defer { isConnectingWifi = false }

        let target = address.contains(":") ? address : "\(address):5555"
        do {
            let output = try await runAdb(["connect", target])
            guard output.contains("connected") && !output.contains("cannot") &&
                  !output.contains("failed") else {
                return .failure(.connectFailed(output.trimmingCharacters(in: .whitespacesAndNewlines)))
            }
        } catch {
            return .failure(.adbFailed(error.localizedDescription))
        }
        await refresh()
        return .success(target)
    }

    /// The device's Wi-Fi (wlan) IPv4 address, from `ip route` on the device.
    private func deviceWifiAddress(serial: String) async -> String? {
        guard let output = try? await runAdb(["-s", serial, "shell", "ip", "route"]) else {
            return nil
        }
        // e.g. "192.168.1.0/24 dev wlan0 proto kernel scope link src 192.168.1.42"
        for line in output.split(separator: "\n") where line.contains("wlan") {
            let parts = line.split(whereSeparator: \.isWhitespace).map(String.init)
            if let srcIndex = parts.firstIndex(of: "src"), srcIndex + 1 < parts.count {
                return parts[srcIndex + 1]
            }
        }
        return nil
    }

    private func runAdb(_ args: [String]) async throws -> String {
        try await Task.detached(priority: .userInitiated) { [toolchain] in
            try ProcessRunner.runCapturing(url: toolchain.adbURL, args: args)
        }.value
    }

    enum WifiError: Error {
        case noWifiAddress
        case adbFailed(String)
        case connectFailed(String)

        var message: String {
            switch self {
            case .noWifiAddress:
                return "Couldn't find the device's Wi-Fi address. Is it on the same network?"
            case .adbFailed(let msg):
                return msg
            case .connectFailed(let msg):
                return msg.isEmpty ? "adb connect failed" : msg
            }
        }
    }

    func refresh() async {
        isRefreshing = true
        defer { isRefreshing = false }
        do {
            let output = try await Task.detached(priority: .userInitiated) { [toolchain] in
                try ProcessRunner.runCapturing(url: toolchain.adbURL, args: ["devices", "-l"])
            }.value
            devices = Self.parse(output)
            lastError = nil
        } catch {
            devices = []
            lastError = "\(error.localizedDescription)"
        }
    }

    static func parse(_ output: String) -> [AdbDevice] {
        var result: [AdbDevice] = []
        for rawLine in output.split(whereSeparator: { $0 == "\n" || $0 == "\r" }) {
            let line = rawLine.trimmingCharacters(in: .whitespaces)
            if line.isEmpty { continue }
            if line.hasPrefix("List of devices") { continue }
            if line.hasPrefix("*") { continue }
            let parts = line.split(whereSeparator: \.isWhitespace).map(String.init)
            guard parts.count >= 2 else { continue }
            let serial = parts[0]
            let state = parts[1]
            var model: String?
            for tag in parts.dropFirst(2) {
                if tag.hasPrefix("model:") {
                    model = String(tag.dropFirst("model:".count))
                        .replacingOccurrences(of: "_", with: " ")
                }
            }
            result.append(AdbDevice(id: serial, serial: serial, state: state, model: model))
        }
        return result
    }
}
