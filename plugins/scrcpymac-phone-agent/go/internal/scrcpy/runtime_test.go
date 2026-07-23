package scrcpy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
)

// newTestRuntime returns a runtime that never touches adb and is always closed
// when the test ends, so no test can leave a listener or a goroutine behind.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	r := New(Options{
		NewADB: func(string) (*adb.Client, error) {
			return nil, &Error{Msg: "adb is unavailable in tests"}
		},
	})
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// fakeConn is a buffered in-memory net.Conn: writes are captured rather than
// blocking, which is what a control-socket assertion needs.
type fakeConn struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.buf.Write(p)
}

func (c *fakeConn) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *fakeConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// fakeStreaming puts a runtime into the streaming state with a fake control
// socket, so the control and status paths can be exercised without a device.
func fakeStreaming(t *testing.T, r *Runtime, meta *VideoMeta, token string) *fakeConn {
	t.Helper()
	control := &fakeConn{}
	r.mu.Lock()
	r.sess = &session{serial: meta.Serial, control: control, exited: make(chan struct{})}
	r.meta = meta
	r.state = StateStreaming
	r.errMsg = ""
	r.token = token
	r.streamID++
	r.startedAt = time.Now()
	r.mu.Unlock()
	return control
}

func TestIdleStatusPayload(t *testing.T) {
	r := newTestRuntime(t)
	got := jsonresult.Text(r.Status())

	const want = `{
  "ok": true,
  "state": "idle",
  "backend": "adb",
  "encoding": "JPEG",
  "error": "",
  "fps": 0.0,
  "frames": 0
}`
	if got != want {
		t.Fatalf("idle status:\n%s\nwant:\n%s", got, want)
	}
}

func TestStreamingStatusPayload(t *testing.T) {
	r := newTestRuntime(t)
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	fakeStreaming(t, r, &VideoMeta{
		Serial: "2f019965", DeviceName: "OnePlus6", CodecID: H264CodecID,
		Width: 540, Height: 1140, NativeWidth: 1080, NativeHeight: 2280,
		MaxFPS: 60, ResolutionPercent: 50,
	}, "tok123")

	status := r.Status()
	wantKeys := []string{
		"ok", "state", "backend", "encoding", "error", "fps", "frames",
		"serial", "deviceName", "deviceWidth", "deviceHeight",
		"frameWidth", "frameHeight", "maxFps", "resolutionPercent", "codec",
		"streamUrl",
	}
	got := status.Keys()
	if len(got) != len(wantKeys) {
		t.Fatalf("status keys = %v, want %v", got, wantKeys)
	}
	for i := range wantKeys {
		if got[i] != wantKeys[i] {
			t.Fatalf("status key %d = %q, want %q (order is contract)", i, got[i], wantKeys[i])
		}
	}

	url, _ := status.Get("streamUrl")
	if want := fmt.Sprintf("ws://127.0.0.1:%d/stream?token=tok123", port); url != want {
		t.Fatalf("streamUrl = %v, want %v", url, want)
	}
	if backend, _ := status.Get("backend"); backend != "plugin-h264" {
		t.Fatalf("backend = %v", backend)
	}
	if encoding, _ := status.Get("encoding"); encoding != "H.264" {
		t.Fatalf("encoding = %v", encoding)
	}
	if !strings.Contains(jsonresult.Text(status), `"fps": 0.0`) {
		t.Error(`fps must serialise as a Python float ("0.0", never "0")`)
	}
}

