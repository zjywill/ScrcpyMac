import Foundation

/// Wire-format constants + parsers for the scrcpy server protocol (v3.3.4).
/// The authoritative spec is Genymobile/scrcpy under server/src/main/java/...
enum ScrcpyProtocol {
    /// Version string passed as the first argument when invoking the server
    /// via `app_process`. Must match the server jar's build version.
    static let version = "3.3.4"

    /// Fixed-size device name field (UTF-8, zero-padded / zero-terminated).
    static let deviceNameFieldLength = 64

    /// Per-frame header emitted before each video packet (when sendFrameMeta).
    /// 8 bytes pts+flags (big-endian) + 4 bytes packet size (big-endian).
    static let frameHeaderLength = 12

    /// Top bit of the pts field: this is a codec-config packet (e.g. SPS/PPS).
    static let packetFlagConfig: UInt64 = 1 << 63
    /// Second-highest bit: this is a keyframe.
    static let packetFlagKeyFrame: UInt64 = 1 << 62
    /// Mask to extract the raw PTS value.
    static let packetPtsMask: UInt64 = (1 << 62) - 1

    /// Video codec ids the server can announce. Four-char big-endian tags.
    enum VideoCodec: UInt32 {
        case h264 = 0x68323634   // "h264"
        case h265 = 0x68323635   // "h265"
        case av1  = 0x00617631   // "\0av1"

        var label: String {
            switch self {
            case .h264: return "h264"
            case .h265: return "h265"
            case .av1: return "av1"
            }
        }
    }
}

/// Device metadata the server sends on the first (video) socket right after
/// the optional one-byte handshake: a 64-byte UTF-8 name field followed by the
/// 12-byte video codec header (codec id, width, height — all big-endian u32).
struct ScrcpyDeviceMeta {
    let deviceName: String
    let videoCodec: ScrcpyProtocol.VideoCodec?
    let rawCodecId: UInt32
    let width: Int
    let height: Int
}

/// Per-frame header preceding each video payload on the video socket.
struct ScrcpyVideoPacketHeader {
    let isConfig: Bool
    let isKeyFrame: Bool
    let pts: UInt64     // microseconds, raw scale from MediaCodec
    let size: Int       // payload byte length, tail of this header

    static func parse(_ bytes: Data) -> ScrcpyVideoPacketHeader? {
        guard bytes.count >= ScrcpyProtocol.frameHeaderLength else { return nil }
        let ptsField = bytes.readBigEndian(UInt64.self, at: 0)
        let size = bytes.readBigEndian(UInt32.self, at: 8)
        return ScrcpyVideoPacketHeader(
            isConfig: (ptsField & ScrcpyProtocol.packetFlagConfig) != 0,
            isKeyFrame: (ptsField & ScrcpyProtocol.packetFlagKeyFrame) != 0,
            pts: ptsField & ScrcpyProtocol.packetPtsMask,
            size: Int(size)
        )
    }
}

extension Data {
    /// Read a big-endian fixed-width integer at a byte offset. Caller must
    /// ensure the range is in bounds.
    func readBigEndian<T: FixedWidthInteger>(_ type: T.Type, at offset: Int) -> T {
        var value: T = 0
        let byteCount = MemoryLayout<T>.size
        for i in 0..<byteCount {
            value = (value << 8) | T(self[self.startIndex + offset + i])
        }
        return value
    }
}
