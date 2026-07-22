package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/scrcpy"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/widget"
)

// The plugin-owned scrcpy stream: start, status, stop — plus the adapter that
// plugs the real runtime into the StreamRuntime seam in widget_runtime.go, so
// every other app tool switches from adb to the scrcpy control socket the moment
// a session comes up.
//
// All three tools are app-only (AppOnlyMeta) and use result Shape C — a
// hand-built structuredContent object plus a human sentence — because the Python
// returned CallToolResult directly and therefore declared no outputSchema.
//
// Only start_stream has an error path. stream_status and stop_stream have no
// try/except in the Python and cannot return isError: a failure is already
// inside the status payload as state:"error" with a non-empty error.

func init() {
	mcpserver.Register(mcpserver.Registration{
		Name:  "scrcpy-stream",
		Order: mcpserver.OrderAppTools + 10,
		Apply: registerStreamTools,
	})
}

func registerStreamTools(s *mcp.Server, env *mcpserver.Env) error {
	// One runtime per process: its loopback port is already published in the
	// widget resource's CSP, so nothing may build a second one.
	runtime := scrcpy.Configure(env.Context(), env.Log)

	// Keep the widget CSP honest when the listener is bound LATE. main binds it
	// before mcpserver.New and passes the port through Options, but a bind that
	// failed at startup and succeeds on the first stream_start would otherwise
	// leave the published connectDomains without the port its own stream is
	// served on — the CSP is resolved at marshal time, but only from whatever
	// SetLoopbackPort was last told.
	scrcpy.SetLoopbackObserver(widget.SetLoopbackPort)

	// Three seams, one runtime. Each tool group declared the slice of the
	// runtime it needs and defaulted to adb until something installed it; this
	// is the single place that does, so all three switch together the moment a
	// stream comes up. Miss one and that group silently keeps taking the adb
	// path forever — phone_backend reporting "adb" mid-stream, phone_tap going
	// through `input tap` instead of the control socket.
	SetStreamRuntime(streamAdapter{runtime: runtime})      // the app-only scrcpymac_ui_* tools
	SetInputRuntime(inputStreamAdapter{runtime: runtime})  // phone_tap/swipe/key/type/paste
	SetDeviceStreamStatus(deviceStreamStatusFrom(runtime)) // phone_backend, phone_device_info

	// The single guarantee that nothing outlives the process: the scrcpy
	// app_process on the device, its adb forward, the loopback listener and
	// every relay goroutine are released here, however the session ends.
	env.OnShutdown("scrcpy-runtime", runtime.Close)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_start_stream",
		Title:       "Start standalone ScrcpyMac stream",
		Description: "Start the plugin-owned scrcpy H.264 and control session.",
		Annotations: Annotations(false, false, false, false),
		Meta:        AppOnlyMeta,
		InputSchema: ObjectSchema("scrcpymac_ui_start_streamArguments",
			Prop{Name: "serial", Type: "string", Title: "Serial", Required: true},
			Prop{Name: "max_fps", Type: "integer", Title: "Max Fps", Default: 60},
			Prop{Name: "resolution_percent", Type: "integer", Title: "Resolution Percent", Default: 50},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in streamStartIn) (*mcp.CallToolResult, any, error) {
		// The Python selects the device first, so a bad serial fails with
		// "Android device not found: ..." before scrcpy-server is pushed.
		if _, err := widgetSelectDevice(ctx, env, in.Serial); err != nil {
			return StructuredError(err)
		}
		payload, err := runtime.Start(ctx, in.Serial, in.MaxFPS, in.ResolutionPercent)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, "Started the standalone ScrcpyMac H.264 stream.")
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_stream_status",
		Title:       "Read standalone ScrcpyMac stream status",
		Description: "Read the plugin-owned H.264 stream state and measured FPS.",
		Annotations: Annotations(true, false, true, false),
		Meta:        AppOnlyMeta,
		InputSchema: ObjectSchema("scrcpymac_ui_stream_statusArguments"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ streamNoArgs) (*mcp.CallToolResult, any, error) {
		return Structured(runtime.Status(), "Read the standalone ScrcpyMac stream status.")
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_stop_stream",
		Title:       "Stop standalone ScrcpyMac stream",
		Description: "Stop the plugin-owned scrcpy session and release its adb forward.",
		Annotations: Annotations(false, false, true, false),
		Meta:        AppOnlyMeta,
		InputSchema: ObjectSchema("scrcpymac_ui_stop_streamArguments"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ streamNoArgs) (*mcp.CallToolResult, any, error) {
		return Structured(runtime.Stop(), "Stopped the standalone ScrcpyMac stream.")
	})

	return nil
}

// streamStartIn mirrors scrcpymac_ui_start_stream's parameters. The defaults
// come from the hand-written InputSchema, which the SDK applies before the
// handler runs; the runtime re-snaps both values anyway.
type streamStartIn struct {
	Serial            string `json:"serial"`
	MaxFPS            int    `json:"max_fps,omitempty"`
	ResolutionPercent int    `json:"resolution_percent,omitempty"`
}

// streamNoArgs is the argument type of the two no-parameter tools.
type streamNoArgs struct{}

// inputStreamAdapter bridges *scrcpy.Runtime to the InputRuntime interface the
// model-visible input tools depend on.
//
// It is a second adapter rather than more methods on streamAdapter because both
// interfaces declare Status() with different return types — one Go type cannot
// satisfy both.
type inputStreamAdapter struct{ runtime *scrcpy.Runtime }

var _ InputRuntime = inputStreamAdapter{}