func TestPullPacketsReturnsConcatenatedApplicationPackets(t *testing.T) {
	r := newTestRuntime(t)
	fakeStreaming(t, r, &VideoMeta{
		Serial: "S", Width: 540, Height: 1140, NativeWidth: 1080, NativeHeight: 2280,
	}, "tok")
	r.hub.Broadcast(makePacket(0, kindConfig, 32))
	r.hub.Broadcast(makePacket(1, kindKey, 64))
	r.hub.Broadcast(makePacket(2, kindDelta, 64))

	batch := r.PullPackets(context.Background(), 1<<20, 100)
	if batch.PacketCount != 3 {
		t.Fatalf("PacketCount = %d, want 3", batch.PacketCount)
	}
	if batch.DroppedGOPs != 0 || batch.DroppedPackets != 0 {
		t.Fatalf("unexpected drops: %d GOPs / %d packets", batch.DroppedGOPs, batch.DroppedPackets)
	}

	offset := 0
	decoded := 0
	for offset < len(batch.Data) {
		if len(batch.Data)-offset < WSHeaderLength {
			t.Fatalf("truncated packet at offset %d", offset)
		}
		length := int(binary.BigEndian.Uint32(batch.Data[offset+10 : offset+WSHeaderLength]))
		end := offset + WSHeaderLength + length
		if end > len(batch.Data) {
			t.Fatalf("packet at offset %d ends past batch", offset)
		}
		if _, _, _, _, err := DecodeStreamPacket(batch.Data[offset:end]); err != nil {
			t.Fatalf("packet %d: %v", decoded, err)
		}
		decoded++
		offset = end
	}
	if decoded != batch.PacketCount {
		t.Fatalf("decoded %d packets, summary reports %d", decoded, batch.PacketCount)
	}
}

// A clean stop clears the video metadata; the device keys disappear with it.
func TestStopClearsMetadataAndKeepsFrameCount(t *testing.T) {
	r := newTestRuntime(t)
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 10, Height: 20}, "tok")
	r.mu.Lock()
	r.frameCount = 42
	r.mu.Unlock()

	status := r.Stop()
	if state, _ := status.Get("state"); state != StateIdle {
		t.Fatalf("state after stop = %v, want idle", state)
	}
	if status.Has("serial") || status.Has("streamUrl") {
		t.Fatalf("a clean stop must drop the device keys: %v", status.Keys())
	}
	// _frame_count is deliberately not reset by stop, only by start.
	if frames, _ := status.Get("frames"); frames != 42 {
		t.Fatalf("frames = %v, want 42 (stop must not reset the counter)", frames)
	}
	if r.AcceptsStreamToken("tok") {
		t.Error("the stream token must be invalidated by stop")
	}
}

func TestControlMethodsRequireARunningStream(t *testing.T) {
	r := newTestRuntime(t)
	const want = "plugin scrcpy stream is not running"

	if _, err := r.TapRelative(0.5, 0.5); err == nil || err.Error() != want {
		t.Errorf("TapRelative error = %v, want %q", err, want)
	}
	if _, err := r.SwipeRelative(context.Background(), 0, 0, 1, 1, 100); err == nil || err.Error() != want {
		t.Errorf("SwipeRelative error = %v, want %q", err, want)
	}
	if _, err := r.Key("back"); err == nil || err.Error() != want {
		t.Errorf("Key error = %v, want %q", err, want)
	}
	if _, err := r.Paste("hi"); err == nil || err.Error() != want {
		t.Errorf("Paste error = %v, want %q", err, want)
	}
}

func TestTapRelativeSendsTheRightBytes(t *testing.T) {
	r := newTestRuntime(t)
	meta := &VideoMeta{Serial: "2f019965", Width: 540, Height: 1140}
	control := fakeStreaming(t, r, meta, "tok")

	payload, err := r.TapRelative(0.5, 0.5)
	if err != nil {
		t.Fatalf("TapRelative: %v", err)
	}

	// Python's round() is banker's: round(0.5 * 539) is 270, round(0.5 * 1139)
	// is 570 (both exact halves rounding to even would differ from math.Round).
	wantDown := touchMessage(touchActionDown, 270, 570, 540, 1140, true)
	wantUp := touchMessage(touchActionUp, 270, 570, 540, 1140, false)
	if got := hex.EncodeToString(control.bytes()); got != hex.EncodeToString(append(wantDown, wantUp...)) {
		t.Fatalf("control bytes = %s", got)
	}

	wantKeys := []string{"ok", "action", "serial", "point", "coordinateSpace", "backend"}
	if got := payload.Keys(); !equalStrings(got, wantKeys) {
		t.Fatalf("tap payload keys = %v, want %v", got, wantKeys)
	}
	if backend, _ := payload.Get("backend"); backend != "plugin-control" {
		t.Fatalf("backend = %v", backend)
	}
}

