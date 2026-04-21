import Foundation

/// Encoders for scrcpy's client-to-device control protocol (v3.3.4). All
/// multi-byte fields are big-endian. Coordinates are in *device* pixel space,
/// not the local view — the position struct tells the server what screen
/// resolution those pixels are in so it can map them for any rotation.
enum ControlMessage {
    // Message type ids (must match ControlMessage.java).
    static let typeInjectKeycode: UInt8     = 0
    static let typeInjectText: UInt8        = 1
    static let typeInjectTouchEvent: UInt8  = 2
    static let typeInjectScrollEvent: UInt8 = 3
    static let typeBackOrScreenOn: UInt8    = 4

    // Android MotionEvent actions scrcpy accepts for touch injection.
    enum TouchAction: UInt8 {
        case down = 0
        case up = 1
        case move = 2
    }

    // Android MotionEvent button flags; we only ever send the primary one.
    static let buttonPrimary: Int32 = 1  // BUTTON_PRIMARY

    // Virtual pointer id scrcpy reserves for mouse input — keeps mouse taps
    // distinguishable from real multitouch pointers.
    static let pointerIdMouse: UInt64 = 0xFFFFFFFFFFFFFFFF  // (long)-1

    /// Build an INJECT_TOUCH_EVENT packet for a single-finger/mouse action.
    /// - Parameters:
    ///   - x, y: position in device pixels (0..screenWidth × 0..screenHeight).
    ///   - screenWidth, screenHeight: device native resolution from handshake.
    static func injectTouch(
        action: TouchAction,
        x: Int32, y: Int32,
        screenWidth: UInt16, screenHeight: UInt16,
        pressure: Float = 1.0,
        buttonsPressed: Bool = true
    ) -> Data {
        var d = Data()
        d.reserveCapacity(32)
        d.appendBE(typeInjectTouchEvent)
        d.appendBE(action.rawValue)
        d.appendBE(pointerIdMouse)
        d.appendBE(UInt32(bitPattern: x))
        d.appendBE(UInt32(bitPattern: y))
        d.appendBE(screenWidth)
        d.appendBE(screenHeight)
        d.appendBE(u16FixedPoint(pressure))
        // actionButton = primary for DOWN/UP transitions; 0 for MOVE.
        let actionButton: Int32 = (action == .move) ? 0 : buttonPrimary
        d.appendBE(UInt32(bitPattern: actionButton))
        // buttons mask = primary while held, 0 after release.
        let buttons: Int32 = buttonsPressed ? buttonPrimary : 0
        d.appendBE(UInt32(bitPattern: buttons))
        return d
    }

    /// Build an INJECT_SCROLL_EVENT packet.
    /// hScroll/vScroll are -16..+16 per scrcpy's convention (the wire uses a
    /// signed 16-bit fixed-point encoding of -1..1 then the server multiplies
    /// by 16).
    static func injectScroll(
        x: Int32, y: Int32,
        screenWidth: UInt16, screenHeight: UInt16,
        hScroll: Float, vScroll: Float
    ) -> Data {
        var d = Data()
        d.reserveCapacity(21)
        d.appendBE(typeInjectScrollEvent)
        d.appendBE(UInt32(bitPattern: x))
        d.appendBE(UInt32(bitPattern: y))
        d.appendBE(screenWidth)
        d.appendBE(screenHeight)
        d.appendBE(i16FixedPoint(hScroll / 16.0))
        d.appendBE(i16FixedPoint(vScroll / 16.0))
        d.appendBE(UInt32(bitPattern: buttonPrimary))  // buttons
        return d
    }

    // MARK: - Fixed-point helpers

    /// scrcpy's u16FixedPoint: float in [0, 1] → uint16 (0x0000..0xFFFF).
    private static func u16FixedPoint(_ f: Float) -> UInt16 {
        let clamped = max(0.0, min(1.0, f))
        return UInt16(clamped * Float(UInt16.max))
    }

    /// scrcpy's i16FixedPoint: float in [-1, 1] → int16 (-32768..32767).
    private static func i16FixedPoint(_ f: Float) -> UInt16 {
        let clamped = max(-1.0, min(1.0, f))
        let scaled = clamped * Float(Int16.max)
        return UInt16(bitPattern: Int16(scaled))
    }
}

// MARK: - Big-endian append helpers

private extension Data {
    mutating func appendBE(_ v: UInt8) {
        append(v)
    }
    mutating func appendBE(_ v: UInt16) {
        var be = v.bigEndian
        Swift.withUnsafeBytes(of: &be) { append(contentsOf: $0) }
    }
    mutating func appendBE(_ v: UInt32) {
        var be = v.bigEndian
        Swift.withUnsafeBytes(of: &be) { append(contentsOf: $0) }
    }
    mutating func appendBE(_ v: UInt64) {
        var be = v.bigEndian
        Swift.withUnsafeBytes(of: &be) { append(contentsOf: $0) }
    }
}
