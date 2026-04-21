import SwiftUI
import AppKit
import AVFoundation

/// NSView whose backing layer is an `AVSampleBufferDisplayLayer`. Also acts as
/// the capture surface for mouse + scroll events which we forward to scrcpy's
/// control socket via a delegate callback.
final class SampleBufferHostView: NSView {
    let displayLayer = AVSampleBufferDisplayLayer()

    /// Called for each pointer / scroll event; the session maps coordinates to
    /// the device and emits a ControlMessage. `isPressed` mirrors the left
    /// button state for touch DOWN/MOVE/UP semantics.
    var onPointerEvent: ((PointerEvent) -> Void)?
    var onKeyEvent: ((KeyEvent) -> Void)?

    struct PointerEvent {
        enum Kind { case down, move, up, scroll(hScroll: Float, vScroll: Float) }
        let kind: Kind
        let location: CGPoint
        let viewSize: CGSize
    }

    struct KeyEvent {
        enum Kind { case down, up }
        let kind: Kind
        let event: NSEvent
    }

    override init(frame frameRect: NSRect) {
        super.init(frame: frameRect)
        wantsLayer = true
        layerContentsRedrawPolicy = .onSetNeedsDisplay
    }

    required init?(coder: NSCoder) { fatalError("not implemented") }

    override func makeBackingLayer() -> CALayer {
        displayLayer.backgroundColor = NSColor.black.cgColor
        displayLayer.videoGravity = .resizeAspect
        return displayLayer
    }

    // Match Android's top-left origin so a click at (0,0) maps to the top-left
    // of the mirrored screen. Without this, y grows upward in AppKit.
    override var isFlipped: Bool { true }

    // Let clicks land even when the window isn't key — feels native when the
    // user has just given the SwiftUI controls focus but taps into the mirror.
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }

    override var acceptsFirstResponder: Bool { true }

    // MARK: - Mouse events

    override func mouseDown(with event: NSEvent) {
        window?.makeFirstResponder(self)
        emit(.down, event: event)
    }

    override func mouseDragged(with event: NSEvent) {
        emit(.move, event: event)
    }

    override func mouseUp(with event: NSEvent) {
        emit(.up, event: event)
    }

    override func scrollWheel(with event: NSEvent) {
        let p = convert(event.locationInWindow, from: nil)
        onPointerEvent?(PointerEvent(
            kind: .scroll(
                hScroll: Float(event.scrollingDeltaX),
                vScroll: Float(event.scrollingDeltaY)
            ),
            location: p,
            viewSize: bounds.size
        ))
    }

    override func keyDown(with event: NSEvent) {
        onKeyEvent?(KeyEvent(kind: .down, event: event))
    }

    override func keyUp(with event: NSEvent) {
        onKeyEvent?(KeyEvent(kind: .up, event: event))
    }

    private func emit(_ kind: PointerEvent.Kind, event: NSEvent) {
        let p = convert(event.locationInWindow, from: nil)
        onPointerEvent?(PointerEvent(kind: kind, location: p, viewSize: bounds.size))
    }
}

struct MirrorView: NSViewRepresentable {
    final class Coordinator {
        weak var hostView: SampleBufferHostView?
    }

    var onLayerReady: (AVSampleBufferDisplayLayer) -> Void
    var onPointerEvent: (SampleBufferHostView.PointerEvent) -> Void
    var onKeyEvent: (SampleBufferHostView.KeyEvent) -> Void

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> SampleBufferHostView {
        let v = SampleBufferHostView(frame: .zero)
        v.onPointerEvent = onPointerEvent
        v.onKeyEvent = onKeyEvent
        context.coordinator.hostView = v
        DispatchQueue.main.async { [layer = v.displayLayer] in
            onLayerReady(layer)
        }
        return v
    }

    func updateNSView(_ nsView: SampleBufferHostView, context: Context) {
        nsView.onPointerEvent = onPointerEvent
        nsView.onKeyEvent = onKeyEvent
    }
}
