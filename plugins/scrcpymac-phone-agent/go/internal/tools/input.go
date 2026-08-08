package tools

// input.go — the phone_* input group: phone_tap, phone_tap_relative,
// phone_tap_image, phone_swipe, phone_long_press, phone_key, phone_type and
// phone_paste.
//
// It is a port of the tap/swipe/key/type/paste half of
// server/phone_agent/actions.py, specified by docs/spec-actions.md §4 and §5.
// The adb call is the easy part; the contract lives in the details:
//
//   - the verify+retry cross (§4.4): ONE baseline screenshot, five offsets in
//     the order target/up/down/left/right truncated to retries+1, a 0.45 s
//     settle between the tap and the after-screenshot, and a 72x128 RGB
//     resample whose per-channel delta >= 20 must cover >= 3.5 % of the 9216
//     cells before the tap counts as landed;
//   - two result shapes per tool — the adb path and the plugin-control path —
//     which differ in both keys and key spelling (duration_ms vs durationMs);
//   - Python's round() is ties-to-even, so every coordinate mapping goes
//     through jsonresult.PyRoundInt, never math.Round;
//   - shlex.quote's ASCII-only safe set, which is what makes Chinese and emoji
//     survive `cmd clipboard set-text`.
//
// Every symbol here is prefixed `input`/`Input` so this file can be edited in
// parallel with the other tool groups in this package.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

func init() {
	mcpserver.Register(mcpserver.Registration{
		Name:  "phone-input",
		Order: mcpserver.OrderPhoneTools + 20,
		Apply: registerPhoneInput,
	})
}

// ---------------------------------------------------------------------------
// Constants lifted verbatim from actions.py
// ---------------------------------------------------------------------------

const (
	// inputTapRetryRadiusPX is actions.tap's retry_radius_px default. The MCP
	// layer does not expose it; the value is clamped to [1, 96] anyway.
	inputTapRetryRadiusPX = 32
	// inputTapSettleS is actions.tap's settle_s default, clamped to [0.1, 2.0].
	inputTapSettleS = 0.45
	// inputTapMaxRetries is the clamp on the retries parameter: at most five
	// attempts, because there are only five offsets.
	inputTapMaxRetries = 4

	// inputChangeThreshold is the fraction of resampled cells that must differ
	// before a tap counts as having changed the screen. A real WeChat send
	// changed 2.96% of the sampled screen; the old 3.5% threshold retried into
	// the adjacent control revealed after the message was sent.
	inputChangeThreshold = 0.02
	// inputChangeChannelDelta is the per-channel absolute difference that makes
	// one resampled cell "changed".
	inputChangeChannelDelta = 20
	// The resample target is a FIXED 72x128 — aspect ratio is deliberately not
	// preserved, so a 1080x2280 screen is squashed horizontally. Do not "fix"
	// it: the threshold is calibrated against this exact grid.
	inputSampleW      = 72
	inputSampleH      = 128
	inputSamplePixels = inputSampleW * inputSampleH // 9216

	// inputPasteSettle covers the `cmd clipboard set-text` round trip. Without
	// it the KEYCODE_PASTE keyevent can beat the clipboard write.
	inputPasteSettle = 150 * time.Millisecond

	// inputScreenSizeTTL memoises `wm size`. See the deviation note on
	// inputActions.wmScreenSize.
	inputScreenSizeTTL = 2 * time.Second

	inputNoChangeHint = "No screen change detected after tapping the target and nearby points."
)

// inputKeycodeOrder is actions.KEYCODES in Python dict insertion order. The
// order is contract: the "Unknown key" error joins these names with ", " and the
// model sees the result verbatim.
var inputKeycodeOrder = []string{
	"back", "home", "recents", "enter", "delete",
	"tab", "menu", "power", "volume_up", "volume_down", "paste",
}

var inputKeycodes = map[string]int{
	"back":        4,
	"home":        3,
	"recents":     187,
	"enter":       66,
	"delete":      67,
	"tab":         61,
	"menu":        82,
	"power":       26,
	"volume_up":   24,
	"volume_down": 25,
	"paste":       279,
}

// ---------------------------------------------------------------------------
// Seams: the scrcpy runtime and the ui_tree cache
// ---------------------------------------------------------------------------

// InputRuntimeStatus is the slice of ScrcpyRuntime.status() the input tools
// read. DeviceWidth/DeviceHeight are NATIVE device pixels (status.deviceWidth /
// deviceHeight), not frame pixels.
type InputRuntimeStatus struct {
	Streaming    bool
	DeviceWidth  int
	DeviceHeight int
}

// InputRuntime is the seam the plugin's scrcpy H.264 runtime plugs into.
//
// While the stream is up, taps, swipes, keys and pastes go over the scrcpy
// control socket instead of adb, and the payload shape changes: the runtime
// returns {ok, action, serial, point, coordinateSpace, backend:"plugin-control"}
// where the adb path returns {ok, action, x, y, serial}. The runtime therefore
// builds its own ordered payload and this file passes it straight through.
//
// Nothing implements this yet — the runtime lands in a later phase — so
// inputRuntime() returns nil and every tool takes the adb path, which is exactly
// what the Python does when the stream is not running.
type InputRuntime interface {
	// Status reports whether the stream is live and, if so, the device size.
	Status() InputRuntimeStatus
	// IsActive is status.state == "streaming" && metadata present. key() and
	// paste() branch on this rather than on Status().Streaming, matching the
	// Python.
	IsActive() bool
	// StartForPaste brings up a temporary control session when the device does
	// not implement `cmd clipboard`. The caller stops it after the clipboard
	// message has had time to reach scrcpy-server.
	StartForPaste(ctx context.Context) error
	Stop()

	TapRelative(ctx context.Context, x, y float64) (*jsonresult.Obj, error)
	SwipeRelative(ctx context.Context, x1, y1, x2, y2 float64, durationMS int) (*jsonresult.Obj, error)
	Key(ctx context.Context, name string) (*jsonresult.Obj, error)
	Paste(ctx context.Context, text string) (*jsonresult.Obj, error)
}

