package scrcpy

import "encoding/binary"

// scrcpy control-message types. Only the three the plugin uses are declared;
// their numbering comes from scrcpy's ControlMessage and is version-stable
// across the 2.x/3.x line.
const (
	msgInjectKeycode    byte = 0
	msgInjectTouchEvent byte = 2
	msgSetClipboard     byte = 9
)

// Touch actions, matching android.view.MotionEvent.
const (
	touchActionDown byte = 0
	touchActionUp   byte = 1
	touchActionMove byte = 2
)

// Key actions, matching android.view.KeyEvent.
const (
	keyActionDown byte = 0
	keyActionUp   byte = 1
)

// pointerIDMouse is scrcpy's synthetic "virtual mouse" pointer id (-1 as u64).
const pointerIDMouse uint64 = 0xFFFFFFFFFFFFFFFF

// touchMessage builds an INJECT_TOUCH_EVENT message.
//
// Layout — 32 bytes, big endian, no padding:
//
//	u8   type = 2
//	u8   action (0 down, 1 up, 2 move)
//	u64  pointer id
//	u32  x
//	u32  y
//	u16  frame width
//	u16  frame height
//	u16  pressure, u16 fixed point (0xFFFF == 1.0)
//	u32  action button
//	u32  buttons
//
// Pressure is 0 on release and full scale otherwise; action button is 0 for a
// move and 1 (PRIMARY) otherwise. Both rules are copied from _send_touch.
func touchMessage(action byte, x, y int, width, height int, buttons bool) []byte {
	pressure := uint16(0xFFFF)
	if action == touchActionUp {
		pressure = 0
	}
	actionButton := uint32(1)
	if action == touchActionMove {
		actionButton = 0
	}
	buttonMask := uint32(0)
	if buttons {
		buttonMask = 1
	}

	msg := make([]byte, 0, 32)
	msg = append(msg, msgInjectTouchEvent, action)
	msg = binary.BigEndian.AppendUint64(msg, pointerIDMouse)
	msg = binary.BigEndian.AppendUint32(msg, uint32(x))
	msg = binary.BigEndian.AppendUint32(msg, uint32(y))
	msg = binary.BigEndian.AppendUint16(msg, uint16(width))
	msg = binary.BigEndian.AppendUint16(msg, uint16(height))
	msg = binary.BigEndian.AppendUint16(msg, pressure)
	msg = binary.BigEndian.AppendUint32(msg, actionButton)
	return binary.BigEndian.AppendUint32(msg, buttonMask)
}

// keyMessage builds an INJECT_KEYCODE message.
//
// Layout — 14 bytes: u8 type = 0, u8 action, u32 keycode, u32 repeat,
// u32 metaState.
func keyMessage(action byte, code uint32) []byte {
	msg := make([]byte, 0, 14)
	msg = append(msg, msgInjectKeycode, action)
	msg = binary.BigEndian.AppendUint32(msg, code)
	msg = binary.BigEndian.AppendUint32(msg, 0)
	return binary.BigEndian.AppendUint32(msg, 0)
}

// clipboardMessage builds a SET_CLIPBOARD message with paste=1, which makes
// scrcpy-server set the device clipboard and immediately inject a paste.
//
// Layout — u8 type = 9, u64 sequence, u8 paste, u32 length, then UTF-8 text.
// Sequence 0 means "do not acknowledge", which is what the plugin wants: it
// never reads from the control socket.
func clipboardMessage(text string) []byte {
	encoded := []byte(text)
	msg := make([]byte, 0, 14+len(encoded))
	msg = append(msg, msgSetClipboard)
	msg = binary.BigEndian.AppendUint64(msg, 0)
	msg = append(msg, 1)
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(encoded)))
	return append(msg, encoded...)
}
