package scrcpy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
)

// fpsWindowSize matches the Python's deque(maxlen=180): three seconds at 60 fps.
const fpsWindowSize = 180

// Options configures a Runtime.
type Options struct {
	// Log receives relay diagnostics. Never point it at stdout: stdout is the
	// MCP transport.
	Log *slog.Logger
	// Context bounds the scrcpy-server process. It should be the server
	// lifetime context so a SIGTERM tears the device-side process down even if
	// Close is somehow skipped.
	Context context.Context
	// NewADB builds the adb client for a stream. Defaults to adb.New, which
	// re-resolves the binary; tests substitute it.
	NewADB func(serial string) (*adb.Client, error)
}

// Runtime owns one plugin-local scrcpy server, stream and control session.
//
// Exactly one instance exists per process (see Default). Every exported method
// is safe for concurrent use: MCP tool calls, the packet pump, the loopback
// listener and shutdown all touch it at once.
type Runtime struct {
	mu sync.Mutex

	log     *slog.Logger
	baseCtx context.Context
	newADB  func(serial string) (*adb.Client, error)

	state    string
	errMsg   string
	streamID int
	token    string
	codec    string
	meta     *VideoMeta
	sess     *session

	// frames counts non-config packets and drives the published fps, exactly
	// like the Python's _frame_count / _frame_times pair. packets counts every
	// packet including the SPS/PPS, and exists only for the diagnostics view —
	// the difference between "the device is producing frames" and "the widget
	// is rendering them" is what makes a stall diagnosable.
	frames        fpsWindow
	packets       fpsWindow
	frameCount    int
	packetCount   int
	keyFrameCount int
	byteCount     int64
	startedAt     time.Time
	clientSeq     uint64

	hub    *Hub
	lb     *loopback
	closed bool

	// controlMu serialises control-socket writes, mirroring the Python's
	// separate _control_send_lock: a swipe holds it for its whole duration and
	// must not also hold the state lock.
	controlMu sync.Mutex

	pumpWG sync.WaitGroup
	connWG sync.WaitGroup
}

// New builds a Runtime. It binds nothing and touches no device.
func New(opts Options) *Runtime {
	r := &Runtime{
		log:     opts.Log,
		baseCtx: opts.Context,
		newADB:  opts.NewADB,
		state:   StateIdle,
		codec:   DefaultCodec,
		hub:     newHub(),
	}
	if r.baseCtx == nil {
		r.baseCtx = context.Background()
	}
	if r.newADB == nil {
		r.newADB = func(serial string) (*adb.Client, error) { return adb.New(serial) }
	}
	return r
}

var (
	defaultOnce sync.Once
	defaultRT   *Runtime
)

// Default returns the process-wide runtime. The MCP tools, the widget resource's
// CSP and the shutdown hook all have to agree on one instance, and the loopback
// port it publishes must stay stable for the process lifetime.
func Default() *Runtime {
	defaultOnce.Do(func() { defaultRT = New(Options{}) })
	return defaultRT
}

// Configure attaches a lifetime context and a logger to the process runtime.
// Both are optional and neither overwrites a value already set, so the order in
// which main and the tool registrar call it does not matter.
func Configure(ctx context.Context, log *slog.Logger) *Runtime {
	r := Default()
	r.mu.Lock()
	if ctx != nil {
		r.baseCtx = ctx
	}
	if log != nil {
		r.log = log
	}
	r.mu.Unlock()
	return r
}

// EnsureDefaultLoopback binds the process runtime's loopback listener and
// returns its port, or 0 if it could not be bound.
//
// It has to run BEFORE the MCP server is constructed: the port is baked into the
// widget resource's CSP connectDomains for the process lifetime, so a listener
// bound afterwards would be one the widget is forbidden to reach. A zero return
// is survivable — the CSP still carries the 127.0.0.1:* wildcards — but the H.264
// path is then dead, so it is logged loudly.
func EnsureDefaultLoopback(log *slog.Logger) int {
	r := Configure(nil, log)
	port, err := r.EnsureLoopback()
	if err != nil {
		r.logger().Warn("could not bind the loopback H.264 listener; the widget will fall back to JPEG", "err", err)
		return 0
	}
	return port
}

func (r *Runtime) logger() *slog.Logger {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	return r.log
}

