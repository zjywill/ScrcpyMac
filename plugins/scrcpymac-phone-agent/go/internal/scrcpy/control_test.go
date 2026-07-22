package scrcpy

import (
	"encoding/hex"
	"testing"
)

// The control-socket layouts are scrcpy's wire protocol. Every expected string
// here came from running the Python's own struct.pack call, so a refactor that
// reorders or resizes a field fails loudly instead of producing taps in the
// wrong place.
func TestControlMessageLayouts(t *testing.T) {
	tests := []struct {
		name string
		msg  []byte
		want string
	}{
		{
			// struct.pack(">BBQIIHHHII", 2, 0, 0xFFFFFFFFFFFFFFFF, 540, 1200, 1080, 2280, 0xFFFF, 1, 1)
			name: "touch down",
			msg:  touchMessage(touchActionDown, 540, 1200, 1080, 2280, true),
			want: "0200ffffffffffffffff0000021c000004b0043808e8ffff0000000100000001",
		},
		{
			// action 2 zeroes the action button but keeps the button mask.
			name: "touch move",
			msg:  touchMessage(touchActionMove, 540, 1200, 1080, 2280, true),
			want: "0202ffffffffffffffff0000021c000004b0043808e8ffff0000000000000001",
		},
		{
			// action 1 zeroes the pressure and releases the buttons.
			name: "touch up",
			msg:  touchMessage(touchActionUp, 540, 1200, 1080, 2280, false),
			want: "0201ffffffffffffffff0000021c000004b0043808e800000000000100000000",
		},
		{
			// struct.pack(">BBIII", 0, 0, 4, 0, 0)
			name: "key down",
			msg:  keyMessage(keyActionDown, 4),
			want: "0000000000040000000000000000",
		},
		{
			name: "key up",
			msg:  keyMessage(keyActionUp, 4),
			want: "0001000000040000000000000000",
		},
		{
			// struct.pack(">BQBI", 9, 0, 1, len(utf8)) + utf8. The length is in
			// BYTES, not runes: "hi 你好" is 9 bytes and 5 runes.
			name: "clipboard paste",
			msg:  clipboardMessage("hi 你好"),
			want: "0900000000000000000100000009686920e4bda0e5a5bd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hex.EncodeToString(tt.msg); got != tt.want {
				t.Fatalf("\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestControlMessageSizes(t *testing.T) {
	if n := len(touchMessage(touchActionDown, 0, 0, 1, 1, true)); n != 32 {
		t.Errorf("touch message is %d bytes, want 32", n)
	}
	if n := len(keyMessage(keyActionDown, 66)); n != 14 {
		t.Errorf("key message is %d bytes, want 14", n)
	}
	if n := len(clipboardMessage("")); n != 14 {
		t.Errorf("empty clipboard message is %d bytes, want 14", n)
	}
}

func TestKeycodeTable(t *testing.T) {
	for _, tt := range []struct {
		name string
		code uint32
	}{
		{"back", 4}, {"home", 3}, {"recents", 187}, {"enter", 66}, {"delete", 67},
		{"tab", 61}, {"menu", 82}, {"power", 26}, {"volume_up", 24}, {"volume_down", 25},
		{"paste", 279},
	} {
		got, ok := lookupKeycode(tt.name)
		if !ok || got != tt.code {
			t.Errorf("lookupKeycode(%q) = %d, %v; want %d, true", tt.name, got, ok, tt.code)
		}
	}

	// Normalisation matches name.lower().strip().
	if got, ok := lookupKeycode("  BACK "); !ok || got != 4 {
		t.Errorf("lookupKeycode with surrounding space and case = %d, %v", got, ok)
	}
	if _, ok := lookupKeycode("nope"); ok {
		t.Error("an unknown key must not resolve")
	}

	// The "Supported: ..." tail is model-visible and joins the table in order.
	const want = "back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down, paste"
	if got := keycodeNames(); got != want {
		t.Fatalf("keycodeNames() = %q, want %q", got, want)
	}
}

func TestPyRepr(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "''"},
		{"5037\n", `'5037\n'`},
		{"it's", `"it's"`},
		{"a\tb", `'a\tb'`},
	} {
		if got := pyRepr(tt.in); got != tt.want {
			t.Errorf("pyRepr(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
