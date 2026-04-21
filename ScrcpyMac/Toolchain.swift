import Foundation

/// Locates the external tools we still need as subprocesses. The scrcpy-server
/// jar is pushed and run on the Android device via adb; the actual mirror
/// client is written natively in Swift (VideoToolbox + AVSampleBufferDisplayLayer),
/// so no desktop scrcpy binary / SDL / ffmpeg is required.
struct Toolchain {
    let adbURL: URL
    let scrcpyServerJarURL: URL
    let source: Source

    enum Source { case bundled, system }

    static func detect() -> Toolchain {
        if let bundled = bundled() { return bundled }
        return system()
    }

    private static func bundled() -> Toolchain? {
        guard let res = Bundle.main.resourceURL else { return nil }
        let adb = res.appendingPathComponent("bin/adb")
        let jar = res.appendingPathComponent("share/scrcpy-server")
        let fm = FileManager.default
        guard fm.isExecutableFile(atPath: adb.path),
              fm.fileExists(atPath: jar.path) else {
            return nil
        }
        return Toolchain(adbURL: adb, scrcpyServerJarURL: jar, source: .bundled)
    }

    private static func system() -> Toolchain {
        let fm = FileManager.default
        let adbCandidates = [
            "\(NSHomeDirectory())/Library/Android/sdk/platform-tools/adb",
            "/opt/homebrew/bin/adb",
            "/usr/local/bin/adb",
        ]
        let adb = adbCandidates.first { fm.isExecutableFile(atPath: $0) } ?? adbCandidates[0]

        // Scrcpy's Homebrew install ships the server jar alongside the binary.
        let jarCandidates = [
            "/opt/homebrew/share/scrcpy/scrcpy-server",
            "/usr/local/share/scrcpy/scrcpy-server",
        ]
        let jar = jarCandidates.first { fm.fileExists(atPath: $0) } ?? jarCandidates[0]

        return Toolchain(
            adbURL: URL(fileURLWithPath: adb),
            scrcpyServerJarURL: URL(fileURLWithPath: jar),
            source: .system
        )
    }

    var descriptionLine: String {
        switch source {
        case .bundled: return "adb: bundled · server: bundled"
        case .system:  return "adb: \(adbURL.path)"
        }
    }
}