// loopbackObserver is notified whenever the listener is bound. It exists so the
// widget resource's CSP can name the real port even when the bind happens after
// the MCP server was built — the startup path passes the port to
// mcpserver.Options, but a bind that failed at startup and succeeded on the
// first stream_start would otherwise leave the CSP with wildcards only.
var loopbackObserver atomic.Pointer[func(int)]

// SetLoopbackObserver registers fn, called with the port every time the loopback
// listener is successfully bound. Passing nil clears it. It must not block.
func SetLoopbackObserver(fn func(int)) {
	if fn == nil {
		loopbackObserver.Store(nil)
		return
	}
	loopbackObserver.Store(&fn)
}

func notifyLoopbackBound(port int) {
	if fn := loopbackObserver.Load(); fn != nil && port > 0 {
		(*fn)(port)
	}
}

// EnsureLoopback binds the loopback WebSocket listener if it is not bound yet
// and returns its port. It is idempotent: the listener is bound once and kept.
func (r *Runtime) EnsureLoopback() (int, error) {
	r.mu.Lock()
	switch {
	case r.closed:
		r.mu.Unlock()
		return 0, errorf("plugin loopback stream is unavailable")
	case r.lb != nil:
		port := r.lb.port
		r.mu.Unlock()
		return port, nil
	}
	r.mu.Unlock()

	lb, err := r.startLoopback()
	if err != nil {
		return 0, err
	}

	r.mu.Lock()
	if r.closed || r.lb != nil {
		existing := r.lb
		r.mu.Unlock()
		lb.close()
		if existing != nil {
			return existing.port, nil
		}
		return 0, errorf("plugin loopback stream is unavailable")
	}
	r.lb = lb
	port := lb.port
	r.mu.Unlock()
	notifyLoopbackBound(port)
	return port, nil
}

// LoopbackPort returns the bound loopback port, or 0 when nothing is bound.
func (r *Runtime) LoopbackPort() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lb == nil {
		return 0
	}
	return r.lb.port
}

// BackendName is "plugin-h264" while the stream runs and "adb" otherwise. It is
// what the phone_backend tool and the widget's backend badge report.
func (r *Runtime) BackendName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return backendName(r.state)
}

func backendName(state string) string {
	if state == StateStreaming {
		return "plugin-h264"
	}
	return "adb"
}

// IsActive reports whether the plugin stream is running, optionally for one
// specific serial. An empty serial means "any device".
func (r *Runtime) IsActive(serial string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == StateStreaming && r.meta != nil && (serial == "" || r.meta.Serial == serial)
}

// SelectedSerial returns the serial of the running stream, or "".
func (r *Runtime) SelectedSerial() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.meta == nil {
		return ""
	}
	return r.meta.Serial
}

// AcceptsStreamToken authorises a loopback WebSocket upgrade. The comparison is
// constant time and only ever succeeds while a stream is actually running.
func (r *Runtime) AcceptsStreamToken(token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return token != "" &&
		r.state == StateStreaming &&
		subtle.ConstantTimeCompare([]byte(token), []byte(r.token)) == 1
}

// Status is the frozen stream-status payload.
//
// The seven base keys are always present, in this order. The nine device keys
// appear only while video metadata is known — which, after a clean stop, it is
// not, but after a failed stop it still is. streamUrl appears only when the
// stream is running, the loopback is bound and a token exists.
func (r *Runtime) Status() *jsonresult.Obj {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusLocked()
}

func (r *Runtime) statusLocked() *jsonresult.Obj {
	streaming := r.state == StateStreaming

	encoding := "JPEG"
	if streaming {
		encoding = "H.264"
	}

	payload := jsonresult.Of(
		"ok", r.state != StateError,
		"state", r.state,
		"backend", backendName(r.state),
		"encoding", encoding,
		"error", r.errMsg,
		// A Python float: round(fps, 1) serialises as "0.0", never "0".
		"fps", jsonresult.Float(jsonresult.PyRound(r.frames.rate(), 1)),
		"frames", r.frameCount,
	)

	if meta := r.meta; meta != nil {
		payload.Set("serial", meta.Serial)
		payload.Set("deviceName", meta.DeviceName)
		payload.Set("deviceWidth", meta.NativeWidth)
		payload.Set("deviceHeight", meta.NativeHeight)
		payload.Set("frameWidth", meta.Width)
		payload.Set("frameHeight", meta.Height)
		payload.Set("maxFps", meta.MaxFPS)
		payload.Set("resolutionPercent", meta.ResolutionPercent)
		payload.Set("codec", r.codec)
	}

	if port := r.loopbackPortLocked(); streaming && port != 0 && r.token != "" {
		payload.Set("streamUrl", fmt.Sprintf("ws://127.0.0.1:%d%s?token=%s", port, StreamPath, r.token))
	}
	return payload
}

