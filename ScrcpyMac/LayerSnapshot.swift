import AppKit
import QuartzCore

/// Render a `CALayer` (e.g. `AVSampleBufferDisplayLayer`) to PNG bytes.
enum LayerSnapshot {
    static func png(from layer: CALayer) -> Data? {
        let scale = layer.contentsScale > 0 ? layer.contentsScale : NSScreen.main?.backingScaleFactor ?? 2
        let width = Int(layer.bounds.width * scale)
        let height = Int(layer.bounds.height * scale)
        guard width > 0, height > 0 else { return nil }

        guard let colorSpace = CGColorSpace(name: CGColorSpace.sRGB) else { return nil }
        guard let ctx = CGContext(
            data: nil,
            width: width,
            height: height,
            bitsPerComponent: 8,
            bytesPerRow: 0,
            space: colorSpace,
            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue
        ) else { return nil }

        ctx.scaleBy(x: scale, y: scale)
        layer.render(in: ctx)

        guard let image = ctx.makeImage() else { return nil }
        let rep = NSBitmapImageRep(cgImage: image)
        return rep.representation(using: .png, properties: [:])
    }
}