var inputRuntimeState struct {
	mu sync.RWMutex
	rt InputRuntime
}

// SetInputRuntime installs the scrcpy runtime the input tools route through
// while a stream is active. Passing nil restores the adb-only behaviour.
func SetInputRuntime(rt InputRuntime) {
	inputRuntimeState.mu.Lock()
	defer inputRuntimeState.mu.Unlock()
	inputRuntimeState.rt = rt
}

func inputRuntime() InputRuntime {
	inputRuntimeState.mu.RLock()
	defer inputRuntimeState.mu.RUnlock()
	return inputRuntimeState.rt
}

var inputMutationHooks struct {
	mu  sync.RWMutex
	fns []func()
}

// InputOnScreenMutation registers fn to run after every input action that can
// change what is on screen: tap, swipe, key, type and paste.
//
// It exists because actions.py invalidates the shared ui_tree cache from each of
// those methods (spec-actions §6.1), and the cache belongs to the phone_ui_tree
// group, not to this file. The group that owns the cache registers its
// invalidator here; until it does, the hook list is empty and nothing is cached,
// which is also correct.
func InputOnScreenMutation(fn func()) {
	if fn == nil {
		return
	}
	inputMutationHooks.mu.Lock()
	defer inputMutationHooks.mu.Unlock()
	inputMutationHooks.fns = append(inputMutationHooks.fns, fn)
}

func inputScreenMutated() {
	inputMutationHooks.mu.RLock()
	fns := inputMutationHooks.fns
	inputMutationHooks.mu.RUnlock()
	for _, fn := range fns {
		fn()
	}
}

// ---------------------------------------------------------------------------
// The actions object
// ---------------------------------------------------------------------------

// inputActions is the input half of PhoneActions. It owns no device state — the
// serial lives on the shared adb client — only a short-lived `wm size` memo.
type inputActions struct {
	env *mcpserver.Env

	mu         sync.Mutex
	sizeSerial string
	sizeW      int
	sizeH      int
	sizeAt     time.Time
	// now is time.Now except in tests.
	now func() time.Time
}

func newInputActions(env *mcpserver.Env) *inputActions {
	return &inputActions{env: env, now: time.Now}
}