func (r *Runtime) loopbackPortLocked() int {
	if r.lb == nil {
		return 0
	}
	return r.lb.port
}

// Stats exposes the relay's drop accounting. It is deliberately absent from
// Status: that payload's key set is frozen contract.
func (r *Runtime) Stats() HubStats { return r.hub.Stats() }

// Start brings up a scrcpy session for serial and begins relaying H.264.
//
// ctx bounds the setup adb calls only. The scrcpy-server process is bound to the
// runtime's lifetime context instead, because the MCP request context is
// cancelled the moment this tool call returns.
//
// max_fps snaps to 60 or 30 and resolution_percent clamps to [25, 100], exactly
// as the Python did — those are the only two values the widget ever sends.
func (r *Runtime) Start(ctx context.Context, serial string, maxFPS, resolutionPercent int) (*jsonresult.Obj, error) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return nil, errorf("device serial must not be empty")
	}
	if maxFPS >= 45 {
		maxFPS = 60
	} else {
		maxFPS = 30
	}
	if resolutionPercent < 25 {
		resolutionPercent = 25
	}
	if resolutionPercent > 100 {
		resolutionPercent = 100
	}

	// Any previous session goes first, exactly like the Python: one runtime,
	// one device stream.
	r.Stop()

	token, err := randomToken()
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errorf("plugin scrcpy stream is not running")
	}
	r.state = StateStarting
	r.errMsg = ""
	r.streamID++
	streamID := r.streamID
	r.token = token
	r.codec = DefaultCodec
	r.frames.reset()
	r.packets.reset()
	r.frameCount = 0
	r.packetCount = 0
	r.keyFrameCount = 0
	r.byteCount = 0
	r.startedAt = time.Now()
	procCtx := r.baseCtx
	r.mu.Unlock()

	log := r.logger()

	if _, err := r.EnsureLoopback(); err != nil {
		r.failStart(streamID, err)
		return nil, err
	}

	client, err := r.newADB(serial)
	if err != nil {
		r.failStart(streamID, err)
		return nil, err
	}

	sess, err := startSession(ctx, procCtx, log, client, serial, maxFPS, resolutionPercent)
	if err != nil {
		r.failStart(streamID, err)
		return nil, err
	}

	r.mu.Lock()
	if streamID != r.streamID {
		r.mu.Unlock()
		sess.close(log)
		return nil, errorf("scrcpy start was superseded")
	}
	r.sess = sess
	r.meta = sess.meta
	r.state = StateStreaming
	r.errMsg = ""
	status := r.statusLocked()
	r.mu.Unlock()

	r.pumpWG.Add(1)
	go r.pumpVideo(streamID, sess)

	log.Info("scrcpy stream started",
		"serial", serial,
		"frame", fmt.Sprintf("%dx%d", sess.meta.Width, sess.meta.Height),
		"maxFps", maxFPS,
		"forwardPort", sess.forwardPort,
		"loopbackPort", r.LoopbackPort())
	return status, nil
}

// failStart records a start failure, but only if this attempt is still the
// current one — a concurrent Start or Stop supersedes it.
func (r *Runtime) failStart(streamID int, err error) {
	r.mu.Lock()
	if streamID == r.streamID {
		r.state = StateError
		r.errMsg = err.Error()
		r.token = ""
	}
	r.mu.Unlock()
}

// Stop ends the session and releases everything it holds: clients, sockets, the
// device-side process and the adb forward. The loopback listener stays bound.
//
// It is safe to call when nothing is running, and it always returns the status
// payload the stop_stream tool publishes.
func (r *Runtime) Stop() *jsonresult.Obj {
	r.stopResources(0, "")
	// Safe from anywhere except the pump goroutine itself, which calls
	// stopResources directly for exactly that reason.
	r.pumpWG.Wait()
	return r.Status()
}