func TestTapRelativeClampsCoordinates(t *testing.T) {
	r := newTestRuntime(t)
	meta := &VideoMeta{Serial: "S", Width: 100, Height: 200}
	control := fakeStreaming(t, r, meta, "tok")

	if _, err := r.TapRelative(-5, 42); err != nil {
		t.Fatalf("TapRelative: %v", err)
	}
	want := hex.EncodeToString(append(
		touchMessage(touchActionDown, 0, 199, 100, 200, true),
		touchMessage(touchActionUp, 0, 199, 100, 200, false)...))
	if got := hex.EncodeToString(control.bytes()); got != want {
		t.Fatalf("out-of-range coordinates were not clamped:\n got %s\nwant %s", got, want)
	}
}

func TestSwipeRelativePayloadAndSteps(t *testing.T) {
	r := newTestRuntime(t)
	meta := &VideoMeta{Serial: "S", Width: 100, Height: 200}
	control := fakeStreaming(t, r, meta, "tok")

	payload, err := r.SwipeRelative(context.Background(), 0, 0, 1, 1, 64)
	if err != nil {
		t.Fatalf("SwipeRelative: %v", err)
	}

	// round(64/16) == 4 steps: one down, three moves, one up.
	const touchBytes = 32
	if got := len(control.bytes()); got != 5*touchBytes {
		t.Fatalf("wrote %d bytes, want %d (down + 3 moves + up)", got, 5*touchBytes)
	}

	wantKeys := []string{"ok", "action", "serial", "from", "to", "durationMs", "backend"}
	if got := payload.Keys(); !equalStrings(got, wantKeys) {
		t.Fatalf("swipe payload keys = %v, want %v", got, wantKeys)
	}
	if duration, _ := payload.Get("durationMs"); duration != 64 {
		t.Fatalf("durationMs = %v", duration)
	}
}

func TestSwipeRelativeIsCancellable(t *testing.T) {
	r := newTestRuntime(t)
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 100, Height: 200}, "tok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := r.SwipeRelative(ctx, 0, 0, 1, 1, 10_000); err == nil {
		t.Fatal("a cancelled swipe must return an error rather than run for ten seconds")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a cancelled swipe took %v", elapsed)
	}
}

func TestKeyAndPastePayloads(t *testing.T) {
	r := newTestRuntime(t)
	control := fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 10, Height: 20}, "tok")

	payload, err := r.Key(" BACK ")
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if key, _ := payload.Get("key"); key != "back" {
		t.Fatalf("key = %v, want the normalised name", key)
	}
	if got := hex.EncodeToString(control.bytes()); got != hex.EncodeToString(
		append(keyMessage(keyActionDown, 4), keyMessage(keyActionUp, 4)...)) {
		t.Fatalf("key bytes = %s", got)
	}

	pastePayload, err := r.Paste("你好 world")
	if err != nil {
		t.Fatalf("Paste: %v", err)
	}
	// Python's len() is a rune count: "你好 world" is 8 runes, 12 bytes.
	if length, _ := pastePayload.Get("length"); length != 8 {
		t.Fatalf("paste length = %v, want 8 runes", length)
	}
}

func TestKeyRejectsUnknownNames(t *testing.T) {
	r := newTestRuntime(t)
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 10, Height: 20}, "tok")

	_, err := r.Key("foo")
	const want = "Unknown key 'foo'. Supported: back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down, paste"
	if err == nil || err.Error() != want {
		t.Fatalf("Key error = %v,\nwant %q", err, want)
	}
}

func TestPasteRejectsEmptyText(t *testing.T) {
	r := newTestRuntime(t)
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 10, Height: 20}, "tok")
	if _, err := r.Paste(""); err == nil || err.Error() != "text must not be empty" {
		t.Fatalf("Paste(\"\") error = %v", err)
	}
}