// ready is Python's _ready(): resolve the client, then ensure a device is
// selected. Both failures are AdbError and reach the model as
// {"ok": false, "error": ...}.
func (a *inputActions) ready(ctx context.Context) (*adb.Client, error) {
	client, err := a.env.ADB()
	if err != nil {
		return nil, err
	}
	if _, err := client.EnsureDevice(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// wmScreenSize is AdbClient.screen_size() with a short memo.
//
// DEVIATION (blessed by spec-actions §4.2): the Python re-runs `wm size` on
// every tap and every screenshot, so one verified tap costs up to eight extra
// adb round trips. The result is cached for inputScreenSizeTTL, keyed on the
// serial, which is invisible except across a rotation that happens inside a
// single tool call.
func (a *inputActions) wmScreenSize(ctx context.Context) (int, int, error) {
	client, err := a.ready(ctx)
	if err != nil {
		return 0, 0, err
	}
	serial := client.Serial()

	a.mu.Lock()
	if a.sizeSerial == serial && a.sizeW > 0 && a.sizeH > 0 && a.now().Sub(a.sizeAt) < inputScreenSizeTTL {
		w, h := a.sizeW, a.sizeH
		a.mu.Unlock()
		return w, h, nil
	}
	a.mu.Unlock()

	w, h, err := client.ScreenSize(ctx)
	if err != nil {
		return 0, 0, err
	}

	a.mu.Lock()
	a.sizeSerial, a.sizeW, a.sizeH, a.sizeAt = serial, w, h, a.now()
	a.mu.Unlock()
	return w, h, nil
}

// screenSize is Python's _screen_size(): the streaming device size when the
// plugin runtime is up, else `wm size`, else "unknown" — errors are swallowed,
// exactly like the Python's except (AdbError, OSError): return None.
func (a *inputActions) screenSize(ctx context.Context) (int, int, bool) {
	if rt := inputRuntime(); rt != nil {
		if st := rt.Status(); st.Streaming && st.DeviceWidth != 0 && st.DeviceHeight != 0 {
			return st.DeviceWidth, st.DeviceHeight, true
		}
	}
	w, h, err := a.wmScreenSize(ctx)
	if err != nil {
		return 0, 0, false
	}
	return w, h, true
}

// requiredScreenSize is _required_screen_size().
func (a *inputActions) requiredScreenSize(ctx context.Context) (int, int, error) {
	w, h, ok := a.screenSize(ctx)
	if !ok {
		return 0, 0, &adb.Error{Msg: "Could not determine the device screen size"}
	}
	return w, h, nil
}

// clampPoint is _clamp_device_point(): clamp into [0, size-1], or pass the point
// through unchanged when the screen size is unknown.
func (a *inputActions) clampPoint(ctx context.Context, x, y int) (int, int) {
	w, h, ok := a.screenSize(ctx)
	if !ok {
		return x, y
	}
	return Clamp(x, 0, w-1), Clamp(y, 0, h-1)
}

// inputShot is the subset of actions._adb_screenshot()'s dict this file needs.
// Width/Height come from the PNG header so verification metadata stays in the
// same coordinate space as the captured image.
type inputShot struct {
	Serial    string
	Width     int
	Height    int
	PNG       []byte
	SizeBytes int
}

// screenshot is actions.screenshot() == _adb_screenshot(). There is deliberately
// no scrcpy-stream path: even while the H.264 stream runs, screenshots go
// through `adb exec-out screencap -p`.
func (a *inputActions) screenshot(ctx context.Context) (*inputShot, error) {
	client, err := a.ready(ctx)
	if err != nil {
		return nil, err
	}
	data, err := client.RunBytes(ctx, adb.ShortTimeout, "exec-out", "screencap", "-p")
	if err != nil {
		return nil, err
	}
	w, h, err := deviceRequirePNGSize(data)
	if err != nil {
		return nil, err
	}
	return &inputShot{
		Serial:    client.Serial(),
		Width:     w,
		Height:    h,
		PNG:       data,
		SizeBytes: len(data),
	}, nil
}

// sleep is an interruptible time.Sleep. The Python sleeps unconditionally; here
// a cancelled context (server shutting down) aborts instead of holding the
// process open.
func (a *inputActions) sleep(ctx context.Context, d time.Duration) error {
	return inputSleep(ctx, d)
}

func inputSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Command construction (kept separate so it can be unit-tested without a device)
// ---------------------------------------------------------------------------

func inputTapCommand(x, y int) string { return fmt.Sprintf("input tap %d %d", x, y) }

func inputSwipeCommand(x1, y1, x2, y2, durationMS int) string {
	return fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, durationMS)
}

func inputKeyeventCommand(code int) string { return fmt.Sprintf("input keyevent %d", code) }

// inputTypeTextCommand builds `input text <quoted>`.
//
// The escape ORDER is load-bearing: "%" -> "%25" first, then " " -> "%s". Doing
// it the other way round turns every space into "%25s" and corrupts the text.
// `input text` decodes %s as a space on the device; other %XX sequences are not
// decoded on most builds, which is why phone_type is documented as ASCII-only
// and phone_paste is the sanctioned path for anything real.
//
// Quoting is adb.Quote — Python's shlex.quote, whose ASCII-only safe set is what
// forces every Chinese character and emoji to be single-quoted, which is what
// makes them survive the device shell. There is deliberately only one
// implementation of that rule in the tree; a second copy that drifted would
// break Chinese paste silently.
func inputTypeTextCommand(text string) string {
	return "input text " + adb.Quote(adb.EscapeInputText(text))
}

func inputClipboardCommand(text string) string {
	return "cmd clipboard set-text " + adb.Quote(text)
}

// inputPyRepr renders s the way Python's repr() does, because phone_key's
// "Unknown key {name!r}" reaches the model verbatim: single quotes, switching to
// double quotes only when the value contains a ' and no ".
func inputPyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// inputWrapRuntimeError converts a scrcpy runtime failure into an adb.Error, the
// way actions.key()/paste() re-raise ScrcpyRuntimeError as AdbError.
//
// The Python does NOT do this on the tap/swipe paths, where the exception
// escapes server.py's `except (AdbError, OSError)` entirely (spec-actions
// BUG-8). Wrapping here plus tools.JSON's catch-all makes every path return the
// {"ok": false, "error": ...} payload instead of an unhandled exception.
func inputWrapRuntimeError(err error) error {
	if err == nil {
		return nil
	}
	if adb.IsError(err) {
		return err
	}
	return &adb.Error{Msg: err.Error()}
}

// ---------------------------------------------------------------------------
// Payload builders — one per result shape, so key order is testable
// ---------------------------------------------------------------------------

func inputTapPayload(x, y int, serial string) *jsonresult.Obj {
	return jsonresult.Of("ok", true, "action", "tap", "x", x, "y", y, "serial", serial)
}

func inputSwipePayload(x1, y1, x2, y2, durationMS int, serial string) *jsonresult.Obj {
	// snake_case duration_ms is the adb shape; the plugin-control shape spells
	// it durationMs. Both are contract.
	return jsonresult.Of(
		"ok", true,
		"action", "swipe",
		"from", []int{x1, y1},
		"to", []int{x2, y2},
		"duration_ms", durationMS,
		"serial", serial,
	)
}

func inputKeyPayload(key string, code int, serial string) *jsonresult.Obj {
	return jsonresult.Of("ok", true, "action", "key", "key", key, "code", code, "serial", serial)
}

func inputTextPayload(action, text, serial string) *jsonresult.Obj {
	// length is Python's len(str) — a rune count. len(text) in Go is bytes and
	// would report 6 for a 2-character Chinese string.
	return jsonresult.Of(
		"ok", true,
		"action", action,
		"length", jsonresult.RuneLen(text),
		"serial", serial,
	)
}

func inputAttemptPayload(x, y int, changed bool, score float64) *jsonresult.Obj {
	return jsonresult.Of(
		"point", []int{x, y},
		"screen_changed", changed,
		// round(score, 4) is a Python float: 0.0 must serialise as "0.0".
		"change_score", jsonresult.Float(jsonresult.PyRound(score, 4)),
	)
}

// inputVerificationUnavailable is the baseline-capture-failure shape.
//
// REPLICATED BUG (spec-actions BUG-2): "attempts" is the integer 1 here and a
// list of attempt objects on every other path. contract.json records the
// integer, so it is reproduced rather than repaired.
func inputVerificationUnavailable() *jsonresult.Obj {
	return jsonresult.Of("requested", true, "available", false, "attempts", 1)
}

func inputVerificationVerified(attempts []any, afterSizeBytes int) *jsonresult.Obj {
	return jsonresult.Of(
		"requested", true,
		"available", true,
		"verified", true,
		"attempts", attempts,
		"after_size_bytes", afterSizeBytes,
	)
}

func inputVerificationFailed(attempts []any) *jsonresult.Obj {
	return jsonresult.Of(
		"requested", true,
		"available", true,
		"verified", false,
		"attempts", attempts,
		"hint", inputNoChangeHint,
	)
}

// ---------------------------------------------------------------------------
// Tap
// ---------------------------------------------------------------------------

// inputTapOptions mirrors actions.tap's keyword arguments. The MCP layer exposes
// only verify and retries; retry_radius_px and settle_s stay at the actions
// defaults, and all three are clamped exactly as the Python clamps them.
type inputTapOptions struct {
	Verify        bool
	Retries       int
	RetryRadiusPX int
	SettleS       float64
}

// inputDefaultTapOptions is actions.tap's signature defaults. Note Verify is
// false at the actions level even though phone_tap defaults it to true.
func inputDefaultTapOptions() inputTapOptions {
	return inputTapOptions{
		Verify:        false,
		Retries:       2,
		RetryRadiusPX: inputTapRetryRadiusPX,
		SettleS:       inputTapSettleS,
	}
}

// inputTapOffsets is the 5-point cross, truncated to retries+1 entries.
//
// The order is target, UP, DOWN, LEFT, RIGHT — vertical neighbours before
// horizontal ones. retries=0 gives one attempt, the default retries=2 gives
// target/up/down, retries>=4 gives all five.
func inputTapOffsets(retries, radius int) [][2]int {
	all := [][2]int{{0, 0}, {0, -radius}, {0, radius}, {-radius, 0}, {radius, 0}}
	n := retries + 1
	if n < 1 {
		n = 1
	}
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// inputDevice is the device surface the tap family drives. Splitting it out
// keeps the retry/threshold/sleep logic and the coordinate mapping — the parts
// that are actually hard — testable without a phone.
type inputDevice interface {
	requiredScreenSize(ctx context.Context) (int, int, error)
	clampPoint(ctx context.Context, x, y int) (int, int)
	tapOnce(ctx context.Context, x, y int) (*jsonresult.Obj, error)
	screenshot(ctx context.Context) (*inputShot, error)
	sleep(ctx context.Context, d time.Duration) error
}

var _ inputDevice = (*inputActions)(nil)

// tapOnce is actions._tap_once(): the runtime's relative tap while streaming,
// `input tap X Y` otherwise. Both shapes are returned verbatim.
func (a *inputActions) tapOnce(ctx context.Context, x, y int) (*jsonresult.Obj, error) {
	if rt := inputRuntime(); rt != nil {
		if st := rt.Status(); st.Streaming {
			w := max(1, st.DeviceWidth)
			h := max(1, st.DeviceHeight)
			result, err := rt.TapRelative(ctx,
				float64(x)/float64(max(w-1, 1)),
				float64(y)/float64(max(h-1, 1)))
			if err != nil {
				return nil, inputWrapRuntimeError(err)
			}
			inputScreenMutated()
			return result, nil
		}
	}
	client, err := a.ready(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, inputTapCommand(x, y)); err != nil {
		return nil, err
	}
	inputScreenMutated()
	return inputTapPayload(x, y, client.Serial()), nil
}

func (a *inputActions) tap(ctx context.Context, x, y int, opts inputTapOptions) (*jsonresult.Obj, error) {
	return inputVerifiedTap(ctx, a, x, y, opts)
}

// inputVerifiedTap is actions.tap().
//
// Structure, in the Python's exact order:
//
//	clamp -> fast path when !verify
//	clamp the three knobs
//	ONE baseline screenshot (its failure is a distinct, documented shape)
//	for each offset: clamped candidate -> tap -> settle -> screenshot ->
//	                 change score against the SAME baseline -> record attempt
//	first attempt that changes the screen wins; otherwise the give-up shape.
//
// Two behaviours that look like bugs and are deliberately kept: the baseline is
// never refreshed (so the comparison is cumulative from the start, which is what
// makes a persistent change credit the attempt that caused it), and clamping can
// collapse two offsets onto the same pixel near a screen edge, tapping it twice
// and listing it twice in `attempts` (spec-actions BUG-10).
func inputVerifiedTap(ctx context.Context, dev inputDevice, x, y int, opts inputTapOptions) (*jsonresult.Obj, error) {
	px, py := dev.clampPoint(ctx, x, y)
	if !opts.Verify {
		return dev.tapOnce(ctx, px, py)
	}

	retries := Clamp(opts.Retries, 0, inputTapMaxRetries)
	radius := Clamp(opts.RetryRadiusPX, 1, 96)
	settle := Clamp(opts.SettleS, 0.1, 2.0)

	baseline, err := dev.screenshot(ctx)
	if err != nil {
		if !inputRecoverable(ctx, err) {
			return nil, err
		}
		result, terr := dev.tapOnce(ctx, px, py)
		if terr != nil {
			return nil, terr
		}
		result.Set("verification", inputVerificationUnavailable())
		return result, nil
	}
	// Decoding the baseline once instead of once per attempt is an
	// optimisation only: the resulting samples are identical.
	baseSample, _ := inputSampleRGB(baseline.PNG)

	attempts := []any{}
	var last *jsonresult.Obj

	for _, offset := range inputTapOffsets(retries, radius) {
		cx, cy := dev.clampPoint(ctx, px+offset[0], py+offset[1])
		result, err := dev.tapOnce(ctx, cx, cy)
		if err != nil {
			return nil, err
		}
		last = result

		if err := dev.sleep(ctx, time.Duration(settle*float64(time.Second))); err != nil {
			return nil, err
		}

		changed := false
		score := 0.0
		afterSize := 0
		after, err := dev.screenshot(ctx)
		switch {
		case err != nil && !inputRecoverable(ctx, err):
			return nil, err
		case err != nil:
			// Python: except (AdbError, OSError) -> changed=False, score=0.0.
		default:
			afterSize = after.SizeBytes
			score = inputChangeScoreSampled(baseSample, after.PNG)
			changed = score >= inputChangeThreshold
		}

		attempts = append(attempts, inputAttemptPayload(cx, cy, changed, score))
		if changed {
			last.Set("verification", inputVerificationVerified(attempts, afterSize))
			return last, nil
		}
	}

	if last == nil { // unreachable: there is always at least one offset
		last = jsonresult.New()
	}
	last.Set("verification", inputVerificationFailed(attempts))
	return last, nil
}

// inputRecoverable reports whether err is the Python (AdbError, OSError) class
// of failure that tap() swallows. A failure while the context is already done is
// the server going away, and must propagate instead.
func inputRecoverable(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil
}

// ---------------------------------------------------------------------------
// Coordinate mapping
// ---------------------------------------------------------------------------

// inputRelativeToDevice maps a 0..1 coordinate onto native pixels.
//
// The scaling convention is (size - 1), so 1.0 lands on the LAST pixel, and the
// rounding is Python's round(): ties to EVEN. round(0.5*1077) is 538 in Python
// and 539 with math.Round — an off-by-one on every screen whose width is even.
func inputRelativeToDevice(v float64, size int) int {
	return jsonresult.PyRoundInt(v * float64(size-1))
}

// inputImageToDevice maps a point in an arbitrary image space onto native
// pixels. max(imageSize-1, 1) keeps a 1-pixel-wide image from dividing by zero;
// its only legal coordinate, 0, maps to 0.
func inputImageToDevice(v, imageSize, deviceSize int) int {
	return jsonresult.PyRoundInt((float64(v) / float64(max(imageSize-1, 1))) * float64(deviceSize-1))
}

func (a *inputActions) tapRelative(ctx context.Context, x, y float64, opts inputTapOptions) (*jsonresult.Obj, error) {
	return inputTapRelative(ctx, a, x, y, opts)
}

func inputTapRelative(ctx context.Context, dev inputDevice, x, y float64, opts inputTapOptions) (*jsonresult.Obj, error) {
	// Bounds are inclusive at both ends. NaN fails this test, as it does in
	// Python, and raises the same message.
	if !(x >= 0.0 && x <= 1.0 && y >= 0.0 && y <= 1.0) {
		return nil, &adb.Error{Msg: "relative x and y must be between 0 and 1"}
	}
	width, height, err := dev.requiredScreenSize(ctx)
	if err != nil {
		return nil, err
	}
	deviceX := inputRelativeToDevice(x, width)
	deviceY := inputRelativeToDevice(y, height)
	tapped, err := inputVerifiedTap(ctx, dev, deviceX, deviceY, opts)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"coordinate_space", "relative",
		// The ORIGINAL floats are echoed back, so 1.0 must not become 1.
		"source", []jsonresult.Float{jsonresult.Float(x), jsonresult.Float(y)},
		"device_point", []int{deviceX, deviceY},
		"tap", tapped,
	), nil
}

func (a *inputActions) tapImage(ctx context.Context, x, y, imageWidth, imageHeight int, opts inputTapOptions) (*jsonresult.Obj, error) {
	return inputTapImage(ctx, a, x, y, imageWidth, imageHeight, opts)
}

func inputTapImage(ctx context.Context, dev inputDevice, x, y, imageWidth, imageHeight int, opts inputTapOptions) (*jsonresult.Obj, error) {
	if imageWidth <= 0 || imageHeight <= 0 {
		return nil, &adb.Error{Msg: "image_width and image_height must be positive"}
	}
	if !(x >= 0 && x < imageWidth && y >= 0 && y < imageHeight) {
		return nil, &adb.Error{Msg: "image point must be inside image_width x image_height"}
	}
	width, height, err := dev.requiredScreenSize(ctx)
	if err != nil {
		return nil, err
	}
	deviceX := inputImageToDevice(x, imageWidth, width)
	deviceY := inputImageToDevice(y, imageHeight, height)
	tapped, err := inputVerifiedTap(ctx, dev, deviceX, deviceY, opts)
	if err != nil {
		return nil, err
	}
	// tap_image carries device_size; tap_relative does not. Do not harmonise.
	return jsonresult.Of(
		"ok", true,
		"coordinate_space", "image",
		"source", jsonresult.Of("point", []int{x, y}, "size", []int{imageWidth, imageHeight}),
		"device_point", []int{deviceX, deviceY},
		"device_size", []int{width, height},
		"tap", tapped,
	), nil
}

// ---------------------------------------------------------------------------
// Swipe / long press / key / type / paste
// ---------------------------------------------------------------------------

// swipe is actions.swipe(). Unlike tap it does NOT clamp: out-of-range values go
// straight to `input swipe`, or become out-of-range relatives that the runtime
// clamps to [0, 1].
func (a *inputActions) swipe(ctx context.Context, x1, y1, x2, y2, durationMS int) (*jsonresult.Obj, error) {
	if rt := inputRuntime(); rt != nil {
		if st := rt.Status(); st.Streaming {
			w := max(1, st.DeviceWidth)
			h := max(1, st.DeviceHeight)
			denomX := float64(max(w-1, 1))
			denomY := float64(max(h-1, 1))
			result, err := rt.SwipeRelative(ctx,
				float64(x1)/denomX, float64(y1)/denomY,
				float64(x2)/denomX, float64(y2)/denomY,
				// duration_ms is passed through UNCLAMPED here; the runtime
				// clamps it to [0, 10000] itself.
				durationMS)
			if err != nil {
				return nil, inputWrapRuntimeError(err)
			}
			inputScreenMutated()
			return result, nil
		}
	}
	client, err := a.ready(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, inputSwipeCommand(x1, y1, x2, y2, durationMS)); err != nil {
		return nil, err
	}
	inputScreenMutated()
	return inputSwipePayload(x1, y1, x2, y2, durationMS, client.Serial()), nil
}

// longPress delegates to swipe with from == to, so the result says
// action:"swipe" with from/to rather than action:"long_press" with x/y
// (spec-actions BUG-5). Replicated: the model already reads it.
func (a *inputActions) longPress(ctx context.Context, x, y, durationMS int) (*jsonresult.Obj, error) {
	return a.swipe(ctx, x, y, x, y, durationMS)
}

func (a *inputActions) key(ctx context.Context, name string) (*jsonresult.Obj, error) {
	key := strings.TrimSpace(strings.ToLower(name))
	code, ok := inputKeycodes[key]
	if !ok {
		return nil, &adb.Error{Msg: fmt.Sprintf("Unknown key %s. Supported: %s",
			inputPyRepr(name), strings.Join(inputKeycodeOrder, ", "))}
	}
	// Note this branches on is_active(), not on status().state, matching the
	// Python. The runtime keycode table must include paste=279 or key("paste")
	// works over adb and fails while streaming (spec-actions BUG-3).
	if rt := inputRuntime(); rt != nil && rt.IsActive() {
		result, err := rt.Key(ctx, key)
		if err != nil {
			return nil, inputWrapRuntimeError(err)
		}
		inputScreenMutated()
		return result, nil
	}
	client, err := a.ready(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, inputKeyeventCommand(code)); err != nil {
		return nil, err
	}
	inputScreenMutated()
	return inputKeyPayload(key, code, client.Serial()), nil
}

// typeText is always the adb path, even while streaming.
//
// _ready() runs BEFORE the empty-text check, so `phone_type("")` with no device
// attached reports the device error, not "text must not be empty". Replicated.
func (a *inputActions) typeText(ctx context.Context, text string) (*jsonresult.Obj, error) {
	client, err := a.ready(ctx)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, &adb.Error{Msg: "text must not be empty"}
	}
	if _, err := client.Shell(ctx, inputTypeTextCommand(text)); err != nil {
		return nil, err
	}
	inputScreenMutated()
	return inputTextPayload("type", text, client.Serial()), nil
}