// Close is the process-shutdown path: stop the session, drop the listener, wait
// for every goroutine this package started.
func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	lb := r.lb
	r.lb = nil
	r.mu.Unlock()

	r.stopResources(0, "")
	lb.close()
	r.hub.Close()
	r.connWG.Wait()
	r.pumpWG.Wait()
	return nil
}

// stopResources tears the session down.
//
// expectedStreamID > 0 makes the teardown conditional: the packet pump uses it
// so a pump that notices its socket died cannot stop a session that has already
// been replaced. errMsg != "" records a failure, which keeps the video metadata
// visible in the status payload; a clean stop clears it.
func (r *Runtime) stopResources(expectedStreamID int, errMsg string) {
	r.mu.Lock()
	if expectedStreamID > 0 && expectedStreamID != r.streamID {
		r.mu.Unlock()
		return
	}
	// Invalidate the current stream id so a pump still in flight sees that it
	// has been superseded and stops broadcasting.
	r.streamID++
	sess := r.sess
	r.sess = nil
	r.token = ""
	r.frames.reset()
	r.packets.reset()
	if errMsg != "" {
		r.state = StateError
		r.errMsg = errMsg
	} else {
		r.state = StateIdle
		r.errMsg = ""
		r.meta = nil
	}
	log := r.log
	r.mu.Unlock()

	r.hub.CloseClients()
	r.hub.ResetReplay()
	sess.close(log)
}

// pumpVideo reads packets until the socket dies or the session is superseded.
func (r *Runtime) pumpVideo(streamID int, sess *session) {
	defer r.pumpWG.Done()

	err := r.readPackets(streamID, sess)
	if err == nil {
		return
	}

	r.mu.Lock()
	report := streamID == r.streamID && r.state == StateStreaming
	r.mu.Unlock()
	if !report {
		return
	}

	r.hub.BroadcastText(textFrame(jsonresult.Of("type", "error", "error", err.Error())))
	r.stopResources(streamID, fmt.Sprintf("H.264 stream ended: %v", err))
}

// readPackets is the packet pump.
//
// It does exactly three things per packet: read it, frame it once, hand it to
// the hub. Handing it over is a slice append per client under that client's own
// mutex — no socket write, no allocation-heavy work, no logging — so a client
// that stops reading can never slow the device encoder down. That is the whole
// point of the redesign; the Python called sendall() right here.
//
// It returns nil when the session was superseded, which is a normal exit.
func (r *Runtime) readPackets(streamID int, sess *session) error {
	header := make([]byte, FrameHeaderLength)
	for {
		if _, err := io.ReadFull(sess.reader, header); err != nil {
			return readError(err)
		}
		isConfig, isKey, pts, size, err := ParseVideoHeader(header)
		if err != nil {
			return err
		}
		if size > maxPayloadBytes {
			return errorf("scrcpy-server announced an implausible packet of %d bytes", size)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(sess.reader, payload); err != nil {
			return readError(err)
		}

		packet := EncodeStreamPacket(isConfig, isKey, pts, payload)

		var codecFrame []byte
		now := time.Now()
		r.mu.Lock()
		if streamID != r.streamID || r.state != StateStreaming {
			r.mu.Unlock()
			return nil
		}
		r.packetCount++
		r.packets.add(now)
		r.byteCount += int64(len(payload))
		if isConfig {
			r.codec = codecFromConfig(payload)
			codecFrame = textFrame(jsonresult.Of("type", "config", "codec", r.codec))
		} else {
			r.frameCount++
			r.frames.add(now)
			if isKey {
				r.keyFrameCount++
			}
		}
		r.mu.Unlock()

		if codecFrame != nil {
			r.hub.BroadcastText(codecFrame)
		}
		r.hub.Broadcast(packet)
	}
}

// enterHandler admits a loopback HTTP handler and makes Close wait for it.
func (r *Runtime) enterHandler() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.connWG.Add(1)
	return true
}

func (r *Runtime) exitHandler() { r.connWG.Done() }

