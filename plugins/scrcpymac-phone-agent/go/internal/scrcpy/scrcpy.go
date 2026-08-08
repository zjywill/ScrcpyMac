// Package scrcpy owns the plugin's standalone scrcpy session: it pushes
// scrcpy-server to the device, spawns it over `adb shell app_process`, reads the
// H.264 video stream, drives the control socket, and relays the video to the
// Codex widget over a loopback WebSocket.
//
// It is a port of server/phone_agent/scrcpy_runtime.py with one deliberate
// exception — the relay transport, which is redesigned. The Python design has
// three defects that the Go relay must not reproduce:
//
//   - _broadcast_binary calls socket.sendall() synchronously on the packet-pump
//     thread with no queue and no drop policy, so one slow client backpressures
//     the device encoder and stalls every other client.
//   - Nothing sets TCP_NODELAY, so Nagle batches small NAL units behind the
//     delayed-ACK timer.
//   - The only recovery from a slow client is the widget's own
//     "drop a delta frame when decodeQueueSize > 4", which snaps the H.264
//     reference chain and freezes the picture until the next key frame — up to
//     ten seconds away, because that is scrcpy's I-frame interval.
//
// The Go design instead gives every client a bounded queue and its own writer
// goroutine (Hub/Client in hub.go); the pump never blocks. When a client falls
// behind, whole GOPs are discarded from the oldest end so the queue always
// resumes at a key frame, and a client that has been forced to discard is gated
// until the next key frame arrives. A new client is replayed the current GOP
// from its key frame, not just the key frame alone, so its decoder starts with
// an intact reference chain.
//
// # What is frozen contract
//
//   - The 12-byte device video header, the CONFIG/KEYFRAME pts flags and the
//     control-socket message layouts are scrcpy's wire protocol.
//   - The 14-byte application header this package puts in front of every H.264
//     access unit is parsed by ui/src/main.ts:parseWsPacket. The widget is not
//     being rewritten, so version/flags/pts/length and their byte offsets cannot
//     change.
//   - Every error string here reaches the model (or the widget) verbatim through
//     {"ok": false, "error": ...}, so they are byte-identical to the Python.
package scrcpy

import (
	"fmt"
	"strings"
)

// Wire constants shared with scrcpy-server and with the widget.
const (
	// DeviceNameLength is the fixed-width device name scrcpy-server sends after
	// the handshake byte.
	DeviceNameLength = 64
	// FrameHeaderLength is scrcpy's per-packet video header: a 64-bit pts field
	// carrying two flag bits, then a 32-bit payload length.
	FrameHeaderLength = 12

	packetFlagConfig   uint64 = 1 << 63
	packetFlagKeyFrame uint64 = 1 << 62
	packetPTSMask      uint64 = (1 << 62) - 1

	// H264CodecID is scrcpy's four-character code for H.264 ("h264").
	H264CodecID uint32 = 0x68323634

	// WSPacketVersion is byte 0 of the application header. The widget rejects
	// anything else with "Unsupported ScrcpyMac stream version."
	WSPacketVersion byte = 1
	// WSFlagConfig marks a codec-configuration packet (SPS/PPS).
	WSFlagConfig byte = 1
	// WSFlagKeyFrame marks an IDR access unit.
	WSFlagKeyFrame byte = 2
	// WSHeaderLength is the application header size: version(1) + flags(1) +
	// pts(8) + length(4). ui/src/main.ts calls this WS_PACKET_HEADER_BYTES.
	WSHeaderLength = 14
)

// DefaultCodec is the codec string reported before the SPS has been parsed. It
// is Baseline 3.0, the same fallback the Python and the widget use.
const DefaultCodec = "avc1.42E01E"

// Runtime states, in the exact spelling the status payload publishes.
const (
	StateIdle      = "idle"
	StateStarting  = "starting"
	StateStreaming = "streaming"
	StateError     = "error"
)

// jarDevicePath is where scrcpy-server is pushed. It is deliberately distinct
// from scrcpy's own /data/local/tmp/scrcpy-server.jar so a desktop scrcpy and
// this plugin never overwrite each other's jar mid-session.
const jarDevicePath = "/data/local/tmp/scrcpymac-plugin-server.jar"

// Error is the Go equivalent of ScrcpyRuntimeError. Its message is model-visible
// through the {"ok": false, "error": ...} payload the tools return.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errorf(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// VideoMeta describes the negotiated video stream. It is the Go form of the
// frozen ScrcpyVideoMeta dataclass.
type VideoMeta struct {
	Serial            string
	DeviceName        string
	CodecID           uint32
	Width             int
	Height            int
	NativeWidth       int
	NativeHeight      int
	MaxFPS            int
	ResolutionPercent int
}

// keycode is one entry of the control-socket key table.
type keycode struct {
	Name string
	Code uint32
}

// keycodes is the key table used while the plugin stream is active.
//
// The order is contract: the "Unknown key ... Supported: ..." message joins the
// table in declaration order, exactly like Python joining its dict.
//
// One deliberate difference from scrcpy_runtime.KEYCODES: "paste" (279) is
// present here. The Python's runtime table omitted it while actions.KEYCODES
// (the adb path) had it last, so scrcpymac_ui_key("paste") worked when idle and
// failed with "Unknown key" the moment the H.264 stream came up. Appending it
// last makes both backends accept the same eleven keys and keeps the joined
// order identical to the adb table.
var keycodes = []keycode{
	{"back", 4},
	{"home", 3},
	{"recents", 187},
	{"enter", 66},
	{"delete", 67},
	{"tab", 61},
	{"menu", 82},
	{"power", 26},
	{"volume_up", 24},
	{"volume_down", 25},
	{"paste", 279},
}

// lookupKeycode resolves a key name, mirroring name.lower().strip().
func lookupKeycode(name string) (uint32, bool) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for _, entry := range keycodes {
		if entry.Name == normalized {
			return entry.Code, true
		}
	}
	return 0, false
}

// keycodeNames is the "Supported: ..." tail of the unknown-key error.
func keycodeNames() string {
	names := make([]string, 0, len(keycodes))
	for _, entry := range keycodes {
		names = append(names, entry.Name)
	}
	return strings.Join(names, ", ")
}

// pyRepr renders s the way Python's {!r} does. The forward-port failure message
// interpolates adb's stdout with !r, and that text reaches the model.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