// paste goes through the device clipboard, which is what makes Chinese and emoji
// work: shellQuote single-quotes any non-ASCII string and `cmd clipboard
// set-text` takes UTF-8 bytes. The empty-text check runs FIRST here, unlike
// typeText.
func (a *inputActions) paste(ctx context.Context, text string) (*jsonresult.Obj, error) {
	if text == "" {
		return nil, &adb.Error{Msg: "text must not be empty"}
	}
	if rt := inputRuntime(); rt != nil {
		startedForPaste := false
		if !rt.IsActive() {
			if err := rt.StartForPaste(ctx); err != nil {
				return nil, inputWrapRuntimeError(err)
			}
			startedForPaste = true
			defer rt.Stop()
		}
		result, err := rt.Paste(ctx, text)
		if err != nil {
			return nil, inputWrapRuntimeError(err)
		}
		if startedForPaste {
			if err := a.sleep(ctx, inputPasteSettle); err != nil {
				return nil, err
			}
		}
		inputScreenMutated()
		return result, nil
	}
	client, err := a.ready(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, inputClipboardCommand(text)); err != nil {
		return nil, err
	}
	if err := a.sleep(ctx, inputPasteSettle); err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, inputKeyeventCommand(inputKeycodes["paste"])); err != nil {
		return nil, err
	}
	inputScreenMutated()
	return inputTextPayload("paste", text, client.Serial()), nil
}