// serveClient runs one upgraded connection: register, replay, then read until
// the peer goes away. The writer lives on its own goroutine inside Client.
func (r *Runtime) serveClient(conn *wsConn) {
	ready, streamID := r.readyFrame()
	if ready == nil {
		// The stream stopped between the token check and the upgrade.
		_ = conn.close()
		return
	}

	client := newClient(r.nextClientID(), conn, r.logger())
	client.start()
	if !r.hub.Add(client, ready) {
		client.shutdown()
		return
	}
	defer func() {
		r.hub.Remove(client)
		client.shutdown()
	}()

	// Close the window between building the ready message and registering:
	// a stop in between would have run CloseClients before this client existed,
	// leaving it attached to a session that is already gone.
	if !r.stillStreaming(streamID) {
		return
	}

	if err := conn.readLoop(); err != nil {
		r.logger().Debug("loopback client read ended", "err", err)
	}
}

// stillStreaming reports whether streamID is still the live session.
func (r *Runtime) stillStreaming(streamID int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == StateStreaming && r.streamID == streamID
}

// readyFrame builds the {"type":"ready", ...status} message a client receives
// first, along with the stream id it belongs to. It returns nil when the stream
// is no longer running.
func (r *Runtime) readyFrame() ([]byte, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != StateStreaming {
		return nil, 0
	}
	status := r.statusLocked()
	payload := jsonresult.Of("type", "ready")
	for _, key := range status.Keys() {
		value, _ := status.Get(key)
		payload.Set(key, value)
	}
	return textFrame(payload), r.streamID
}

// textFrame renders a payload as a compact JSON WebSocket text frame.
func textFrame(payload *jsonresult.Obj) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil
	}
	return wsFrame(opText, bytes.TrimRight(buf.Bytes(), "\n"))
}

// TapRelative taps normalised coordinates through the control socket.
func (r *Runtime) TapRelative(x, y float64) (*jsonresult.Obj, error) {
	meta, err := r.requireControlMeta()
	if err != nil {
		return nil, err
	}
	px := jsonresult.PyRoundInt(clampUnit(x) * float64(meta.Width-1))
	py := jsonresult.PyRoundInt(clampUnit(y) * float64(meta.Height-1))

	if err := r.sendTouch(touchActionDown, px, py, meta, true); err != nil {
		return nil, err
	}
	if err := r.sendTouch(touchActionUp, px, py, meta, false); err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"action", "tap",
		"serial", meta.Serial,
		"point", []int{px, py},
		"coordinateSpace", []int{meta.Width, meta.Height},
		"backend", "plugin-control",
	), nil
}

// SwipeRelative drags between normalised coordinates, interpolating one move
// event per ~16 ms so Android sees a real gesture rather than a teleport.
//
// ctx aborts the interpolation; the Python had no way to interrupt a ten-second
// swipe, which would have held shutdown open for its full duration.
func (r *Runtime) SwipeRelative(ctx context.Context, x1, y1, x2, y2 float64, durationMS int) (*jsonresult.Obj, error) {
	meta, err := r.requireControlMeta()
	if err != nil {
		return nil, err
	}
	startX := jsonresult.PyRoundInt(clampUnit(x1) * float64(meta.Width-1))
	startY := jsonresult.PyRoundInt(clampUnit(y1) * float64(meta.Height-1))
	endX := jsonresult.PyRoundInt(clampUnit(x2) * float64(meta.Width-1))
	endY := jsonresult.PyRoundInt(clampUnit(y2) * float64(meta.Height-1))

	if durationMS < 0 {
		durationMS = 0
	}
	if durationMS > 10_000 {
		durationMS = 10_000
	}
	steps := jsonresult.PyRoundInt(float64(durationMS) / 16)
	if steps < 1 {
		steps = 1
	}
	if steps > 120 {
		steps = 120
	}

	if err := r.sendTouch(touchActionDown, startX, startY, meta, true); err != nil {
		return nil, err
	}
	started := time.Now()
	for step := 1; step < steps; step++ {
		progress := float64(step) / float64(steps)
		x := jsonresult.PyRoundInt(float64(startX) + float64(endX-startX)*progress)
		y := jsonresult.PyRoundInt(float64(startY) + float64(endY-startY)*progress)
		if err := r.sendTouch(touchActionMove, x, y, meta, true); err != nil {
			return nil, err
		}
		target := started.Add(time.Duration(float64(durationMS) * progress * float64(time.Millisecond)))
		if remaining := time.Until(target); remaining > 0 {
			timer := time.NewTimer(remaining)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				// Release the pointer rather than leaving the device with a
				// finger stuck down.
				_ = r.sendTouch(touchActionUp, x, y, meta, false)
				return nil, errorf("scrcpy swipe was cancelled")
			}
		}
	}
	if err := r.sendTouch(touchActionUp, endX, endY, meta, false); err != nil {
		return nil, err
	}

	return jsonresult.Of(
		"ok", true,
		"action", "swipe",
		"serial", meta.Serial,
		"from", []int{startX, startY},
		"to", []int{endX, endY},
		"durationMs", durationMS,
		"backend", "plugin-control",
	), nil
}