func TestStartRejectsAnEmptySerial(t *testing.T) {
	r := newTestRuntime(t)
	_, err := r.Start(context.Background(), "   ", 60, 50)
	if err == nil || err.Error() != "device serial must not be empty" {
		t.Fatalf("Start error = %v", err)
	}
	if state, _ := r.Status().Get("state"); state != StateIdle {
		t.Fatalf("a rejected start must not change the state, got %v", state)
	}
}

// A failed start must leave the runtime in the error state with the failure
// message visible, and must not leave a token behind.
func TestStartFailureIsRecorded(t *testing.T) {
	r := newTestRuntime(t)
	_, err := r.Start(context.Background(), "nosuchdevice", 60, 50)
	if err == nil {
		t.Fatal("Start must fail when no adb client can be built")
	}
	status := r.Status()
	if state, _ := status.Get("state"); state != StateError {
		t.Fatalf("state = %v, want error", state)
	}
	if ok, _ := status.Get("ok"); ok != false {
		t.Fatalf("ok = %v, want false in the error state", ok)
	}
	if msg, _ := status.Get("error"); msg != "adb is unavailable in tests" {
		t.Fatalf("error = %v", msg)
	}
	if r.AcceptsStreamToken("anything") {
		t.Error("a failed start must not leave a usable token")
	}
}

func TestEnsureLoopbackIsIdempotent(t *testing.T) {
	r := newTestRuntime(t)
	first, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	second, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback (second): %v", err)
	}
	if first != second || first == 0 {
		t.Fatalf("the loopback port must be bound once and stay stable: %d then %d", first, second)
	}
	if got := r.LoopbackPort(); got != first {
		t.Fatalf("LoopbackPort = %d, want %d", got, first)
	}
}

// Shutdown has to be airtight: no listener, no goroutines, nothing that outlives
// the process.
func TestCloseLeavesNothingBehind(t *testing.T) {
	before := goroutineCount()

	r := New(Options{NewADB: func(string) (*adb.Client, error) {
		return nil, &Error{Msg: "unused"}
	}})
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 10, Height: 20}, "tok")

	// Attach a client and push traffic through it.
	conn, resp, br := dialStream(t, port, "/stream?token=tok", nil)
	if resp.StatusCode != 101 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	readServerFrame(t, br) // ready
	r.hub.Broadcast(makePacket(0, kindKey, 64))
	readServerFrame(t, br)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	_ = conn.Close()

	// The listener is gone.
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the loopback listener is still accepting connections after Close")
	}
	// A closed runtime refuses to rebind: its port is published in the widget
	// CSP and must not silently change.
	if _, err := r.EnsureLoopback(); err == nil {
		t.Fatal("a closed runtime must not bind a new listener")
	}

	assertNoGoroutineLeak(t, before)
}

func TestFPSWindow(t *testing.T) {
	var w fpsWindow
	if got := w.rate(); got != 0 {
		t.Fatalf("an empty window must report 0, got %v", got)
	}

	base := time.Now()
	w.add(base)
	if got := w.rate(); got != 0 {
		t.Fatalf("a single sample must report 0 (Python needs two), got %v", got)
	}

	w.reset()
	for i := 0; i < 61; i++ {
		w.add(base.Add(time.Duration(i) * time.Second / 60))
	}
	// 61 samples spanning one second: (61-1)/1.0 == 60.
	if got := jsonresult.PyRound(w.rate(), 1); got != 60 {
		t.Fatalf("rate = %v, want 60", got)
	}

	// The window is bounded at 180 samples, like deque(maxlen=180).
	w.reset()
	for i := 0; i < 500; i++ {
		w.add(base.Add(time.Duration(i) * time.Millisecond))
	}
	if got := w.len(); got != fpsWindowSize {
		t.Fatalf("window length = %d, want %d", got, fpsWindowSize)
	}
	if got := jsonresult.PyRound(w.rate(), 1); got != 1000 {
		t.Fatalf("rate over the trailing window = %v, want 1000", got)
	}
}

