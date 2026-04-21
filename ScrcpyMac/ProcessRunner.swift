import Foundation

enum ProcessRunner {
    struct Failure: Error, LocalizedError {
        let exitCode: Int32
        let stderr: String
        var errorDescription: String? {
            let trimmed = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
            if trimmed.isEmpty { return "Process exited with code \(exitCode)" }
            return "Exit \(exitCode): \(trimmed)"
        }
    }

    /// Runs a process synchronously, waits for it, returns stdout.
    static func runCapturing(url: URL, args: [String], env: [String: String]? = nil) throws -> String {
        let proc = Process()
        proc.executableURL = url
        proc.arguments = args
        if let env { proc.environment = env }
        let out = Pipe()
        let err = Pipe()
        proc.standardOutput = out
        proc.standardError = err
        try proc.run()
        proc.waitUntilExit()
        let outData = out.fileHandleForReading.readDataToEndOfFile()
        let errData = err.fileHandleForReading.readDataToEndOfFile()
        if proc.terminationStatus != 0 {
            throw Failure(
                exitCode: proc.terminationStatus,
                stderr: String(data: errData, encoding: .utf8) ?? ""
            )
        }
        return String(data: outData, encoding: .utf8) ?? ""
    }
}