// Status mirrors actions.py's read of runtime.status(): the state string decides
// whether the streaming path is taken, and the sizes are NATIVE device pixels
// (deviceWidth/deviceHeight), not the encoded frame size.
func (a inputStreamAdapter) Status() InputRuntimeStatus {
	d := a.runtime.Diagnostics()
	return InputRuntimeStatus{
		Streaming:    d.State == scrcpy.StateStreaming,
		DeviceWidth:  d.DeviceWidth,
		DeviceHeight: d.DeviceHeight,
	}
}

// IsActive is state == "streaming" AND metadata present, which is what key() and
// paste() branch on. Metadata is assigned under the same lock hold that sets the
// state, so in practice this never disagrees with Status().Streaming.
func (a inputStreamAdapter) IsActive() bool { return a.runtime.IsActive("") }

func (a inputStreamAdapter) TapRelative(_ context.Context, x, y float64) (*jsonresult.Obj, error) {
	return a.runtime.TapRelative(x, y)
}

func (a inputStreamAdapter) SwipeRelative(ctx context.Context, x1, y1, x2, y2 float64, durationMS int) (*jsonresult.Obj, error) {
	return a.runtime.SwipeRelative(ctx, x1, y1, x2, y2, durationMS)
}

func (a inputStreamAdapter) Key(_ context.Context, name string) (*jsonresult.Obj, error) {
	return a.runtime.Key(name)
}

func (a inputStreamAdapter) Paste(_ context.Context, text string) (*jsonresult.Obj, error) {
	return a.runtime.Paste(text)
}

// deviceStreamStatusFrom is what phone_backend and phone_device_info read.
//
// The false result is "not streaming", i.e. take the adb path — the Python's
// `if runtime.status()["state"] == "streaming"`. Requiring metadata as well is
// not a stricter test in practice (it is set under the same lock hold) but it
// does guarantee the fields below are populated, where the Python would have
// raised KeyError.
func deviceStreamStatusFrom(runtime *scrcpy.Runtime) DeviceStreamStatusFunc {
	return func() (DeviceStreamStatus, bool) {
		if !runtime.IsActive("") {
			return DeviceStreamStatus{}, false
		}
		d := runtime.Diagnostics()
		return DeviceStreamStatus{
			Serial:       d.Serial,
			DeviceWidth:  d.DeviceWidth,
			DeviceHeight: d.DeviceHeight,
			FrameWidth:   d.FrameWidth,
			FrameHeight:  d.FrameHeight,
			// status()["fps"] is round(rate, 1) — a Python float that must
			// serialise as "0.0", never "0".
			FPS:   jsonresult.PyRound(d.FramesPerSecond, 1),
			Codec: d.Codec,
		}, true
	}
}

// streamAdapter bridges *scrcpy.Runtime to the StreamRuntime interface the
// widget tools depend on.
//
// It is a thin mapping and deliberately so: the interface is written in terms of
// what the tool payloads need (context-taking methods, jsonresult.Obj results),
// while the runtime is written in terms of the scrcpy session. Only IsActive and
// Diagnostics do real work here.
type streamAdapter struct{ runtime *scrcpy.Runtime }

var _ StreamRuntime = streamAdapter{}

func (a streamAdapter) BackendName() string { return a.runtime.BackendName() }

// IsActive collapses the runtime's per-serial check: the widget tools only ever
// ask "is the plugin driving some device right now".
func (a streamAdapter) IsActive() bool { return a.runtime.IsActive("") }

func (a streamAdapter) Status() *jsonresult.Obj { return a.runtime.Status() }

func (a streamAdapter) TapRelative(_ context.Context, x, y float64) (*jsonresult.Obj, error) {
	return a.runtime.TapRelative(x, y)
}

func (a streamAdapter) SwipeRelative(ctx context.Context, x1, y1, x2, y2 float64, durationMs int) (*jsonresult.Obj, error) {
	return a.runtime.SwipeRelative(ctx, x1, y1, x2, y2, durationMs)
}

func (a streamAdapter) Key(_ context.Context, name string) (*jsonresult.Obj, error) {
	return a.runtime.Key(name)
}

func (a streamAdapter) Paste(_ context.Context, text string) (*jsonresult.Obj, error) {
	return a.runtime.Paste(text)
}

func (a streamAdapter) Diagnostics() StreamDiagnostics {
	d := a.runtime.Diagnostics()
	out := StreamDiagnostics{
		State:            d.State,
		Backend:          d.Backend,
		Serial:           d.Serial,
		Codec:            d.Codec,
		DeviceWidth:      d.DeviceWidth,
		DeviceHeight:     d.DeviceHeight,
		FrameWidth:       d.FrameWidth,
		FrameHeight:      d.FrameHeight,
		UptimeS:          d.UptimeS,
		Packets:          d.Packets,
		Frames:           d.Frames,
		KeyFrames:        d.KeyFrames,
		Bytes:            d.Bytes,
		PacketsPerSecond: d.PacketsPerSecond,
		FramesPerSecond:  d.FramesPerSecond,
		DroppedGOPs:      d.DroppedGOPs,
		DroppedPackets:   d.DroppedPackets,
		LastError:        d.LastError,
		Clients:          make([]StreamClientDiagnostics, 0, len(d.Clients)),
	}
	for _, client := range d.Clients {
		out.Clients = append(out.Clients, StreamClientDiagnostics{
			ID:                 client.ID,
			ConnectedS:         client.ConnectedS,
			QueueDepth:         client.QueueDepth,
			QueueCapacity:      client.QueueCapacity,
			PacketsSent:        client.PacketsSent,
			BytesSent:          client.BytesSent,
			DroppedGOPs:        client.DroppedGOPs,
			DroppedPackets:     client.DroppedPackets,
			WaitingForKeyFrame: client.WaitingForKeyFrame,
		})
	}
	return out
}