// Key injects a navigation or hardware key.
func (r *Runtime) Key(name string) (*jsonresult.Obj, error) {
	meta, err := r.requireControlMeta()
	if err != nil {
		return nil, err
	}
	normalized := strings.ToLower(strings.TrimSpace(name))
	code, ok := lookupKeycode(name)
	if !ok {
		return nil, errorf("Unknown key %s. Supported: %s", pyRepr(name), keycodeNames())
	}
	if err := r.controlSend(keyMessage(keyActionDown, code)); err != nil {
		return nil, err
	}
	if err := r.controlSend(keyMessage(keyActionUp, code)); err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"action", "key",
		"key", normalized,
		"serial", meta.Serial,
		"backend", "plugin-control",
	), nil
}

// Paste sets the device clipboard and injects a paste in one control message.
func (r *Runtime) Paste(text string) (*jsonresult.Obj, error) {
	meta, err := r.requireControlMeta()
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, errorf("text must not be empty")
	}
	if err := r.controlSend(clipboardMessage(text)); err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"action", "paste",
		// Python's len() counts code points, not bytes.
		"length", jsonresult.RuneLen(text),
		"serial", meta.Serial,
		"backend", "plugin-control",
	), nil
}

// requireControlMeta returns the video metadata, or the exact error the Python
// raised when no plugin stream is up.
func (r *Runtime) requireControlMeta() (*VideoMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != StateStreaming || r.meta == nil || r.sess == nil || r.sess.control == nil {
		return nil, errorf("plugin scrcpy stream is not running")
	}
	return r.meta, nil
}

func (r *Runtime) sendTouch(action byte, x, y int, meta *VideoMeta, buttons bool) error {
	return r.controlSend(touchMessage(action, x, y, meta.Width, meta.Height, buttons))
}

// controlSend writes one control message. Writes are serialised so a swipe's
// interleaved move events cannot be spliced by a concurrent tap.
func (r *Runtime) controlSend(payload []byte) error {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()

	r.mu.Lock()
	var conn net.Conn
	if r.sess != nil {
		conn = r.sess.control
	}
	active := r.state == StateStreaming
	r.mu.Unlock()

	if !active || conn == nil {
		return errorf("plugin scrcpy control socket is not available")
	}
	if _, err := conn.Write(payload); err != nil {
		return errorf("scrcpy control send failed: %v", err)
	}
	return nil
}

// clampUnit constrains a normalised coordinate to [0, 1].
func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// randomToken mints the stream token: 32 random bytes, base64url, unpadded —
// the same 43-character shape as Python's secrets.token_urlsafe(32).
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", errorf("could not generate a stream token: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// fpsWindow is the Python's deque(maxlen=180) of frame arrival times.
type fpsWindow struct {
	times [fpsWindowSize]time.Time
	count int
}

func (w *fpsWindow) add(t time.Time) {
	w.times[w.count%len(w.times)] = t
	w.count++
}

func (w *fpsWindow) reset() { w.count = 0 }

func (w *fpsWindow) len() int {
	if w.count < len(w.times) {
		return w.count
	}
	return len(w.times)
}

// rate is (frames-1) / elapsed over the window, matching the Python exactly —
// including that it needs two samples and reports 0 otherwise.
func (w *fpsWindow) rate() float64 {
	n := w.len()
	if n < 2 {
		return 0
	}
	oldest := w.times[0]
	if w.count > len(w.times) {
		oldest = w.times[w.count%len(w.times)]
	}
	newest := w.times[(w.count-1)%len(w.times)]
	elapsed := newest.Sub(oldest).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(n-1) / elapsed
}
