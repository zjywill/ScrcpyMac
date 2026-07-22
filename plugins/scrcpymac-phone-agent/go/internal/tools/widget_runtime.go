package tools

// The seam between the widget/app tool surface and the plugin's scrcpy H.264
// runtime.
//
// Every app tool that mutates the device branches on whether the plugin owns a
// live scrcpy session: while it does, input goes down the scrcpy control socket
// (fast, no adb round trip) and the payloads use camelCase keys plus
// backend:"plugin-control"; while it does not, input goes through
// `adb shell input ...` and the payloads use the older snake_case keys. Both
// shapes are contract — see docs/mcp-contract.md §3.9-3.12.
//
// The runtime itself lives elsewhere. This file declares only what the tools
// need from it, so the two can be written independently and so every tool here
// is unit-testable against a fake with no device, no adb and no scrcpy-server.
//
// # For whoever implements the runtime
//
// Implement StreamRuntime on an adapter in your own file in this package and
// call SetStreamRuntime(rt) once, from your registrar (Apply). Returning
// *jsonresult.Obj rather than a struct is deliberate: JSON key order is contract
// and Go maps sort alphabetically, so the payload has to be built ordered at the
// source. The exact key order each method must produce is documented on it.
//
// Until SetStreamRuntime is called, an idle runtime is in force: IsActive is
// false, so every tool takes its adb path and the whole surface works without a
// stream. That is also what the Python does before the first
// scrcpymac_ui_start_stream.

import (
	"context"
	"errors"
	"sync"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
)

// ErrStreamNotRunning is what a control call returns when no scrcpy session is
// live. The message is model-visible through {"ok": false, "error": ...} and is
// byte-identical to ScrcpyRuntimeError's in scrcpy_runtime._require_control_meta.
var ErrStreamNotRunning = errors.New("plugin scrcpy stream is not running")

// StreamRuntime is the subset of the scrcpy runtime the widget tools use.
//
// All methods must be safe for concurrent use: MCP tool calls are served
// concurrently and the widget fires taps while snapshots are in flight.
type StreamRuntime interface {
	// BackendName is "plugin-h264" while streaming, "adb" otherwise.
	BackendName() string

	// IsActive reports whether a session is streaming AND its control socket is
	// usable. It is the branch every input tool takes.
	IsActive() bool

	// Status is ScrcpyRuntime.status(): the seven base keys in order —
	// ok, state, backend, encoding, error, fps, frames — then, only while
	// session metadata exists, serial, deviceName, deviceWidth, deviceHeight,
	// frameWidth, frameHeight, maxFps, resolutionPercent, codec, and finally
	// streamUrl only when state=="streaming" with a bound loopback and a token.
	//
	// fps must be a jsonresult.Float (Python's round(x, 1) renders 0.0, not 0).
	Status() *jsonresult.Obj

	// TapRelative taps normalised coordinates, clamping both to [0,1].
	// Keys, in order: ok, action, serial, point, coordinateSpace, backend.
	TapRelative(ctx context.Context, x, y float64) (*jsonresult.Obj, error)

	// SwipeRelative drags between normalised coordinates over durationMs, which
	// the caller has already clamped to [0,10000].
	// Keys, in order: ok, action, serial, from, to, durationMs, backend.
	SwipeRelative(ctx context.Context, x1, y1, x2, y2 float64, durationMs int) (*jsonresult.Obj, error)

	// Key presses a named key. The runtime's own table has ten entries and
	// deliberately omits "paste" — scrcpymac_ui_key("paste") therefore works over
	// adb and fails while streaming, which is existing behaviour.
	// Keys, in order: ok, action, key, serial, backend.
	Key(ctx context.Context, name string) (*jsonresult.Obj, error)

	// Paste sets the device clipboard and pastes. length is a rune count.
	// Keys, in order: ok, action, length, serial, backend.
	Paste(ctx context.Context, text string) (*jsonresult.Obj, error)

	// Diagnostics is the server-side view of the relay, for phone_stream_status.
	// It must never block on the packet pump or on a client's socket.
	Diagnostics() StreamDiagnostics
}

// StreamDiagnostics is the relay's own account of what it is doing, exposed by
// phone_stream_status.
//
// This exists because the only stream-health tool the Python had was app-only,
// which is precisely why a ~1 FPS regression was so hard to diagnose: the model
// could see that the picture was wrong but had no way to ask whether frames were
// arriving from the device, whether a client's queue was backed up, or whether
// whole GOPs were being dropped. Every field here answers one of those.
type StreamDiagnostics struct {
	// State mirrors Status()["state"]: idle, starting, streaming or error.
	State string
	// Backend is "plugin-h264" while streaming, "adb" otherwise.
	Backend string
	// Serial is the streaming device, "" when idle.
	Serial string
	// Codec is the negotiated codec string, e.g. "avc1.42E01E".
	Codec string
	// DeviceWidth/DeviceHeight are the panel's native pixels.
	DeviceWidth, DeviceHeight int
	// FrameWidth/FrameHeight are the encoded frame size.
	FrameWidth, FrameHeight int

	// UptimeS is seconds since the session started, 0 when idle.
	UptimeS float64

	// Packets, Frames and KeyFrames count what the pump read off the device.
	// Frames excludes config (SPS/PPS) packets; KeyFrames counts IDRs, i.e. the
	// GOP boundaries a lagging client can resync on.
	Packets, Frames, KeyFrames int64
	// Bytes is the total H.264 payload read from the device socket.
	Bytes int64

	// PacketsPerSecond and FramesPerSecond are measured over the pump's recent
	// window — the device's real output rate, independent of any client.
	PacketsPerSecond, FramesPerSecond float64

	// DroppedGOPs and DroppedPackets are totals across all clients. A relay that
	// drops must drop WHOLE GOPs: discarding an isolated delta frame breaks the
	// H.264 reference chain and freezes the picture until the next keyframe.
	DroppedGOPs, DroppedPackets int64

	// LastError is the most recent relay error, "" when healthy.
	LastError string

	// Clients is one entry per attached WebSocket client. Never nil: an empty
	// slice must serialise as [] and not null.
	Clients []StreamClientDiagnostics
}

