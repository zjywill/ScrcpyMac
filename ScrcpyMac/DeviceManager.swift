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

    private let toolchain: Toolchain

    init(toolchain: Toolchain) {
        self.toolchain = toolchain
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
