package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

// Expected strings in this file were produced by running the real Python:
// json.dumps(payload, ensure_ascii=False, indent=2), shlex.quote(...), repr(...)
// and round(...). They are not hand-written approximations.

// ---------------------------------------------------------------------------
// shlex.quote
//
// The implementation lives in internal/adb (adb.Quote); these cases stay here as
// an independent corpus captured from the real python3, so the two packages'
// expectations have to agree.
// ---------------------------------------------------------------------------

func TestInputShellQuoteMatchesShlex(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"a-b_c.d/e:f,g+h=i@j%k", "a-b_c.d/e:f,g+h=i@j%k"},
		{"hello%sworld", "hello%sworld"},
		{"50%25", "50%25"},
		// Every non-ASCII rune is outside \w under re.ASCII, so Chinese and
		// emoji are always quoted. This is what makes phone_paste work.
		{"你好", "'你好'"},
		{"你好 世界", "'你好 世界'"},
		{"emoji😀", "'emoji😀'"},
		{"$HOME", "'$HOME'"},
		{"a b", "'a b'"},
		{"it's", `'it'"'"'s'`},
		{"", "''"},
		{`a"b`, `'a"b'`},
		{"a;b", "'a;b'"},
		{"tab\ttab", "'tab\ttab'"},
		{"a\nb", "'a\nb'"},
		{"back`tick", "'back`tick'"},
		{"semi;rm -rf /", "'semi;rm -rf /'"},
	}
	for _, tc := range cases {
		if got := adb.Quote(tc.in); got != tc.want {
			t.Errorf("adb.Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInputShellSafeSetIsExactlyPythonsWordAtPercentPlusEqualsColonCommaDotSlashDash(t *testing.T) {
	const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_@%+=:,./-"
	for b := 0; b < 128; b++ {
		one := string([]byte{byte(b)})
		want := strings.ContainsRune(safe, rune(b))
		if got := adb.Quote(one) == one; got != want {
			t.Errorf("adb.Quote leaves %q unquoted = %v, want %v", rune(b), got, want)
		}
	}
	// Every byte of a multi-byte rune is >= 0x80 and therefore unsafe.
	for b := 128; b < 256; b++ {
		one := string([]byte{byte(b)})
		if adb.Quote(one) == one {
			t.Errorf("adb.Quote left 0x%02x unquoted", b)
		}
	}
}

// ---------------------------------------------------------------------------
// Command construction
// ---------------------------------------------------------------------------

func TestInputTypeTextCommandEscapeOrder(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "input text hello"},
		// "%" first, then " ": a space becomes %s and a literal % becomes %25.
		{"hello world", "input text hello%sworld"},
		{"100%", "input text 100%25"},
		// Reversing the order would produce "%25s" here instead of "%25%s".
		{"50% off", "input text 50%25%soff"},
		// A literal "%s" typed by the user must survive as "%25s".
		{"%s", "input text %25s"},
		{"a'b", `input text 'a'"'"'b'`},
		{"你好", "input text '你好'"},
		{"$(rm -rf /)", `input text '$(rm%s-rf%s/)'`},
	}
	for _, tc := range cases {
		if got := inputTypeTextCommand(tc.in); got != tc.want {
			t.Errorf("inputTypeTextCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInputClipboardCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "cmd clipboard set-text hello"},
		// paste does NOT do the %/space escaping — the text goes to the
		// clipboard verbatim, only shell-quoted.
		{"hello world", "cmd clipboard set-text 'hello world'"},
		{"100%", "cmd clipboard set-text 100%"},
		{"你好，世界", "cmd clipboard set-text '你好，世界'"},
		{"emoji 😀 test", "cmd clipboard set-text 'emoji 😀 test'"},
		{"it's", `cmd clipboard set-text 'it'"'"'s'`},
	}
	for _, tc := range cases {
		if got := inputClipboardCommand(tc.in); got != tc.want {
			t.Errorf("inputClipboardCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInputSimpleCommands(t *testing.T) {
	if got, want := inputTapCommand(100, 200), "input tap 100 200"; got != want {
		t.Errorf("inputTapCommand = %q, want %q", got, want)
	}
	if got, want := inputTapCommand(-5, 0), "input tap -5 0"; got != want {
		t.Errorf("inputTapCommand = %q, want %q", got, want)
	}
	if got, want := inputSwipeCommand(1, 2, 3, 4, 350), "input swipe 1 2 3 4 350"; got != want {
		t.Errorf("inputSwipeCommand = %q, want %q", got, want)
	}
	if got, want := inputKeyeventCommand(279), "input keyevent 279"; got != want {
		t.Errorf("inputKeyeventCommand = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Coordinate maths
// ---------------------------------------------------------------------------

func TestInputRelativeToDevice(t *testing.T) {
	cases := []struct {
		name string
		v    float64
		size int
		want int
	}{
		// Boundaries: the mapping is onto (size-1), so 1.0 is the LAST pixel.
		{"zero maps to first pixel", 0.0, 1080, 0},
		{"one maps to last pixel", 1.0, 1080, 1079},
		{"one maps to last row", 1.0, 2280, 2279},
		{"zero on height", 0.0, 2280, 0},
		// Banker's rounding. 0.5*1079 = 539.5 -> 540 (540 is even).
		{"midpoint of an odd span rounds to even up", 0.5, 1080, 540},
		// 0.5*1077 = 538.5 -> 538 (538 is even). math.Round would say 539.
		{"midpoint of an even span rounds to even down", 0.5, 1078, 538},
		{"midpoint of the OnePlus 6 height", 0.5, 2280, 1140},
		{"quarter", 0.25, 1080, 270},
		{"non-terminating", 0.333, 1080, 359},
		// Off-by-one trap: the naive size (not size-1) mapping would give 1080.
		{"just below one", 0.999, 1080, 1078},
		{"tiny", 0.0005, 1080, 1},
		{"single pixel screen", 1.0, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inputRelativeToDevice(tc.v, tc.size); got != tc.want {
				t.Errorf("inputRelativeToDevice(%v, %d) = %d, want %d", tc.v, tc.size, got, tc.want)
			}
		})
	}
}

func TestInputRelativeToDeviceIsBankersNotTiesAway(t *testing.T) {
	// The single most likely silent regression: swapping PyRoundInt for
	// math.Round shifts every even-width mapping by one pixel.
	if got := inputRelativeToDevice(0.5, 1078); got != 538 {
		t.Fatalf("banker's rounding lost: got %d, want 538", got)
	}
	if away := int(math.Round(0.5 * 1077)); away != 539 {
		t.Fatalf("test premise wrong: math.Round(538.5) = %d", away)
	}
}

func TestInputImageToDevice(t *testing.T) {
	cases := []struct {
		name                     string
		v, imageSize, deviceSize int
		want                     int
	}{
		{"single pixel image", 0, 1, 1080, 0},
		{"origin", 0, 540, 1080, 0},
		// Last image pixel maps to the last device pixel: 539/539*1079.
		{"last image pixel maps to last device pixel", 539, 540, 1080, 1079},
		// Off-by-one trap: the naive size/size mapping gives 540, not 541.
		{"image midpoint is not the device midpoint", 270, 540, 1080, 541},
		{"height midpoint", 380, 1140, 2280, 760},
		// 1/2*1079 = 539.5 -> 540 (even).
		{"tie rounds to even up", 1, 3, 1080, 540},
		// 1/2*1077 = 538.5 -> 538 (even). math.Round would say 539.
		{"tie rounds to even down", 1, 3, 1078, 538},
		{"two pixel image, first", 0, 2, 1080, 0},
		{"two pixel image, last", 1, 2, 1080, 1079},
		{"upscaling from a thumbnail", 3, 4, 1080, 1079},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inputImageToDevice(tc.v, tc.imageSize, tc.deviceSize)
			if got != tc.want {
				t.Errorf("inputImageToDevice(%d, %d, %d) = %d, want %d",
					tc.v, tc.imageSize, tc.deviceSize, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Retry cross
// ---------------------------------------------------------------------------

func TestInputTapOffsets(t *testing.T) {
	cases := []struct {
		retries int
		want    [][2]int
	}{
		{0, [][2]int{{0, 0}}},
		{1, [][2]int{{0, 0}, {0, -32}}},
		// The default: target, above, below. Vertical before horizontal.
		{2, [][2]int{{0, 0}, {0, -32}, {0, 32}}},
		{3, [][2]int{{0, 0}, {0, -32}, {0, 32}, {-32, 0}}},
		{4, [][2]int{{0, 0}, {0, -32}, {0, 32}, {-32, 0}, {32, 0}}},
		// Beyond the cross the list simply runs out; callers clamp to 4 first.
		{9, [][2]int{{0, 0}, {0, -32}, {0, 32}, {-32, 0}, {32, 0}}},
		{-1, [][2]int{{0, 0}}},
	}
	for _, tc := range cases {
		got := inputTapOffsets(tc.retries, inputTapRetryRadiusPX)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("inputTapOffsets(%d) = %v, want %v", tc.retries, got, tc.want)
		}
	}
}

func TestInputTapOptionClamps(t *testing.T) {
	if got := Clamp(9, 0, inputTapMaxRetries); got != 4 {
		t.Errorf("retries clamp: got %d, want 4", got)
	}
	if got := Clamp(-3, 0, inputTapMaxRetries); got != 0 {
		t.Errorf("retries clamp: got %d, want 0", got)
	}
	if got := Clamp(200, 1, 96); got != 96 {
		t.Errorf("radius clamp: got %d, want 96", got)
	}
	if got := Clamp(0, 1, 96); got != 1 {
		t.Errorf("radius clamp: got %d, want 1", got)
	}
	if got := Clamp(5.0, 0.1, 2.0); got != 2.0 {
		t.Errorf("settle clamp: got %v, want 2.0", got)
	}
	if got := Clamp(0.0, 0.1, 2.0); got != 0.1 {
		t.Errorf("settle clamp: got %v, want 0.1", got)
	}
	if def := inputDefaultTapOptions(); def.Verify || def.Retries != 2 ||
		def.RetryRadiusPX != 32 || def.SettleS != 0.45 {
		t.Errorf("actions-level tap defaults drifted: %+v", def)
	}
}

// ---------------------------------------------------------------------------
// Keycodes
// ---------------------------------------------------------------------------

func TestInputKeycodeTable(t *testing.T) {
	want := map[string]int{
		"back": 4, "home": 3, "recents": 187, "enter": 66, "delete": 67,
		"tab": 61, "menu": 82, "power": 26, "volume_up": 24, "volume_down": 25,
		"paste": 279,
	}
	if len(inputKeycodes) != len(want) {
		t.Fatalf("keycode table has %d entries, want %d", len(inputKeycodes), len(want))
	}
	for name, code := range want {
		if got := inputKeycodes[name]; got != code {
			t.Errorf("keycode %q = %d, want %d", name, got, code)
		}
	}
	if len(inputKeycodeOrder) != len(want) {
		t.Fatalf("keycode order has %d entries, want %d", len(inputKeycodeOrder), len(want))
	}
	for _, name := range inputKeycodeOrder {
		if _, ok := inputKeycodes[name]; !ok {
			t.Errorf("keycode order lists unknown key %q", name)
		}
	}
	const order = "back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down, paste"
	if got := strings.Join(inputKeycodeOrder, ", "); got != order {
		t.Errorf("keycode order = %q, want %q", got, order)
	}
}

func TestInputUnknownKeyMessage(t *testing.T) {
	actions := newTestInputActions()
	_, err := actions.key(context.Background(), "Foo")
	if err == nil {
		t.Fatal("want an error for an unknown key")
	}
	const want = "Unknown key 'Foo'. Supported: back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down, paste"
	if err.Error() != want {
		t.Errorf("error = %q\nwant   %q", err.Error(), want)
	}
	if !adb.IsError(err) {
		t.Errorf("unknown key should raise an adb.Error, got %T", err)
	}
	// The message quotes the ORIGINAL argument, not the normalised one.
	_, err = actions.key(context.Background(), "  NOPE ")
	if err == nil || !strings.HasPrefix(err.Error(), `Unknown key '  NOPE '.`) {
		t.Errorf("error should repr the original name, got %v", err)
	}
}

func TestInputPyRepr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Foo", "'Foo'"},
		{"", "''"},
		{"it's", `"it's"`},
		{`a"b`, `'a"b'`},
		{`a"b'c`, `'a"b\'c'`},
		{"你好", "'你好'"},
		{"café", "'café'"},
		{"a\nb", `'a\nb'`},
		{"a\tb", `'a\tb'`},
		{`a\b`, `'a\\b'`},
		{"a\x01b", `'a\x01b'`},
	}
	for _, tc := range cases {
		if got := inputPyRepr(tc.in); got != tc.want {
			t.Errorf("inputPyRepr(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Payload shapes — key order is contract
// ---------------------------------------------------------------------------

func TestInputPayloadsMatchPythonJSON(t *testing.T) {
	cases := []struct {
		name    string
		payload *jsonresult.Obj
		want    string
	}{
		{
			name:    "tap",
			payload: inputTapPayload(1, 2, "s"),
			want: `{
  "ok": true,
  "action": "tap",
  "x": 1,
  "y": 2,
  "serial": "s"
}`,
		},
		{
			name:    "swipe",
			payload: inputSwipePayload(10, 20, 30, 40, 300, "s"),
			want: `{
  "ok": true,
  "action": "swipe",
  "from": [
    10,
    20
  ],
  "to": [
    30,
    40
  ],
  "duration_ms": 300,
  "serial": "s"
}`,
		},
		{
			name:    "key",
			payload: inputKeyPayload("back", 4, "s"),
			want: `{
  "ok": true,
  "action": "key",
  "key": "back",
  "code": 4,
  "serial": "s"
}`,
		},
		{
			name:    "type",
			payload: inputTextPayload("type", "hello world", "s"),
			want: `{
  "ok": true,
  "action": "type",
  "length": 11,
  "serial": "s"
}`,
		},
		{
			// length is a rune count: "你好世界" is 4 in Python, 12 bytes in Go.
			name:    "paste chinese",
			payload: inputTextPayload("paste", "你好世界", "s"),
			want: `{
  "ok": true,
  "action": "paste",
  "length": 4,
  "serial": "s"
}`,
		},
		{
			// An astral-plane emoji is one code point in Python 3 and one rune.
			name:    "paste emoji",
			payload: inputTextPayload("paste", "😀😀", "s"),
			want: `{
  "ok": true,
  "action": "paste",
  "length": 2,
  "serial": "s"
}`,
		},
		{
			name:    "attempt with an integral score",
			payload: inputAttemptPayload(100, 200, false, 0),
			want: `{
  "point": [
    100,
    200
  ],
  "screen_changed": false,
  "change_score": 0.0
}`,
		},
		{
			name:    "attempt score is rounded to 4dp",
			payload: inputAttemptPayload(1, 2, true, 0.123456789),
			want: `{
  "point": [
    1,
    2
  ],
  "screen_changed": true,
  "change_score": 0.1235
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonresult.Text(tc.payload); got != tc.want {
				t.Errorf("payload =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestInputChangeScoreIsAPythonFloat(t *testing.T) {
	// Go would render float64(1) as "1"; Python renders 1.0 as "1.0".
	got := jsonresult.Text(inputAttemptPayload(0, 0, true, 1.0))
	if !strings.Contains(got, `"change_score": 1.0`) {
		t.Errorf("integral change_score lost its .0:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// The verify + retry loop
// ---------------------------------------------------------------------------

// inputFakeDevice is an inputDevice with no phone behind it.
type inputFakeDevice struct {
	width, height int
	serial        string

	taps      [][2]int
	sleeps    []time.Duration
	shotCalls int

	// shot returns the screenshot for call n (0 is the baseline).
	shot func(n int) (*inputShot, error)
	// tapErr, when set, fails the nth tap.
	tapErr func(n int) error
}

func (d *inputFakeDevice) requiredScreenSize(context.Context) (int, int, error) {
	if d.width == 0 || d.height == 0 {
		return 0, 0, &adb.Error{Msg: "Could not determine the device screen size"}
	}
	return d.width, d.height, nil
}

func (d *inputFakeDevice) clampPoint(_ context.Context, x, y int) (int, int) {
	if d.width == 0 || d.height == 0 {
		return x, y
	}
	return Clamp(x, 0, d.width-1), Clamp(y, 0, d.height-1)
}

func (d *inputFakeDevice) tapOnce(_ context.Context, x, y int) (*jsonresult.Obj, error) {
	n := len(d.taps)
	d.taps = append(d.taps, [2]int{x, y})
	if d.tapErr != nil {
		if err := d.tapErr(n); err != nil {
			return nil, err
		}
	}
	return inputTapPayload(x, y, d.serial), nil
}

func (d *inputFakeDevice) screenshot(context.Context) (*inputShot, error) {
	n := d.shotCalls
	d.shotCalls++
	if d.shot == nil {
		return nil, &adb.Error{Msg: "no screenshot"}
	}
	return d.shot(n)
}

func (d *inputFakeDevice) sleep(_ context.Context, dur time.Duration) error {
	d.sleeps = append(d.sleeps, dur)
	return nil
}

func inputSolidShot(t *testing.T, w, h int, c color.RGBA) *inputShot {
	t.Helper()
	data := inputSolidPNG(t, w, h, c)
	return &inputShot{Serial: "fake", Width: w, Height: h, PNG: data, SizeBytes: len(data)}
}

func inputSolidPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	return inputPatchPNG(t, w, h, c, image.Rectangle{}, c)
}

// inputPatchPNG paints a solid background and then a rectangle in patchColor.
func inputPatchPNG(t *testing.T, w, h int, bg color.RGBA, patch image.Rectangle, patchColor color.RGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := bg
			if patch.Dx() > 0 && patch.Dy() > 0 && image.Pt(x, y).In(patch) {
				c = patchColor
			}
			img.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

var (
	inputBlack = color.RGBA{R: 0, G: 0, B: 0, A: 255}
	inputWhite = color.RGBA{R: 255, G: 255, B: 255, A: 255}
)

func TestInputVerifiedTapFastPathSkipsVerification(t *testing.T) {
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake"}
	opts := inputDefaultTapOptions()
	opts.Verify = false

	got, err := inputVerifiedTap(context.Background(), dev, 100, 200, opts)
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if got.Has("verification") {
		t.Error("the unverified fast path must not add a verification key")
	}
	if dev.shotCalls != 0 {
		t.Errorf("fast path took %d screenshots, want 0", dev.shotCalls)
	}
	if len(dev.taps) != 1 || dev.taps[0] != [2]int{100, 200} {
		t.Errorf("taps = %v, want one tap at (100,200)", dev.taps)
	}
}

func TestInputVerifiedTapClampsBeforeTapping(t *testing.T) {
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake"}
	opts := inputDefaultTapOptions()
	opts.Verify = false

	if _, err := inputVerifiedTap(context.Background(), dev, 99999, -40, opts); err != nil {
		t.Fatalf("tap: %v", err)
	}
	if want := [2]int{1079, 0}; dev.taps[0] != want {
		t.Errorf("clamped point = %v, want %v", dev.taps[0], want)
	}
}

func TestInputVerifiedTapSucceedsOnFirstAttempt(t *testing.T) {
	baseline := inputSolidShot(t, 144, 256, inputBlack)
	after := inputSolidShot(t, 144, 256, inputWhite)
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake", shot: func(n int) (*inputShot, error) {
		if n == 0 {
			return baseline, nil
		}
		return after, nil
	}}
	opts := inputTapOpts(true, 2)

	got, err := inputVerifiedTap(context.Background(), dev, 100, 200, opts)
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if len(dev.taps) != 1 {
		t.Fatalf("taps = %v, want exactly one (the first attempt changed the screen)", dev.taps)
	}
	if len(dev.sleeps) != 1 || dev.sleeps[0] != 450*time.Millisecond {
		t.Errorf("settle sleeps = %v, want one 450ms sleep", dev.sleeps)
	}
	want := fmt.Sprintf(`{
  "ok": true,
  "action": "tap",
  "x": 100,
  "y": 200,
  "serial": "fake",
  "verification": {
    "requested": true,
    "available": true,
    "verified": true,
    "attempts": [
      {
        "point": [
          100,
          200
        ],
        "screen_changed": true,
        "change_score": 1.0
      }
    ],
    "after_size_bytes": %d
  }
}`, after.SizeBytes)
	if text := jsonresult.Text(got); text != want {
		t.Errorf("payload =\n%s\nwant\n%s", text, want)
	}
}

func TestInputVerifiedTapRetriesTheCrossThenGivesUp(t *testing.T) {
	baseline := inputSolidShot(t, 144, 256, inputBlack)
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake", shot: func(int) (*inputShot, error) {
		return baseline, nil
	}}

	got, err := inputVerifiedTap(context.Background(), dev, 100, 200, inputTapOpts(true, 2))
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	wantTaps := [][2]int{{100, 200}, {100, 168}, {100, 232}}
	if fmt.Sprint(dev.taps) != fmt.Sprint(wantTaps) {
		t.Errorf("taps = %v, want %v (target, up, down)", dev.taps, wantTaps)
	}
	// One baseline plus one after-shot per attempt.
	if dev.shotCalls != 4 {
		t.Errorf("screenshots = %d, want 4", dev.shotCalls)
	}
	// The payload is the LAST tap's result (Python reassigns last_result every
	// iteration), so x/y are the final candidate, not the requested point. The
	// requested point survives as attempts[0].
	const want = `{
  "ok": true,
  "action": "tap",
  "x": 100,
  "y": 232,
  "serial": "fake",
  "verification": {
    "requested": true,
    "available": true,
    "verified": false,
    "attempts": [
      {
        "point": [
          100,
          200
        ],
        "screen_changed": false,
        "change_score": 0.0
      },
      {
        "point": [
          100,
          168
        ],
        "screen_changed": false,
        "change_score": 0.0
      },
      {
        "point": [
          100,
          232
        ],
        "screen_changed": false,
        "change_score": 0.0
      }
    ],
    "hint": "No screen change detected after tapping the target and nearby points."
  }
}`
	if text := jsonresult.Text(got); text != want {
		t.Errorf("payload =\n%s\nwant\n%s", text, want)
	}
}

func TestInputVerifiedTapComparesEveryAttemptAgainstTheSameBaseline(t *testing.T) {
	// The second attempt changes the screen; the third must never run.
	baseline := inputSolidShot(t, 144, 256, inputBlack)
	changed := inputSolidShot(t, 144, 256, inputWhite)
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake", shot: func(n int) (*inputShot, error) {
		switch n {
		case 0, 1:
			return baseline, nil
		default:
			return changed, nil
		}
	}}

	got, err := inputVerifiedTap(context.Background(), dev, 100, 200, inputTapOpts(true, 4))
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	wantTaps := [][2]int{{100, 200}, {100, 168}}
	if fmt.Sprint(dev.taps) != fmt.Sprint(wantTaps) {
		t.Errorf("taps = %v, want %v", dev.taps, wantTaps)
	}
	verification, _ := got.Get("verification")
	obj, ok := verification.(*jsonresult.Obj)
	if !ok {
		t.Fatalf("verification is %T", verification)
	}
	attempts, _ := obj.Get("attempts")
	list, ok := attempts.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("attempts = %#v, want a 2-element list", attempts)
	}
	if verified, _ := obj.Get("verified"); verified != true {
		t.Errorf("verified = %v, want true", verified)
	}
}

func TestInputVerifiedTapBaselineFailureShape(t *testing.T) {
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake", shot: func(int) (*inputShot, error) {
		return nil, &adb.Error{Msg: "adb exec-out screencap -p failed: device offline"}
	}}

	got, err := inputVerifiedTap(context.Background(), dev, 100, 200, inputTapOpts(true, 2))
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if len(dev.taps) != 1 {
		t.Errorf("taps = %v, want exactly one", dev.taps)
	}
	// REPLICATED BUG-2: "attempts" is the integer 1 on this path only.
	const want = `{
  "ok": true,
  "action": "tap",
  "x": 100,
  "y": 200,
  "serial": "fake",
  "verification": {
    "requested": true,
    "available": false,
    "attempts": 1
  }
}`
	if text := jsonresult.Text(got); text != want {
		t.Errorf("payload =\n%s\nwant\n%s", text, want)
	}
}

func TestInputVerifiedTapAfterScreenshotFailureCountsAsUnchanged(t *testing.T) {
	baseline := inputSolidShot(t, 144, 256, inputBlack)
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake", shot: func(n int) (*inputShot, error) {
		if n == 0 {
			return baseline, nil
		}
		return nil, &adb.Error{Msg: "adb timed out"}
	}}

	got, err := inputVerifiedTap(context.Background(), dev, 100, 200, inputTapOpts(true, 0))
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if !strings.Contains(jsonresult.Text(got), `"change_score": 0.0`) {
		t.Errorf("a failed after-screenshot must score 0.0:\n%s", jsonresult.Text(got))
	}
	if !strings.Contains(jsonresult.Text(got), `"verified": false`) {
		t.Errorf("want verified:false, got\n%s", jsonresult.Text(got))
	}
}

func TestInputVerifiedTapKeepsDuplicateClampedPoints(t *testing.T) {
	// BUG-10, replicated: in the corner, the up and left offsets both clamp
	// back onto the target, so the same pixel is tapped — and listed in
	// attempts — three times. De-duplicating would change the array the model
	// reads and silently reduce the retry count below what was asked for.
	baseline := inputSolidShot(t, 144, 256, inputBlack)
	dev := &inputFakeDevice{width: 100, height: 100, serial: "fake", shot: func(int) (*inputShot, error) {
		return baseline, nil
	}}

	got, err := inputVerifiedTap(context.Background(), dev, 0, 0, inputTapOpts(true, 4))
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	wantTaps := [][2]int{{0, 0}, {0, 0}, {0, 32}, {0, 0}, {32, 0}}
	if fmt.Sprint(dev.taps) != fmt.Sprint(wantTaps) {
		t.Errorf("taps = %v, want %v", dev.taps, wantTaps)
	}
	verification, _ := got.Get("verification")
	attempts, _ := verification.(*jsonresult.Obj).Get("attempts")
	if list, ok := attempts.([]any); !ok || len(list) != 5 {
		t.Errorf("attempts = %#v, want 5 entries including the duplicates", attempts)
	}

	// The same collapse away from the corner still yields five distinct points.
	dev.taps = nil
	if _, err := inputVerifiedTap(context.Background(), dev, 5, 5, inputTapOpts(true, 4)); err != nil {
		t.Fatalf("tap: %v", err)
	}
	wantTaps = [][2]int{{5, 5}, {5, 0}, {5, 37}, {0, 5}, {37, 5}}
	if fmt.Sprint(dev.taps) != fmt.Sprint(wantTaps) {
		t.Errorf("taps = %v, want %v", dev.taps, wantTaps)
	}
}

func TestInputVerifiedTapPropagatesTapErrors(t *testing.T) {
	baseline := inputSolidShot(t, 144, 256, inputBlack)
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake",
		shot:   func(int) (*inputShot, error) { return baseline, nil },
		tapErr: func(int) error { return &adb.Error{Msg: "adb shell input tap 1 2 failed: closed"} },
	}
	if _, err := inputVerifiedTap(context.Background(), dev, 1, 2, inputTapOpts(true, 2)); err == nil {
		t.Fatal("a failing tap must abort the whole call")
	}
}

func TestInputVerifiedTapAbortsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "fake", shot: func(int) (*inputShot, error) {
		return nil, context.Canceled
	}}
	if _, err := inputVerifiedTap(ctx, dev, 1, 2, inputTapOpts(true, 2)); err == nil {
		t.Fatal("a cancelled context must abort rather than fall into the unavailable shape")
	}
}

// ---------------------------------------------------------------------------
// tap_relative / tap_image end to end
// ---------------------------------------------------------------------------

func TestInputTapRelativePayload(t *testing.T) {
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "2f019965"}
	opts := inputTapOpts(false, 0)

	got, err := inputTapRelative(context.Background(), dev, 0.0, 1.0, opts)
	if err != nil {
		t.Fatalf("tap_relative: %v", err)
	}
	// source echoes the ORIGINAL floats, so 0.0 and 1.0 keep their ".0".
	const want = `{
  "ok": true,
  "coordinate_space": "relative",
  "source": [
    0.0,
    1.0
  ],
  "device_point": [
    0,
    2279
  ],
  "tap": {
    "ok": true,
    "action": "tap",
    "x": 0,
    "y": 2279,
    "serial": "2f019965"
  }
}`
	if text := jsonresult.Text(got); text != want {
		t.Errorf("payload =\n%s\nwant\n%s", text, want)
	}
	if got.Has("device_size") {
		t.Error("tap_relative must NOT carry device_size; only tap_image does")
	}
}

func TestInputTapRelativeRejectsOutOfRange(t *testing.T) {
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "s"}
	const want = "relative x and y must be between 0 and 1"
	for _, pt := range [][2]float64{
		{-0.001, 0.5}, {1.001, 0.5}, {0.5, -1}, {0.5, 2},
		{math.NaN(), 0.5}, {0.5, math.NaN()},
	} {
		_, err := inputTapRelative(context.Background(), dev, pt[0], pt[1], inputTapOpts(false, 0))
		if err == nil || err.Error() != want {
			t.Errorf("tap_relative(%v, %v) error = %v, want %q", pt[0], pt[1], err, want)
		}
	}
	// Both bounds are inclusive.
	for _, pt := range [][2]float64{{0, 0}, {1, 1}, {0, 1}, {1, 0}} {
		if _, err := inputTapRelative(context.Background(), dev, pt[0], pt[1], inputTapOpts(false, 0)); err != nil {
			t.Errorf("tap_relative(%v, %v) should be accepted: %v", pt[0], pt[1], err)
		}
	}
}

func TestInputTapImagePayload(t *testing.T) {
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "2f019965"}

	got, err := inputTapImage(context.Background(), dev, 270, 380, 540, 1140, inputTapOpts(false, 0))
	if err != nil {
		t.Fatalf("tap_image: %v", err)
	}
	const want = `{
  "ok": true,
  "coordinate_space": "image",
  "source": {
    "point": [
      270,
      380
    ],
    "size": [
      540,
      1140
    ]
  },
  "device_point": [
    541,
    760
  ],
  "device_size": [
    1080,
    2280
  ],
  "tap": {
    "ok": true,
    "action": "tap",
    "x": 541,
    "y": 760,
    "serial": "2f019965"
  }
}`
	if text := jsonresult.Text(got); text != want {
		t.Errorf("payload =\n%s\nwant\n%s", text, want)
	}
}

func TestInputTapImageValidation(t *testing.T) {
	dev := &inputFakeDevice{width: 1080, height: 2280, serial: "s"}
	cases := []struct {
		x, y, iw, ih int
		want         string
	}{
		{0, 0, 0, 100, "image_width and image_height must be positive"},
		{0, 0, 100, 0, "image_width and image_height must be positive"},
		{0, 0, -1, -1, "image_width and image_height must be positive"},
		{100, 0, 100, 100, "image point must be inside image_width x image_height"},
		{0, 100, 100, 100, "image point must be inside image_width x image_height"},
		{-1, 0, 100, 100, "image point must be inside image_width x image_height"},
		{0, -1, 100, 100, "image point must be inside image_width x image_height"},
	}
	for _, tc := range cases {
		_, err := inputTapImage(context.Background(), dev, tc.x, tc.y, tc.iw, tc.ih, inputTapOpts(false, 0))
		if err == nil || err.Error() != tc.want {
			t.Errorf("tap_image(%d,%d,%d,%d) error = %v, want %q", tc.x, tc.y, tc.iw, tc.ih, err, tc.want)
		}
	}
	// The last legal pixel is size-1, and a 1x1 image accepts only (0,0).
	if _, err := inputTapImage(context.Background(), dev, 99, 99, 100, 100, inputTapOpts(false, 0)); err != nil {
		t.Errorf("(99,99) in a 100x100 image should be legal: %v", err)
	}
	if _, err := inputTapImage(context.Background(), dev, 0, 0, 1, 1, inputTapOpts(false, 0)); err != nil {
		t.Errorf("(0,0) in a 1x1 image should be legal: %v", err)
	}
}

func TestInputTapMappingRequiresAScreenSize(t *testing.T) {
	dev := &inputFakeDevice{serial: "s"} // width/height zero => unknown
	const want = "Could not determine the device screen size"
	if _, err := inputTapRelative(context.Background(), dev, 0.5, 0.5, inputTapOpts(false, 0)); err == nil || err.Error() != want {
		t.Errorf("tap_relative error = %v, want %q", err, want)
	}
	if _, err := inputTapImage(context.Background(), dev, 1, 1, 10, 10, inputTapOpts(false, 0)); err == nil || err.Error() != want {
		t.Errorf("tap_image error = %v, want %q", err, want)
	}
}

// ---------------------------------------------------------------------------
// Screen-change detection
// ---------------------------------------------------------------------------

func TestInputChangeScore(t *testing.T) {
	const w, h = 144, 256 // exactly 2x the 72x128 sample grid

	black := inputSolidPNG(t, w, h, inputBlack)
	white := inputSolidPNG(t, w, h, inputWhite)

	if got := inputChangeScore(black, black); got != 0 {
		t.Errorf("identical images scored %v, want 0", got)
	}
	if got := inputChangeScore(black, white); got != 1 {
		t.Errorf("inverted images scored %v, want 1", got)
	}
	// Below the per-channel threshold of 20: no cell counts as changed.
	nearBlack := inputSolidPNG(t, w, h, color.RGBA{R: 19, G: 19, B: 19, A: 255})
	if got := inputChangeScore(black, nearBlack); got != 0 {
		t.Errorf("a 19/255 shift scored %v, want 0 (threshold is 20)", got)
	}
	atThreshold := inputSolidPNG(t, w, h, color.RGBA{R: 20, G: 0, B: 0, A: 255})
	if got := inputChangeScore(black, atThreshold); got != 1 {
		t.Errorf("a 20/255 shift on one channel scored %v, want 1", got)
	}
	// Garbage decodes to nothing, which is 0.0 rather than an error.
	if got := inputChangeScore([]byte("not a png"), white); got != 0 {
		t.Errorf("undecodable input scored %v, want 0", got)
	}
	if got := inputChangeScore(nil, white); got != 0 {
		t.Errorf("empty input scored %v, want 0", got)
	}
}

func TestInputChangeScoreThresholdSeparatesSmallFromLargeChanges(t *testing.T) {
	const w, h = 1080, 2280

	base := inputSolidPNG(t, w, h, inputBlack)
	// A 100x100 patch is ~0.4 % of the screen: a cursor blink or a toast dot.
	small := inputPatchPNG(t, w, h, inputBlack, image.Rect(0, 0, 100, 100), inputWhite)
	smallScore := inputChangeScore(base, small)
	if smallScore >= inputChangeThreshold {
		t.Errorf("a 100x100 patch scored %v, which is at/above the %v threshold", smallScore, inputChangeThreshold)
	}
	if smallScore == 0 {
		t.Errorf("a 100x100 patch scored exactly 0; the resample is dropping it entirely")
	}
	// Half the screen is unambiguously a navigation.
	half := inputPatchPNG(t, w, h, inputBlack, image.Rect(0, 0, w, h/2), inputWhite)
	if got := inputChangeScore(base, half); got < inputChangeThreshold {
		t.Errorf("half a screen scored %v, below the %v threshold", got, inputChangeThreshold)
	}
	// A compact 3%-height change, representative of a short chat bubble or
	// send-button transition, must cross the threshold without a retry.
	band := inputPatchPNG(t, w, h, inputBlack, image.Rect(0, 0, w, h*3/100), inputWhite)
	if got := inputChangeScore(base, band); got < inputChangeThreshold {
		t.Errorf("a 3%%-height band scored %v, below the %v threshold", got, inputChangeThreshold)
	}
}

func TestInputSampleRGBAreaAverages(t *testing.T) {
	// A half-black/half-white source must average to mid grey in the cells that
	// straddle the boundary — a nearest-neighbour sample would show only 0/255.
	const w, h = 720, 1280
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := uint8(0)
			// Alternate every source row, so each 10-row output cell averages
			// to ~127.
			if y%2 == 0 {
				c = 255
			}
			img.SetNRGBA(x, y, color.NRGBA{R: c, G: c, B: c, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	sample, err := inputSampleRGB(buf.Bytes())
	if err != nil {
		t.Fatalf("inputSampleRGB: %v", err)
	}
	if len(sample) != inputSamplePixels*3 {
		t.Fatalf("sample has %d values, want %d", len(sample), inputSamplePixels*3)
	}
	for i := 0; i < inputSamplePixels; i++ {
		if v := sample[i*3]; v < 120 || v > 135 {
			t.Fatalf("cell %d averaged to %d, want ~127 (area average, not point sampling)", i, v)
		}
	}
}

func TestInputSampleRGBHandlesSourcesSmallerThanTheGrid(t *testing.T) {
	sample, err := inputSampleRGB(inputSolidPNG(t, 8, 8, inputWhite))
	if err != nil {
		t.Fatalf("inputSampleRGB: %v", err)
	}
	for i, v := range sample {
		if v != 255 {
			t.Fatalf("value %d = %d, want 255 (upscale fallback left a hole)", i, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Runtime seam
// ---------------------------------------------------------------------------

type inputFakeRuntime struct {
	status   InputRuntimeStatus
	active   bool
	started  int
	stopped  int
	startErr error
	lastArgs []float64
	lastKey  string
	lastText string
	err      error
}

func (r *inputFakeRuntime) Status() InputRuntimeStatus { return r.status }
func (r *inputFakeRuntime) IsActive() bool             { return r.active }

func (r *inputFakeRuntime) StartForPaste(context.Context) error {
	r.started++
	if r.startErr != nil {
		return r.startErr
	}
	r.active = true
	return nil
}

func (r *inputFakeRuntime) Stop() {
	r.stopped++
	r.active = false
}

func (r *inputFakeRuntime) TapRelative(_ context.Context, x, y float64) (*jsonresult.Obj, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.lastArgs = []float64{x, y}
	return jsonresult.Of("ok", true, "action", "tap", "serial", "rt",
		"point", []int{1, 2}, "coordinateSpace", []int{540, 1140},
		"backend", "plugin-control"), nil
}

func (r *inputFakeRuntime) SwipeRelative(_ context.Context, x1, y1, x2, y2 float64, durationMS int) (*jsonresult.Obj, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.lastArgs = []float64{x1, y1, x2, y2, float64(durationMS)}
	return jsonresult.Of("ok", true, "action", "swipe", "serial", "rt",
		"from", []int{1, 2}, "to", []int{3, 4}, "durationMs", durationMS,
		"backend", "plugin-control"), nil
}

func (r *inputFakeRuntime) Key(_ context.Context, name string) (*jsonresult.Obj, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.lastKey = name
	return jsonresult.Of("ok", true, "action", "key", "key", name, "serial", "rt",
		"backend", "plugin-control"), nil
}

func (r *inputFakeRuntime) Paste(_ context.Context, text string) (*jsonresult.Obj, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.lastText = text
	return jsonresult.Of("ok", true, "action", "paste", "length", jsonresult.RuneLen(text),
		"serial", "rt", "backend", "plugin-control"), nil
}

func withInputRuntime(t *testing.T, rt InputRuntime) {
	t.Helper()
	SetInputRuntime(rt)
	t.Cleanup(func() { SetInputRuntime(nil) })
}

func TestInputRuntimePathIsUsedWhileStreaming(t *testing.T) {
	rt := &inputFakeRuntime{
		status: InputRuntimeStatus{Streaming: true, DeviceWidth: 1080, DeviceHeight: 2280},
		active: true,
	}
	withInputRuntime(t, rt)
	actions := newTestInputActions()
	ctx := context.Background()

	got, err := actions.tapOnce(ctx, 540, 1140)
	if err != nil {
		t.Fatalf("tapOnce: %v", err)
	}
	if backend, _ := got.Get("backend"); backend != "plugin-control" {
		t.Errorf("streaming tap did not use the runtime: %v", jsonresult.Text(got))
	}
	// 540/(1080-1) and 1140/(2280-1).
	if rt.lastArgs[0] != 540.0/1079.0 || rt.lastArgs[1] != 1140.0/2279.0 {
		t.Errorf("relative coordinates = %v, want [%v %v]", rt.lastArgs, 540.0/1079.0, 1140.0/2279.0)
	}

	if _, err := actions.swipe(ctx, 0, 0, 1079, 2279, 400); err != nil {
		t.Fatalf("swipe: %v", err)
	}
	if want := []float64{0, 0, 1, 1, 400}; fmt.Sprint(rt.lastArgs) != fmt.Sprint(want) {
		t.Errorf("swipe args = %v, want %v", rt.lastArgs, want)
	}

	keyResult, err := actions.key(ctx, " BACK ")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if rt.lastKey != "back" {
		t.Errorf("runtime key = %q, want %q", rt.lastKey, "back")
	}
	if keyResult.Has("code") {
		t.Error("the plugin-control key shape has no code key")
	}

	if _, err := actions.paste(ctx, "你好"); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if rt.lastText != "你好" {
		t.Errorf("runtime paste text = %q", rt.lastText)
	}
	if rt.started != 0 || rt.stopped != 0 {
		t.Errorf("active runtime start/stop = %d/%d, want 0/0", rt.started, rt.stopped)
	}
}

func TestInputPasteStartsAndStopsTemporaryRuntime(t *testing.T) {
	rt := &inputFakeRuntime{}
	withInputRuntime(t, rt)
	actions := newTestInputActions()

	result, err := actions.paste(context.Background(), "你好")
	if err != nil {
		t.Fatalf("paste: %v", err)
	}
	if rt.started != 1 || rt.stopped != 1 {
		t.Fatalf("temporary runtime start/stop = %d/%d, want 1/1", rt.started, rt.stopped)
	}
	if rt.lastText != "你好" {
		t.Errorf("runtime paste text = %q", rt.lastText)
	}
	if backend, _ := result.Get("backend"); backend != "plugin-control" {
		t.Errorf("paste backend = %v, want plugin-control", backend)
	}
}

func TestInputPasteStopsTemporaryRuntimeAfterPasteFailure(t *testing.T) {
	rt := &inputFakeRuntime{err: fmt.Errorf("control send failed")}
	withInputRuntime(t, rt)
	actions := newTestInputActions()

	_, err := actions.paste(context.Background(), "你好")
	if err == nil || err.Error() != "control send failed" {
		t.Fatalf("paste error = %v", err)
	}
	if rt.started != 1 || rt.stopped != 1 {
		t.Errorf("temporary runtime start/stop = %d/%d, want 1/1", rt.started, rt.stopped)
	}
}

func TestInputPasteDoesNotStopRuntimeWhenTemporaryStartFails(t *testing.T) {
	rt := &inputFakeRuntime{startErr: fmt.Errorf("start failed")}
	withInputRuntime(t, rt)
	actions := newTestInputActions()

	_, err := actions.paste(context.Background(), "你好")
	if err == nil || err.Error() != "start failed" {
		t.Fatalf("paste error = %v", err)
	}
	if rt.started != 1 || rt.stopped != 0 {
		t.Errorf("failed temporary runtime start/stop = %d/%d, want 1/0", rt.started, rt.stopped)
	}
}

func TestInputRuntimeErrorsBecomeAdbErrors(t *testing.T) {
	// BUG-8 fix: the Python leaves ScrcpyRuntimeError unwrapped on the tap and
	// swipe paths, where server.py's `except (AdbError, OSError)` misses it.
	rt := &inputFakeRuntime{
		status: InputRuntimeStatus{Streaming: true, DeviceWidth: 1080, DeviceHeight: 2280},
		active: true,
		err:    fmt.Errorf("plugin scrcpy stream is not running"),
	}
	withInputRuntime(t, rt)
	actions := newTestInputActions()
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"tap":   func() error { _, err := actions.tapOnce(ctx, 1, 2); return err },
		"swipe": func() error { _, err := actions.swipe(ctx, 1, 2, 3, 4, 300); return err },
		"key":   func() error { _, err := actions.key(ctx, "back"); return err },
		"paste": func() error { _, err := actions.paste(ctx, "hi"); return err },
	} {
		err := call()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if !adb.IsError(err) {
			t.Errorf("%s: error is %T, want an adb.Error so server.py's catch equivalent sees it", name, err)
		}
		if err.Error() != "plugin scrcpy stream is not running" {
			t.Errorf("%s: message = %q", name, err.Error())
		}
	}
}

func TestInputScreenMutationHooksFire(t *testing.T) {
	rt := &inputFakeRuntime{
		status: InputRuntimeStatus{Streaming: true, DeviceWidth: 1080, DeviceHeight: 2280},
		active: true,
	}
	withInputRuntime(t, rt)

	calls := 0
	InputOnScreenMutation(func() { calls++ })
	t.Cleanup(func() {
		inputMutationHooks.mu.Lock()
		inputMutationHooks.fns = nil
		inputMutationHooks.mu.Unlock()
	})

	actions := newTestInputActions()
	ctx := context.Background()
	if _, err := actions.tapOnce(ctx, 1, 2); err != nil {
		t.Fatalf("tapOnce: %v", err)
	}
	if _, err := actions.swipe(ctx, 1, 2, 3, 4, 300); err != nil {
		t.Fatalf("swipe: %v", err)
	}
	if _, err := actions.key(ctx, "back"); err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := actions.paste(ctx, "hi"); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if calls != 4 {
		t.Errorf("ui_tree invalidation ran %d times, want 4", calls)
	}
}

// ---------------------------------------------------------------------------
// Validation that needs no device
// ---------------------------------------------------------------------------

func TestInputPasteRejectsEmptyTextBeforeTouchingAdb(t *testing.T) {
	actions := newTestInputActions()
	_, err := actions.paste(context.Background(), "")
	if err == nil || err.Error() != "text must not be empty" {
		t.Fatalf("paste(\"\") error = %v, want %q", err, "text must not be empty")
	}
}

// ---------------------------------------------------------------------------
// MCP surface
// ---------------------------------------------------------------------------

func newTestInputActions() *inputActions {
	return newInputActions(&mcpserver.Env{Log: mcpserver.NewLogger(os.Stderr)})
}

// inputToolContract mirrors docs/contract.json for the eight tools this file
// owns. Anything that drifts here is visible to Codex.
var inputToolContract = []struct {
	name        string
	description string
	required    []string
	// property -> [jsonType, title, default-as-JSON ("" for none)]
	props [][4]string
}{
	{
		name:        "phone_tap",
		description: "Tap native device pixels. By default verifies a screen change and retries nearby.",
		required:    []string{"x", "y"},
		props: [][4]string{
			{"x", "integer", "X", ""},
			{"y", "integer", "Y", ""},
			{"verify", "boolean", "Verify", "true"},
			{"retries", "integer", "Retries", "2"},
		},
	},
	{
		name:        "phone_tap_relative",
		description: "Tap normalized screenshot coordinates where x and y are between 0 and 1.",
		required:    []string{"x", "y"},
		props: [][4]string{
			{"x", "number", "X", ""},
			{"y", "number", "Y", ""},
			{"verify", "boolean", "Verify", "true"},
			{"retries", "integer", "Retries", "2"},
		},
	},
	{
		name: "phone_tap_image",
		description: "Tap a point measured on a displayed/resized screenshot.\n\n" +
			"Pass the exact width and height of the image coordinate space used to\n" +
			"choose x/y. The tool maps it into current native device pixels.\n",
		required: []string{"x", "y", "image_width", "image_height"},
		props: [][4]string{
			{"x", "integer", "X", ""},
			{"y", "integer", "Y", ""},
			{"image_width", "integer", "Image Width", ""},
			{"image_height", "integer", "Image Height", ""},
			{"verify", "boolean", "Verify", "true"},
			{"retries", "integer", "Retries", "2"},
		},
	},
	{
		name:        "phone_swipe",
		description: "Swipe from (x1,y1) to (x2,y2).",
		required:    []string{"x1", "y1", "x2", "y2"},
		props: [][4]string{
			{"x1", "integer", "X1", ""},
			{"y1", "integer", "Y1", ""},
			{"x2", "integer", "X2", ""},
			{"y2", "integer", "Y2", ""},
			{"duration_ms", "integer", "Duration Ms", "300"},
		},
	},
	{
		name:        "phone_long_press",
		description: "Long press at coordinates.",
		required:    []string{"x", "y"},
		props: [][4]string{
			{"x", "integer", "X", ""},
			{"y", "integer", "Y", ""},
			{"duration_ms", "integer", "Duration Ms", "1000"},
		},
	},
	{
		name:        "phone_key",
		description: "Press a key: back, home, recents, enter, delete, tab, menu, power, paste.",
		required:    []string{"name"},
		props:       [][4]string{{"name", "string", "Name", ""}},
	},
	{
		name:        "phone_type",
		description: "Type short ASCII text. Do not use for Chinese — use phone_paste instead.",
		required:    []string{"text"},
		props:       [][4]string{{"text", "string", "Text", ""}},
	},
	{
		name:        "phone_paste",
		description: "Paste text via device clipboard (supports Chinese and emoji).",
		required:    []string{"text"},
		props:       [][4]string{{"text", "string", "Text", ""}},
	},
}

// mcp.Tool.InputSchema/OutputSchema are typed `any`, so the assertions run on
// the marshaled JSON — which is what the client actually sees.
type inputSchemaJSON struct {
	Type       string          `json:"type"`
	Title      string          `json:"title"`
	Required   []string        `json:"required"`
	Properties json.RawMessage `json:"properties"`
}

type inputPropJSON struct {
	Type    string          `json:"type"`
	Title   string          `json:"title"`
	Default json.RawMessage `json:"default"`
}

func inputParseSchema(t *testing.T, schema any) inputSchemaJSON {
	t.Helper()
	if schema == nil {
		t.Fatal("schema is nil")
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var out inputSchemaJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal schema %s: %v", raw, err)
	}
	return out
}

func inputParseProperties(t *testing.T, raw json.RawMessage) map[string]inputPropJSON {
	t.Helper()
	out := map[string]inputPropJSON{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal properties %s: %v", raw, err)
	}
	return out
}

// inputPropertyOrder reports whether the property keys appear in `order` inside
// the raw properties object.
func inputPropertyOrder(raw string, order []string) bool {
	at := -1
	for _, name := range order {
		i := strings.Index(raw, `"`+name+`":`)
		if i < 0 || i < at {
			return false
		}
		at = i
	}
	return true
}

// inputListTools drives a real tools/list over an in-memory transport and
// returns the SERVER-side tools.
//
// The server-side copy matters: the client decodes inputSchema into a
// map[string]any, and re-marshalling a Go map sorts the keys, which would make
// any property-order assertion vacuously fail. Receiving middleware sees the
// result while InputSchema is still the *jsonschema.Schema that gets marshalled
// onto the wire.
func inputListTools(t *testing.T) map[string]*mcp.Tool {
	t.Helper()
	tools, _ := inputConnect(t)
	return tools
}

func inputConnect(t *testing.T) (map[string]*mcp.Tool, *mcp.ClientSession) {
	t.Helper()

	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{
		Name:  "phone-input",
		Order: mcpserver.OrderPhoneTools + 20,
		Apply: registerPhoneInput,
	})
	// A registration failure (nil or non-object InputSchema, bad URI) panics
	// inside AddTool, so simply building the server is the smoke test.
	server, err := mcpserver.New(context.Background(), mcpserver.Options{Registry: registry})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}

	captured := map[string]*mcp.Tool{}
	server.MCP.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method == "tools/list" {
				if list, ok := res.(*mcp.ListToolsResult); ok {
					for _, tool := range list.Tools {
						captured[tool.Name] = tool
					}
				}
			}
			return res, err
		}
	})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.MCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listed.Tools) != len(captured) {
		t.Fatalf("client saw %d tools, middleware captured %d", len(listed.Tools), len(captured))
	}
	return captured, session
}

// TestInputToolCallIsShapeAOverTheWire drives a real tools/call whose failure is
// reached before any adb work, so it needs no device.
func TestInputToolCallIsShapeAOverTheWire(t *testing.T) {
	_, session := inputConnect(t)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "phone_tap_relative",
		Arguments: map[string]any{"x": 5.0, "y": 0.5},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Error("phone_* tools report failures inside the JSON, not via isError")
	}
	const want = `{
  "ok": false,
  "error": "relative x and y must be between 0 and 1"
}`
	if len(res.Content) != 1 {
		t.Fatalf("content has %d blocks, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want a text block", res.Content[0])
	}
	if text.Text != want {
		t.Errorf("text block =\n%s\nwant\n%s", text.Text, want)
	}
	// structuredContent double-encodes the same string under "result".
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var got struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("unmarshal structuredContent %s: %v", structured, err)
	}
	if got.Result != want {
		t.Errorf("structuredContent.result =\n%s\nwant\n%s", got.Result, want)
	}
}

// TestInputToolCallValidatesArguments checks the two schema-driven gates, both
// reachable without a device: a required argument that is missing never reaches
// the handler, and an unknown key is rejected with the Python's exact wording.
func TestInputToolCallValidatesArguments(t *testing.T) {
	_, session := inputConnect(t)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "phone_tap",
		Arguments: map[string]any{"x": 1},
	})
	if err == nil && !res.IsError {
		t.Error("phone_tap without y should fail schema validation")
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "phone_key",
		Arguments: map[string]any{"name": "nope"},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, `"error": "Unknown key 'nope'. Supported: back, home, recents,`) {
		t.Errorf("unknown key payload =\n%s", text)
	}
}

func TestInputToolsMatchTheFrozenContract(t *testing.T) {
	byName := inputListTools(t)
	if len(byName) != len(inputToolContract) {
		t.Fatalf("registered %d tools, want %d: %v", len(byName), len(inputToolContract), byName)
	}

	for _, want := range inputToolContract {
		t.Run(want.name, func(t *testing.T) {
			tool, ok := byName[want.name]
			if !ok {
				t.Fatalf("tool %s is missing", want.name)
			}
			if tool.Description != want.description {
				t.Errorf("description =\n%q\nwant\n%q", tool.Description, want.description)
			}
			if tool.Title != "" {
				t.Errorf("title = %q, want empty (contract records null)", tool.Title)
			}
			if tool.Annotations != nil {
				t.Errorf("annotations = %+v, want nil", tool.Annotations)
			}
			if tool.Meta != nil {
				t.Errorf("_meta = %+v, want nil (phone_* tools carry none)", tool.Meta)
			}

			schema := inputParseSchema(t, tool.InputSchema)
			if schema.Type != "object" {
				t.Errorf("input schema type = %q, want object", schema.Type)
			}
			if schema.Title != want.name+"Arguments" {
				t.Errorf("input schema title = %q, want %q", schema.Title, want.name+"Arguments")
			}
			if fmt.Sprint(schema.Required) != fmt.Sprint(want.required) {
				t.Errorf("required = %v, want %v", schema.Required, want.required)
			}
			props := inputParseProperties(t, schema.Properties)
			if len(props) != len(want.props) {
				t.Errorf("property count = %d, want %d", len(props), len(want.props))
			}
			var order []string
			for _, p := range want.props {
				order = append(order, p[0])
				prop, ok := props[p[0]]
				if !ok {
					t.Errorf("property %s missing", p[0])
					continue
				}
				if prop.Type != p[1] {
					t.Errorf("property %s type = %q, want %q", p[0], prop.Type, p[1])
				}
				if prop.Title != p[2] {
					t.Errorf("property %s title = %q, want %q", p[0], prop.Title, p[2])
				}
				if got := string(prop.Default); got != p[3] {
					t.Errorf("property %s default = %q, want %q", p[0], got, p[3])
				}
			}
			// Declaration order must survive to the wire, because FastMCP
			// emitted the parameters in signature order.
			if got := inputPropertyOrder(string(schema.Properties), order); !got {
				t.Errorf("property order in %s is not %v", schema.Properties, order)
			}

			out := inputParseSchema(t, tool.OutputSchema)
			if out.Title != want.name+"Output" {
				t.Errorf("output schema title = %q, want %q", out.Title, want.name+"Output")
			}
			if fmt.Sprint(out.Required) != "[result]" {
				t.Errorf("output schema required = %v, want [result]", out.Required)
			}
			outProps := inputParseProperties(t, out.Properties)
			if prop, ok := outProps["result"]; !ok || prop.Type != "string" || prop.Title != "Result" {
				t.Errorf("output schema result property = %+v", prop)
			}
		})
	}
}

func TestInputToolFailureIsShapeA(t *testing.T) {
	// A failed call is still isError:false with the payload inside the text
	// block and {"result": "<same text>"} as structuredContent.
	res, out, err := inputResult(nil, &adb.Error{Msg: "adb not found."})
	if err != nil {
		t.Fatalf("inputResult returned an error: %v", err)
	}
	if res.IsError {
		t.Error("phone_* tools report failures inside the JSON, not via isError")
	}
	const want = `{
  "ok": false,
  "error": "adb not found."
}`
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want a text block", res.Content[0])
	}
	if text.Text != want {
		t.Errorf("text block =\n%s\nwant\n%s", text.Text, want)
	}
	if out.Result != want {
		t.Errorf("structuredContent.result =\n%s\nwant\n%s", out.Result, want)
	}
}

func TestInputToolSuccessAppendsOKLast(t *testing.T) {
	res, _, err := inputResult(jsonresult.Of("action", "tap"), nil)
	if err != nil {
		t.Fatalf("inputResult: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	const want = `{
  "action": "tap",
  "ok": true
}`
	if text != want {
		t.Errorf("payload =\n%s\nwant\n%s", text, want)
	}
	// And a payload that already carries ok keeps it where it was.
	res, _, _ = inputResult(inputTapPayload(1, 2, "s"), nil)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(probe["ok"]) != "true" {
		t.Errorf("ok = %s", probe["ok"])
	}
}

// ---------------------------------------------------------------------------
// Optional live check against the attached device
// ---------------------------------------------------------------------------

// TestInputLiveDevice exercises phone_paste against a real phone. It is skipped
// unless PHONE_AGENT_DEVICE_TESTS=1 because a concurrent scrcpy investigation
// may be holding the device, and because it types into whatever happens to be
// focused.
func TestInputLiveDevice(t *testing.T) {
	if os.Getenv("PHONE_AGENT_DEVICE_TESTS") != "1" {
		t.Skip("set PHONE_AGENT_DEVICE_TESTS=1 to run against the attached device")
	}
	actions := newTestInputActions()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	payload, err := actions.paste(ctx, "你好 ScrcpyMac 😀")
	if err != nil {
		t.Fatalf("paste: %v", err)
	}
	t.Logf("paste -> %s", jsonresult.Text(payload))
	// 14 code points: 2 Chinese + 2 spaces + "ScrcpyMac" + 1 astral emoji.
	// len() on the Go string would say 24.
	if length, _ := payload.Get("length"); length != 14 {
		t.Errorf("length = %v, want 14 runes", length)
	}

	// A verified tap on inert wallpaper: three real screencaps through the real
	// change score, ending in the give-up shape. Restore the launcher after.
	tapped, err := actions.tap(ctx, 540, 1140, inputTapOpts(true, 2))
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	t.Logf("verified tap -> %s", jsonresult.Text(tapped))
	verification, ok := tapped.Get("verification")
	if !ok {
		t.Fatal("a verified tap must carry a verification object")
	}
	attempts, _ := verification.(*jsonresult.Obj).Get("attempts")
	if list, ok := attempts.([]any); !ok || len(list) == 0 {
		t.Errorf("attempts = %#v, want a non-empty list", attempts)
	}
	if _, err := actions.key(ctx, "home"); err != nil {
		t.Errorf("restore home: %v", err)
	}
}

// TestInputChangeScoreOnCapturedScreens scores real screenshots so the
// area-average resample can be checked against Pillow's bicubic one. Point
// PHONE_AGENT_SHOT_DIR at a directory of PNGs; every pair is scored and printed.
//
// Run against the attached OnePlus 6 (four 1080x2280 captures: an idle screen
// twice, then the launcher, then the launcher again after a tap), Go scored
// 0 / 0.9912 / 0 and Pillow's bicubic scored 0 / 0.9897 / 0 on the same pairs —
// 0.15 % apart, the same side of the 0.035 threshold, and an exact 0.0 noise
// floor for two captures of a static screen.
func TestInputChangeScoreOnCapturedScreens(t *testing.T) {
	dir := os.Getenv("PHONE_AGENT_SHOT_DIR")
	if dir == "" {
		t.Skip("set PHONE_AGENT_SHOT_DIR to a directory of PNG screenshots")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	shots := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".png") {
			continue
		}
		data, err := os.ReadFile(dir + "/" + entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		names = append(names, entry.Name())
		shots[entry.Name()] = data
	}
	if len(names) < 2 {
		t.Skipf("need at least two PNGs in %s", dir)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			score := inputChangeScore(shots[names[i]], shots[names[j]])
			t.Logf("%s -> %s: %v changed=%v",
				names[i], names[j], jsonresult.PyRound(score, 4), score >= inputChangeThreshold)
		}
	}
	// A screenshot must always score exactly 0 against itself, or the retry
	// loop would report a landed tap for a frozen screen.
	if got := inputChangeScore(shots[names[0]], shots[names[0]]); got != 0 {
		t.Errorf("a screenshot scored %v against itself, want 0", got)
	}
}