func TestScaledMaxSize(t *testing.T) {
	for _, tt := range []struct {
		w, h, percent, want int
	}{
		{1080, 2280, 50, 1140},
		{1080, 2280, 100, 2280},
		{1080, 2280, 25, 570},
		{320, 480, 25, 320},    // floored
		{2280, 1080, 50, 1140}, // landscape: the long edge wins
	} {
		if got := scaledMaxSize(tt.w, tt.h, tt.percent); got != tt.want {
			t.Errorf("scaledMaxSize(%d, %d, %d) = %d, want %d", tt.w, tt.h, tt.percent, got, tt.want)
		}
	}
}

func TestSpawnArgs(t *testing.T) {
	args := spawnArgs(0x0abcdef1, 1140, 60)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"shell", "CLASSPATH=" + jarDevicePath, "app_process", "/",
		"com.genymobile.scrcpy.Server", "scid=0abcdef1", "log_level=info",
		"tunnel_forward=true", "video=true", "audio=false", "control=true",
		"video_codec=h264", "max_size=1140", "max_fps=60",
		"video_bit_rate=4000000", "clipboard_autosync=false", "cleanup=false",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spawn args are missing %q: %s", want, joined)
		}
	}
	// The version is the first positional argument and must match the jar.
	if args[5] != "3.3.4" {
		t.Errorf("scrcpy version argument = %q, want 3.3.4", args[5])
	}
}

func TestDecodeDeviceName(t *testing.T) {
	raw := make([]byte, DeviceNameLength)
	copy(raw, "OnePlus6")
	if got := decodeDeviceName(raw, "serial"); got != "OnePlus6" {
		t.Errorf("decodeDeviceName = %q", got)
	}
	if got := decodeDeviceName(make([]byte, DeviceNameLength), "serial"); got != "serial" {
		t.Errorf("an empty name must fall back to the serial, got %q", got)
	}
}

func TestRandomTokenShape(t *testing.T) {
	token, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	// secrets.token_urlsafe(32) yields 43 unpadded base64url characters.
	if len(token) != 43 || strings.ContainsAny(token, "=+/") {
		t.Fatalf("token = %q (%d chars), want 43 unpadded base64url characters", token, len(token))
	}
	other, err := randomToken()
	if err != nil || other == token {
		t.Fatal("tokens must be unique")
	}
}

func TestDiagnosticsSnapshot(t *testing.T) {
	r := newTestRuntime(t)
	fakeStreaming(t, r, &VideoMeta{
		Serial: "S", Width: 100, Height: 200, NativeWidth: 200, NativeHeight: 400,
	}, "tok")
	r.mu.Lock()
	r.packetCount, r.frameCount, r.keyFrameCount, r.byteCount = 10, 9, 2, 4096
	r.mu.Unlock()

	d := r.Diagnostics()
	if d.State != StateStreaming || d.Backend != "plugin-h264" || d.Serial != "S" {
		t.Fatalf("diagnostics = %+v", d)
	}
	if d.Packets != 10 || d.Frames != 9 || d.KeyFrames != 2 || d.Bytes != 4096 {
		t.Fatalf("counters = %+v", d)
	}
	if d.Clients == nil {
		t.Fatal("Clients must be an empty slice, never nil: it has to serialise as []")
	}
	if d.FrameWidth != 100 || d.DeviceHeight != 400 {
		t.Fatalf("geometry = %+v", d)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func goroutineCount() int {
	runtime.GC()
	return runtime.NumGoroutine()
}

// assertNoGoroutineLeak allows the runtime a moment to reap goroutines before
// declaring a leak; a strict equality check right after Close is flaky by
// construction.
func assertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = goroutineCount()
		if after <= before+1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	buf = buf[:runtime.Stack(buf, true)]
	t.Fatalf("goroutines grew from %d to %d after Close:\n%s", before, after, buf)
}
