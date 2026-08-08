import Foundation
import AVFoundation
import CoreMedia

/// Feeds scrcpy's H.264 Annex-B stream into an `AVSampleBufferDisplayLayer`.
///
/// Flow:
/// 1. First packet has the `CONFIG` flag set — it carries SPS + PPS as two
///    Annex-B NAL units. We extract them and build a `CMVideoFormatDescription`.
/// 2. Subsequent packets carry raw coded-slice NAL units in Annex-B form. We
///    convert to AVCC (4-byte big-endian length prefix per NALU), wrap in a
///    `CMBlockBuffer`, build a `CMSampleBuffer`, and `enqueue` it on the layer.
///    The layer is set to display-immediately, so timestamps only need to be
///    monotonic (we feed scrcpy's microsecond pts).
final class H264Decoder: @unchecked Sendable {
    weak var displayLayer: AVSampleBufferDisplayLayer?

    private var formatDescription: CMVideoFormatDescription?

    func ingestConfig(_ data: Data) {
        let nalus = Self.splitAnnexB(data)
        var sps: Data?
        var pps: Data?
        for n in nalus {
            guard let first = n.first else { continue }
            let type = first & 0x1F
            if type == 7 { sps = n }
            else if type == 8 { pps = n }
        }
        guard let sps, let pps else {
            NSLog("[decoder] config packet missing SPS/PPS: %d NALUs", nalus.count)
            return
        }

        var fd: CMVideoFormatDescription?
        sps.withUnsafeBytes { spsBuf in
            pps.withUnsafeBytes { ppsBuf in
                guard let spsPtr = spsBuf.bindMemory(to: UInt8.self).baseAddress,
                      let ppsPtr = ppsBuf.bindMemory(to: UInt8.self).baseAddress else { return }
                let pointers: [UnsafePointer<UInt8>] = [spsPtr, ppsPtr]
                let sizes: [Int] = [sps.count, pps.count]
                let status = pointers.withUnsafeBufferPointer { ptrBuf in
                    sizes.withUnsafeBufferPointer { sizeBuf in
                        CMVideoFormatDescriptionCreateFromH264ParameterSets(
                            allocator: kCFAllocatorDefault,
                            parameterSetCount: 2,
                            parameterSetPointers: ptrBuf.baseAddress!,
                            parameterSetSizes: sizeBuf.baseAddress!,
                            nalUnitHeaderLength: 4,
                            formatDescriptionOut: &fd
                        )
                    }
                }
                if status != noErr {
                    NSLog("[decoder] CMVideoFormatDescription create failed: %d", status)
                }
            }
        }
        self.formatDescription = fd
    }

    func ingestFrame(_ data: Data, pts: UInt64, keyFrame: Bool) {
        guard let formatDescription, let layer = displayLayer else { return }

        // Build AVCC payload: for each NALU, 4-byte big-endian length + bytes.
        let nalus = Self.splitAnnexB(data)
        var avcc = Data()
        avcc.reserveCapacity(data.count + nalus.count * 4)
        for n in nalus {
            var lenBE = UInt32(n.count).bigEndian
            withUnsafeBytes(of: &lenBE) { avcc.append(contentsOf: $0) }
            avcc.append(n)
        }

        // Wrap in a CMBlockBuffer (let CM allocate + copy; simpler than
        // keeping our Data alive for the sample buffer's lifetime).
        var blockBuffer: CMBlockBuffer?
        var status = CMBlockBufferCreateWithMemoryBlock(
            allocator: kCFAllocatorDefault,
            memoryBlock: nil,
            blockLength: avcc.count,
            blockAllocator: nil,
            customBlockSource: nil,
            offsetToData: 0,
            dataLength: avcc.count,
            flags: 0,
            blockBufferOut: &blockBuffer
        )
        guard status == noErr, let blockBuffer else {
            NSLog("[decoder] CMBlockBufferCreate failed: %d", status); return
        }
        status = avcc.withUnsafeBytes { buf in
            CMBlockBufferReplaceDataBytes(
                with: buf.baseAddress!,
                blockBuffer: blockBuffer,
                offsetIntoDestination: 0,
                dataLength: buf.count
            )
        }
        guard status == noErr else {
            NSLog("[decoder] CMBlockBufferReplaceDataBytes failed: %d", status); return
        }

        var sampleBuffer: CMSampleBuffer?
        var timing = CMSampleTimingInfo(
            duration: .invalid,
            presentationTimeStamp: CMTime(value: CMTimeValue(pts), timescale: 1_000_000),
            decodeTimeStamp: .invalid
        )
        var sampleSize = avcc.count
        status = CMSampleBufferCreateReady(
            allocator: kCFAllocatorDefault,
            dataBuffer: blockBuffer,
            formatDescription: formatDescription,
            sampleCount: 1,
            sampleTimingEntryCount: 1,
            sampleTimingArray: &timing,
            sampleSizeEntryCount: 1,
            sampleSizeArray: &sampleSize,
            sampleBufferOut: &sampleBuffer
        )
        guard status == noErr, let sampleBuffer else {
            NSLog("[decoder] CMSampleBufferCreate failed: %d", status); return
        }

        // Mark as display-immediately so the layer doesn't wait on a host clock.
        if let attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, createIfNecessary: true) as? [NSMutableDictionary],
           let first = attachments.first {
            first[kCMSampleAttachmentKey_DisplayImmediately] = true
            if !keyFrame {
                first[kCMSampleAttachmentKey_NotSync] = true
            }
        }

        layer.enqueue(sampleBuffer)
    }

    func reset() {
        formatDescription = nil
        displayLayer?.flushAndRemoveImage()
    }

    // MARK: - Annex B parsing

    /// Split an Annex-B buffer into individual NAL-unit payloads (without the
    /// 0x000001 / 0x00000001 start codes). Scans byte-by-byte; scrcpy frames
    /// are small enough (tens–hundreds of KB) that a fancy Boyer-Moore isn't
    /// necessary here.
    private static func splitAnnexB(_ data: Data) -> [Data] {
        var nalus: [Data] = []
        let n = data.count
        var i = 0
        var naluStart = -1

        func commit(end: Int) {
            if naluStart >= 0 && end > naluStart {
                nalus.append(data.subdata(in: naluStart..<end))
            }
        }

        while i + 2 < n {
            let isStart4 = (i + 3 < n)
                && data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 0 && data[i + 3] == 1
            let isStart3 = data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 1
            if isStart4 {
                commit(end: i)
                i += 4
                naluStart = i
            } else if isStart3 {
                commit(end: i)
                i += 3
                naluStart = i
            } else {
                i += 1
            }
        }
        commit(end: n)
        return nalus
    }
}