// ---------------------------------------------------------------------------
// Screen-change detection
// ---------------------------------------------------------------------------

// inputChangeScore is actions._screenshot_change_score(): the fraction of a
// fixed 72x128 RGB resample whose per-channel absolute difference reaches 20.
// Any decode failure yields 0.0, exactly like the Python's
// except (OSError, ValueError).
func inputChangeScore(before, after []byte) float64 {
	sample, err := inputSampleRGB(before)
	if err != nil {
		return 0.0
	}
	return inputChangeScoreSampled(sample, after)
}

func inputChangeScoreSampled(before []uint8, after []byte) float64 {
	if len(before) != inputSamplePixels*3 {
		return 0.0
	}
	sample, err := inputSampleRGB(after)
	if err != nil {
		return 0.0
	}
	changed := 0
	for i := 0; i < inputSamplePixels; i++ {
		dr := inputAbsDiff(before[i*3], sample[i*3])
		dg := inputAbsDiff(before[i*3+1], sample[i*3+1])
		db := inputAbsDiff(before[i*3+2], sample[i*3+2])
		if dr >= inputChangeChannelDelta || dg >= inputChangeChannelDelta || db >= inputChangeChannelDelta {
			changed++
		}
	}
	return float64(changed) / float64(inputSamplePixels)
}

