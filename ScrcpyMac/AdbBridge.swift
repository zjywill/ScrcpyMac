import Foundation

/// Thin Swift wrapper over the `adb` CLI. We only need a handful of subcommands
/// to bring up a scrcpy-server session: push, forward, shell, and `devices`.
struct AdbBridge {
    let adbURL: URL

    /// Run a short-lived adb command that's expected to finish quickly.
    /// Returns stdout; throws on non-zero exit.
    func run(_ args: [String], serial: String? = nil) async throws -> String {
        var full = args
        if let serial { full = ["-s", serial] + full }
        return try await Task.detached(priority: .userInitiated) { [adbURL] in
            try ProcessRunner.runCapturing(url: adbURL, args: full)
        }.value
    }

    /// Launch a long-running adb process (typically `shell ...` starting the
    /// scrcpy server). Returns the live `Process` so the caller can terminate
    /// it and observe output asynchronously.
    func spawn(_ args: [String], serial: String? = nil,
               onStdout: @escaping (String) -> Void,
               onStderr: @escaping (String) -> Void,
               onExit: @escaping (Int32) -> Void) throws -> Process {
        var full = args
        if let serial { full = ["-s", serial] + full }

        let proc = Process()
        proc.executableURL = adbURL
        proc.arguments = full

        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe

        outPipe.fileHandleForReading.readabilityHandler = { h in
            let data = h.availableData
            guard !data.isEmpty, let s = String(data: data, encoding: .utf8) else { return }
            onStdout(s)
        }
        errPipe.fileHandleForReading.readabilityHandler = { h in
            let data = h.availableData
            guard !data.isEmpty, let s = String(data: data, encoding: .utf8) else { return }
            onStderr(s)
        }
        proc.terminationHandler = { p in onExit(p.terminationStatus) }

        try proc.run()
        return proc
    }
}
