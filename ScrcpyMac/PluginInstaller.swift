import Foundation

/// Locates and installs the ScrcpyMac Phone Agent plugin (`install.sh`).
@MainActor
final class PluginInstaller: ObservableObject {
    @Published private(set) var isRunning = false
    @Published private(set) var lastMessage: String = ""
    @Published private(set) var lastSuccess: Bool?

    static func pluginRoot() -> URL? {
        if let override = ProcessInfo.processInfo.environment["SCRCPYMAC_PLUGIN_ROOT"] {
            let url = URL(fileURLWithPath: override, isDirectory: true)
            if FileManager.default.fileExists(atPath: url.appendingPathComponent("scripts/install.sh").path) {
                return url
            }
        }

        if let res = Bundle.main.resourceURL {
            let bundled = res.appendingPathComponent("plugins/scrcpymac-phone-agent", isDirectory: true)
            if FileManager.default.fileExists(atPath: bundled.appendingPathComponent("scripts/install.sh").path) {
                // Never run install.sh inside the signed .app bundle — it chmods
                // files and downloads adb binaries, which would break the
                // codesign seal. Stage a copy under Application Support instead.
                return stagedPluginRoot(from: bundled)
            }
        }

        let devCandidates = [
            Bundle.main.bundleURL
                .deletingLastPathComponent()
                .deletingLastPathComponent()
                .deletingLastPathComponent()
                .deletingLastPathComponent()
                .appendingPathComponent("plugins/scrcpymac-phone-agent", isDirectory: true),
            URL(fileURLWithPath: #file)
                .deletingLastPathComponent()
                .deletingLastPathComponent()
                .appendingPathComponent("plugins/scrcpymac-phone-agent", isDirectory: true),
        ]
        for url in devCandidates {
            if FileManager.default.fileExists(atPath: url.appendingPathComponent("scripts/install.sh").path) {
                return url
            }
        }

        let support = supportPluginRoot()
        if FileManager.default.fileExists(atPath: support.appendingPathComponent("scripts/install.sh").path) {
            return support
        }
        return nil
    }

    private static func supportPluginRoot() -> URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/ScrcpyMac/plugins/scrcpymac-phone-agent", isDirectory: true)
    }

    /// Copies the read-only bundled plugin to a user-writable location so the
    /// install script never mutates the signed app bundle.
    private static func stagedPluginRoot(from bundled: URL) -> URL? {
        let fm = FileManager.default
        let dest = supportPluginRoot()
        do {
            try fm.createDirectory(
                at: dest.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            if fm.fileExists(atPath: dest.path) {
                try fm.removeItem(at: dest)
            }
            try fm.copyItem(at: bundled, to: dest)
            return dest
        } catch {
            NSLog("[plugin] failed to stage plugin outside app bundle: %@", "\(error)")
            return nil
        }
    }

    func install() async {
        guard !isRunning else { return }
        guard let root = Self.pluginRoot() else {
            lastSuccess = false
            lastMessage = "Plugin not found. Clone scrcpyMac or set SCRCPYMAC_PLUGIN_ROOT."
            return
        }

        let script = root.appendingPathComponent("scripts/install.sh")
        isRunning = true
        lastMessage = "Installing Phone Agent plugin…"
        lastSuccess = nil
        defer { isRunning = false }

        do {
            let output = try await Task.detached(priority: .userInitiated) {
                try Self.runInstallScript(at: script, pluginRoot: root)
            }.value
            lastSuccess = true
            let tail = output.split(separator: "\n").suffix(4).joined(separator: "\n")
            lastMessage = tail.isEmpty ? "Plugin installed. Reload Cursor / Codex." : String(tail)
        } catch {
            lastSuccess = false
            lastMessage = (error as? LocalizedError)?.errorDescription ?? "\(error)"
        }
    }

    private static func runInstallScript(at script: URL, pluginRoot: URL) throws -> String {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/bin/bash")
        proc.arguments = [script.path]
        proc.currentDirectoryURL = pluginRoot
        var env = ProcessInfo.processInfo.environment
        env["PHONE_AGENT_ROOT"] = pluginRoot.path
        proc.environment = env
        let out = Pipe()
        let err = Pipe()
        proc.standardOutput = out
        proc.standardError = err
        try proc.run()
        // Drain both pipes before waiting: if either pipe buffer fills while
        // we block in waitUntilExit, the child deadlocks on write.
        let errHandle = err.fileHandleForReading
        var errData = Data()
        let errDrained = DispatchSemaphore(value: 0)
        DispatchQueue.global(qos: .utility).async {
            errData = errHandle.readDataToEndOfFile()
            errDrained.signal()
        }
        let outData = out.fileHandleForReading.readDataToEndOfFile()
        errDrained.wait()
        proc.waitUntilExit()
        let outText = String(data: outData, encoding: .utf8) ?? ""
        let errText = String(data: errData, encoding: .utf8) ?? ""
        if proc.terminationStatus != 0 {
            throw ProcessRunner.Failure(exitCode: proc.terminationStatus, stderr: errText.isEmpty ? outText : errText)
        }
        return outText + errText
    }
}