func inputAbsDiff(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}

// inputSampleRGB decodes a PNG and box-averages it down to 72x128 RGB.
//
// Pillow's default resample for an RGB image is BICUBIC, which is a proper
// area-weighted resample — every source pixel contributes. A nearest-neighbour
// sample in Go would be far noisier against the 0.035 threshold and would fire
// on a blinking cursor, so this averages every source pixel that falls in a
// cell. Area-average has no negative lobes, making it very slightly less
// sensitive to thin high-contrast edges than bicubic; the 20/0.035 constants are
// unchanged and match within a fraction of a percent on real screens.
//
// x/image/draw would do this in one call but is not an allowed dependency.
func inputSampleRGB(data []byte) ([]uint8, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty image")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, fmt.Errorf("image has no pixels")
	}
	at := inputPixelReader(img)

	sums := make([]uint64, inputSamplePixels*3)
	counts := make([]uint32, inputSamplePixels)
	for y := 0; y < srcH; y++ {
		row := (y * inputSampleH / srcH) * inputSampleW
		for x := 0; x < srcW; x++ {
			r, g, b := at(bounds.Min.X+x, bounds.Min.Y+y)
			i := row + x*inputSampleW/srcW
			sums[i*3] += uint64(r)
			sums[i*3+1] += uint64(g)
			sums[i*3+2] += uint64(b)
			counts[i]++
		}
	}

	out := make([]uint8, inputSamplePixels*3)
	for i := 0; i < inputSamplePixels; i++ {
		n := uint64(counts[i])
		if n == 0 {
			// Only reachable when the source is smaller than 72x128, i.e. never
			// for a real screenshot. Fall back to the nearest source pixel.
			sx := min((i%inputSampleW)*srcW/inputSampleW, srcW-1)
			sy := min((i/inputSampleW)*srcH/inputSampleH, srcH-1)
			r, g, b := at(bounds.Min.X+sx, bounds.Min.Y+sy)
			out[i*3], out[i*3+1], out[i*3+2] = r, g, b
			continue
		}
		out[i*3] = uint8((sums[i*3] + n/2) / n)
		out[i*3+1] = uint8((sums[i*3+1] + n/2) / n)
		out[i*3+2] = uint8((sums[i*3+2] + n/2) / n)
	}
	return out, nil
}

