import Foundation
import AppKit

/// Encoders for scrcpy's client-to-device control protocol (v3.3.4). All
/// multi-byte fields are big-endian. Coordinates are in *device* pixel space,
/// not the local view — the position struct tells the server what screen
/// resolution those pixels are in so it can map them for any rotation.
enum ControlMessage {
    // Message type ids (must match ControlMessage.java).
    static let typeInjectKeycode: UInt8     = 0
    static let typeInjectTouchEvent: UInt8  = 2
    static let typeInjectScrollEvent: UInt8 = 3
    static let typeBackOrScreenOn: UInt8    = 4
    static let typeSetClipboard: UInt8      = 9
    static let typeSetDisplayPower: UInt8   = 10

    // Android KeyEvent actions scrcpy accepts for keyboard injection.
    enum KeyAction: UInt8 {
        case down = 0
        case up = 1
    }

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

    /// Build an INJECT_KEYCODE packet.
    static func injectKeycode(
        action: KeyAction,
        keycode: Int32,
        repeatCount: Int32 = 0,
        metaState: Int32 = 0
    ) -> Data {
        var d = Data()
        d.reserveCapacity(14)
        d.appendBE(typeInjectKeycode)
        d.appendBE(action.rawValue)
        d.appendBE(UInt32(bitPattern: keycode))
        d.appendBE(UInt32(bitPattern: repeatCount))
        d.appendBE(UInt32(bitPattern: metaState))
        return d
    }

    /// Build a SET_CLIPBOARD packet and optionally request immediate paste.
    static func setClipboard(sequence: UInt64, text: String, paste: Bool) -> Data {
        let utf8 = Data(text.utf8)
        var d = Data()
        d.reserveCapacity(14 + utf8.count)
        d.appendBE(typeSetClipboard)
        d.appendBE(sequence)
        d.appendBE(paste ? UInt8(1) : UInt8(0))
        d.appendBE(UInt32(utf8.count))
        d.append(utf8)
        return d
    }

    /// Build a SET_DISPLAY_POWER packet. `on=false` turns the device screen
    /// off while mirroring continues (scrcpy's `--turn-screen-off`).
    static func setDisplayPower(on: Bool) -> Data {
        var d = Data()
        d.reserveCapacity(2)
        d.appendBE(typeSetDisplayPower)
        d.appendBE(on ? UInt8(1) : UInt8(0))
        return d
    }

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

/// Minimal AppKit -> Android key mapping for scrcpy keyboard injection.
enum AndroidKeycode {
    static let metaShiftOn: Int32 = 0x00000001
    static let metaAltOn: Int32 = 0x00000002
    static let metaCtrlOn: Int32 = 0x00001000
    static let metaMetaOn: Int32 = 0x00010000

    static func map(event: NSEvent) -> (keycode: Int32, metaState: Int32)? {
        let keycode = mapSpecialKey(event) ?? mapCharacterKey(event)
        guard let keycode else { return nil }
        return (keycode, metaState(for: event.modifierFlags))
    }

    /// True when this event is the Cmd+V paste shortcut. The caller should
    /// intercept it and push the Mac clipboard to the device via
    /// SET_CLIPBOARD(paste=true) — otherwise the device would only re-paste
    /// whatever is already in the Android clipboard.
    static func isMacPasteShortcut(_ event: NSEvent) -> Bool {
        guard event.modifierFlags.contains(.command),
              let chars = event.charactersIgnoringModifiers?.lowercased() else {
            return false
        }
        return chars == "v"
    }

    private static func mapSpecialKey(_ event: NSEvent) -> Int32? {
        switch Int(event.keyCode) {
        case 36: return 66   // Enter
        case 48: return 61   // Tab
        case 49: return 62   // Space
        case 51: return 67   // Delete/Backspace
        case 53: return 4    // Escape/Back
        case 123: return 21  // Left
        case 124: return 22  // Right
        case 125: return 20  // Down
        case 126: return 19  // Up
        case 115: return 122 // Home
        case 116: return 92  // Page Up
        case 119: return 93  // Page Down
        case 121: return 123 // End
        default:
            break
        }

        guard let chars = event.charactersIgnoringModifiers, chars.count == 1,
              let scalar = chars.unicodeScalars.first else {
            return nil
        }

        switch scalar.value {
        case 0xF700: return 19  // Up
        case 0xF701: return 20  // Down
        case 0xF702: return 21  // Left
        case 0xF703: return 22  // Right
        default: return nil
        }
    }

    private static func mapCharacterKey(_ event: NSEvent) -> Int32? {
        guard let chars = event.charactersIgnoringModifiers?.uppercased(), chars.count == 1,
              let scalar = chars.unicodeScalars.first else {
            return nil
        }

        switch scalar.value {
        case 65...90:
            return Int32(29 + scalar.value - 65) // A-Z
        case 48...57:
            return scalar.value == 48 ? 7 : Int32(scalar.value - 49 + 8) // 0-9
        case 45:
            return 69 // Minus
        case 61:
            return 70 // Equals
        case 91:
            return 71 // Left bracket
        case 93:
            return 72 // Right bracket
        case 92:
            return 73 // Backslash
        case 59:
            return 74 // Semicolon
        case 39:
            return 75 // Apostrophe
        case 44:
            return 55 // Comma
        case 46:
            return 56 // Period
        case 47:
            return 76 // Slash
        case 96:
            return 68 // Grave
        default:
            return nil
        }
    }

    private static func metaState(for flags: NSEvent.ModifierFlags) -> Int32 {
        var state: Int32 = 0
        if flags.contains(.shift) { state |= metaShiftOn }
        if flags.contains(.option) { state |= metaAltOn }
        if flags.contains(.control) { state |= metaCtrlOn }
        if flags.contains(.command) { state |= metaMetaOn }
        return state
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