// StreamClientDiagnostics is one attached widget.
type StreamClientDiagnostics struct {
	// ID identifies the client in logs; any stable short string.
	ID string
	// ConnectedS is seconds since this client attached.
	ConnectedS float64
	// QueueDepth is how many packets are waiting to be written to this client
	// right now, and QueueCapacity the bound at which the relay starts dropping.
	// A depth that sits near capacity is the signature of a slow consumer.
	QueueDepth, QueueCapacity int
	// PacketsSent and BytesSent are what actually reached this client.
	PacketsSent, BytesSent int64
	// DroppedGOPs and DroppedPackets are what this client missed.
	DroppedGOPs, DroppedPackets int64
	// WaitingForKeyFrame is true while the client is being resynced after a drop
	// and is receiving nothing until the next IDR.
	WaitingForKeyFrame bool
}

var (
	streamRuntimeMu sync.RWMutex
	streamRuntime   StreamRuntime = idleStreamRuntime{}
)

// SetStreamRuntime installs the live scrcpy runtime. Passing nil restores the
// idle runtime, which is what tests and a runtime-less build use.
func SetStreamRuntime(rt StreamRuntime) {
	streamRuntimeMu.Lock()
	defer streamRuntimeMu.Unlock()
	if rt == nil {
		streamRuntime = idleStreamRuntime{}
		return
	}
	streamRuntime = rt
}

// ActiveStreamRuntime returns the installed runtime. It never returns nil, so
// callers need no guard.
func ActiveStreamRuntime() StreamRuntime {
	streamRuntimeMu.RLock()
	defer streamRuntimeMu.RUnlock()
	return streamRuntime
}

// idleStreamRuntime is the runtime in force before any scrcpy session exists.
// It reports the same payload ScrcpyRuntime.status() returns from its initial
// state, so scrcpymac_ui_state and phone_stream_status are meaningful from the
// first tool call.
type idleStreamRuntime struct{}

func (idleStreamRuntime) BackendName() string { return "adb" }

func (idleStreamRuntime) IsActive() bool { return false }

func (idleStreamRuntime) Status() *jsonresult.Obj {
	return jsonresult.Of(
		"ok", true,
		"state", "idle",
		"backend", "adb",
		"encoding", "JPEG",
		"error", "",
		"fps", jsonresult.Float(0),
		"frames", 0,
	)
}

func (idleStreamRuntime) TapRelative(context.Context, float64, float64) (*jsonresult.Obj, error) {
	return nil, ErrStreamNotRunning
}

func (idleStreamRuntime) SwipeRelative(context.Context, float64, float64, float64, float64, int) (*jsonresult.Obj, error) {
	return nil, ErrStreamNotRunning
}

func (idleStreamRuntime) Key(context.Context, string) (*jsonresult.Obj, error) {
	return nil, ErrStreamNotRunning
}

func (idleStreamRuntime) Paste(context.Context, string) (*jsonresult.Obj, error) {
	return nil, ErrStreamNotRunning
}

func (idleStreamRuntime) Diagnostics() StreamDiagnostics {
	return StreamDiagnostics{
		State:   "idle",
		Backend: "adb",
		Clients: []StreamClientDiagnostics{},
	}
}

var (
	widgetDeviceChangeMu  sync.Mutex
	widgetDeviceChangeFns []func()
)

// WidgetOnDeviceChange registers a callback fired whenever a widget tool mutates
// device state — a device selection, tap, swipe, key press or paste.
//
// It exists for one reason: PhoneActions kept the ui_tree cache and the input
// methods on the same object, so every mutation dropped the cache
// (actions.py:160, 339, 365, 394). Here the cache and the widget's input tools
// live in different files, so without this hook a widget tap could be followed
// by a phone_ui_tree served from a tree captured before it. Register the cache
// invalidator from whichever file owns the cache.
//
// Callbacks must not block: they run inline on the tool's goroutine.
func WidgetOnDeviceChange(fn func()) {
	if fn == nil {
		return
	}
	widgetDeviceChangeMu.Lock()
	defer widgetDeviceChangeMu.Unlock()
	widgetDeviceChangeFns = append(widgetDeviceChangeFns, fn)
}

// widgetNotifyDeviceChange runs the registered callbacks.
func widgetNotifyDeviceChange() {
	widgetDeviceChangeMu.Lock()
	fns := make([]func(), len(widgetDeviceChangeFns))
	copy(fns, widgetDeviceChangeFns)
	widgetDeviceChangeMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}