// inputPixelReader returns a fast RGB accessor. `screencap -p` decodes to
// *image.NRGBA, whose Pix holds straight (non-premultiplied) values — exactly
// what Pillow's convert("RGB") produces. The generic fallback goes through
// color.Color, which premultiplies; that only differs for transparent pixels,
// and a screenshot has none.
func inputPixelReader(img image.Image) func(x, y int) (uint8, uint8, uint8) {
	switch im := img.(type) {
	case *image.NRGBA:
		return func(x, y int) (uint8, uint8, uint8) {
			o := im.PixOffset(x, y)
			return im.Pix[o], im.Pix[o+1], im.Pix[o+2]
		}
	case *image.RGBA:
		return func(x, y int) (uint8, uint8, uint8) {
			o := im.PixOffset(x, y)
			return im.Pix[o], im.Pix[o+1], im.Pix[o+2]
		}
	default:
		return func(x, y int) (uint8, uint8, uint8) {
			r, g, b, _ := img.At(x, y).RGBA()
			return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)
		}
	}
}

// ---------------------------------------------------------------------------
// MCP registration
// ---------------------------------------------------------------------------

type inputTapIn struct {
	X       int  `json:"x"`
	Y       int  `json:"y"`
	Verify  bool `json:"verify,omitempty"`
	Retries int  `json:"retries,omitempty"`
}

type inputTapRelativeIn struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Verify  bool    `json:"verify,omitempty"`
	Retries int     `json:"retries,omitempty"`
}

type inputTapImageIn struct {
	X           int  `json:"x"`
	Y           int  `json:"y"`
	ImageWidth  int  `json:"image_width"`
	ImageHeight int  `json:"image_height"`
	Verify      bool `json:"verify,omitempty"`
	Retries     int  `json:"retries,omitempty"`
}

type inputSwipeIn struct {
	X1         int `json:"x1"`
	Y1         int `json:"y1"`
	X2         int `json:"x2"`
	Y2         int `json:"y2"`
	DurationMS int `json:"duration_ms,omitempty"`
}

type inputLongPressIn struct {
	X          int `json:"x"`
	Y          int `json:"y"`
	DurationMS int `json:"duration_ms,omitempty"`
}

type inputKeyIn struct {
	Name string `json:"name"`
}

type inputTextIn struct {
	Text string `json:"text"`
}

// inputResult is the Shape A wrapper: bare JSON in the text block,
// {"result": "<same text>"} as structuredContent, isError never set. server.py's
// _ok() appends ok:true only when absent, which is what OK reproduces.
func inputResult(payload *jsonresult.Obj, err error) (*mcp.CallToolResult, StringOut, error) {
	if err != nil {
		return JSONFail(err)
	}
	return JSONPayload(OK(payload))
}

// inputTapOpts builds the actions-level options from the two knobs the MCP layer
// exposes. retry_radius_px and settle_s are not exposed and stay at their
// actions defaults.
func inputTapOpts(verify bool, retries int) inputTapOptions {
	opts := inputDefaultTapOptions()
	opts.Verify = verify
	opts.Retries = retries
	return opts
}

func registerPhoneInput(s *mcp.Server, env *mcpserver.Env) error {
	actions := newInputActions(env)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_tap",
		Description: "Tap native device pixels. By default verifies a screen change and retries nearby.",
		InputSchema: ObjectSchema("phone_tapArguments",
			Prop{Name: "x", Type: "integer", Title: "X", Required: true},
			Prop{Name: "y", Type: "integer", Title: "Y", Required: true},
			Prop{Name: "verify", Type: "boolean", Title: "Verify", Default: true},
			Prop{Name: "retries", Type: "integer", Title: "Retries", Default: 2},
		),
		OutputSchema: StringOutputSchema("phone_tap"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputTapIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.tap(ctx, in.X, in.Y, inputTapOpts(in.Verify, in.Retries)))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_tap_relative",
		Description: "Tap normalized screenshot coordinates where x and y are between 0 and 1.",
		InputSchema: ObjectSchema("phone_tap_relativeArguments",
			Prop{Name: "x", Type: "number", Title: "X", Required: true},
			Prop{Name: "y", Type: "number", Title: "Y", Required: true},
			Prop{Name: "verify", Type: "boolean", Title: "Verify", Default: true},
			Prop{Name: "retries", Type: "integer", Title: "Retries", Default: 2},
		),
		OutputSchema: StringOutputSchema("phone_tap_relative"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputTapRelativeIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.tapRelative(ctx, in.X, in.Y, inputTapOpts(in.Verify, in.Retries)))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "phone_tap_image",
		Description: "Tap a point measured on a displayed/resized screenshot.\n\n" +
			"Pass the exact width and height of the image coordinate space used to\n" +
			"choose x/y. The tool maps it into current native device pixels.\n",
		InputSchema: ObjectSchema("phone_tap_imageArguments",
			Prop{Name: "x", Type: "integer", Title: "X", Required: true},
			Prop{Name: "y", Type: "integer", Title: "Y", Required: true},
			Prop{Name: "image_width", Type: "integer", Title: "Image Width", Required: true},
			Prop{Name: "image_height", Type: "integer", Title: "Image Height", Required: true},
			Prop{Name: "verify", Type: "boolean", Title: "Verify", Default: true},
			Prop{Name: "retries", Type: "integer", Title: "Retries", Default: 2},
		),
		OutputSchema: StringOutputSchema("phone_tap_image"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputTapImageIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.tapImage(ctx, in.X, in.Y, in.ImageWidth, in.ImageHeight,
			inputTapOpts(in.Verify, in.Retries)))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_swipe",
		Description: "Swipe from (x1,y1) to (x2,y2).",
		InputSchema: ObjectSchema("phone_swipeArguments",
			Prop{Name: "x1", Type: "integer", Title: "X1", Required: true},
			Prop{Name: "y1", Type: "integer", Title: "Y1", Required: true},
			Prop{Name: "x2", Type: "integer", Title: "X2", Required: true},
			Prop{Name: "y2", Type: "integer", Title: "Y2", Required: true},
			Prop{Name: "duration_ms", Type: "integer", Title: "Duration Ms", Default: 300},
		),
		OutputSchema: StringOutputSchema("phone_swipe"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputSwipeIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.swipe(ctx, in.X1, in.Y1, in.X2, in.Y2, in.DurationMS))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_long_press",
		Description: "Long press at coordinates.",
		InputSchema: ObjectSchema("phone_long_pressArguments",
			Prop{Name: "x", Type: "integer", Title: "X", Required: true},
			Prop{Name: "y", Type: "integer", Title: "Y", Required: true},
			Prop{Name: "duration_ms", Type: "integer", Title: "Duration Ms", Default: 1000},
		),
		OutputSchema: StringOutputSchema("phone_long_press"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputLongPressIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.longPress(ctx, in.X, in.Y, in.DurationMS))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_key",
		Description: "Press a key: back, home, recents, enter, delete, tab, menu, power, paste.",
		InputSchema: ObjectSchema("phone_keyArguments",
			Prop{Name: "name", Type: "string", Title: "Name", Required: true},
		),
		OutputSchema: StringOutputSchema("phone_key"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputKeyIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.key(ctx, in.Name))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_type",
		Description: "Type short ASCII text. Do not use for Chinese — use phone_paste instead.",
		InputSchema: ObjectSchema("phone_typeArguments",
			Prop{Name: "text", Type: "string", Title: "Text", Required: true},
		),
		OutputSchema: StringOutputSchema("phone_type"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputTextIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.typeText(ctx, in.Text))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_paste",
		Description: "Paste text via device clipboard (supports Chinese and emoji).",
		InputSchema: ObjectSchema("phone_pasteArguments",
			Prop{Name: "text", Type: "string", Title: "Text", Required: true},
		),
		OutputSchema: StringOutputSchema("phone_paste"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inputTextIn) (*mcp.CallToolResult, StringOut, error) {
		return inputResult(actions.paste(ctx, in.Text))
	})

	return nil
}
