package tools

// The Codex MCP App surface: open_scrcpymac, the app-only scrcpymac_ui_* tools
// this file owns, and phone_stream_status (opt-in, see StreamDiagnosticsEnv).
//
// Ported from server/phone_agent/mcp_ui.py. Everything here except
// phone_stream_status is Shape C — no outputSchema, a hand-built
// structuredContent object, and a short human sentence as the text block. Get
// that backwards and either Codex's output validation or the widget breaks: the
// widget reads structuredContent first (ui/src/main.ts:315) and only falls back
// to JSON.parse of the text block.
//
// Three things in here are contract and easy to lose:
//
//   - AppOnlyMeta ({"ui": {"visibility": ["app"]}}) is the ONLY thing keeping
//     these tools out of the model's tool list. If it stops round-tripping,
//     Codex sees eight extra tools it was never meant to call.
//   - Every input tool has TWO payload shapes. While the plugin owns a scrcpy
//     session the keys are camelCase with backend:"plugin-control"; otherwise
//     they are the older snake_case adb shapes. Both are documented per tool.
//   - Failures are Shape C errors (isError:true, {"ok":false,"error":msg}) —
//     except scrcpymac_ui_state, which deliberately reports isError:false with a
//     populated payload so the widget can still render the backend state.
//
// The stream tools (start/stop/status) are NOT here: they belong to the file
// that owns the scrcpy runtime. This file consumes that runtime through
// StreamRuntime (widget_runtime.go).

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

func init() {
	mcpserver.Register(
		mcpserver.Registration{
			Name:  "widget-open",
			Order: mcpserver.OrderWidgetTool,
			Apply: registerWidgetOpen,
		},
		mcpserver.Registration{
			Name:  "widget-app",
			Order: mcpserver.OrderAppTools,
			Apply: registerWidgetAppTools,
		},
		mcpserver.Registration{
			Name:  "widget-diagnostics",
			Order: mcpserver.OrderPhoneTools + 90,
			Apply: registerWidgetDiagnostics,
		},
	)
}

// Snapshot clamps, applied before the frame is captured (mcp_ui.py:298).
const (
	widgetSnapshotDefaultMaxWidth = 540
	widgetSnapshotMinMaxWidth     = 320
	widgetSnapshotMaxMaxWidth     = 1200
	widgetSnapshotDefaultQuality  = 60
	widgetSnapshotMinQuality      = 45
	widgetSnapshotMaxQuality      = 90
)

// widgetSwipeMaxDurationMs bounds scrcpymac_ui_swipe's duration_ms.
const widgetSwipeMaxDurationMs = 10_000

// widgetPasteSettle is the pause between setting the clipboard and pressing
// KEYCODE_PASTE on the adb path. Without it the paste fires before the
// clipboard service has the new contents.
const widgetPasteSettle = 150 * time.Millisecond

// Model-visible error strings. Byte-identical to the Python: they reach the
// model verbatim inside {"ok": false, "error": ...}.
const (
	widgetErrEmptySerial       = "device serial must not be empty"
	widgetErrSwipeRange        = "swipe coordinates must be between 0 and 1"
	widgetErrTapRange          = "relative x and y must be between 0 and 1"
	widgetErrEmptyText         = "text must not be empty"
	widgetErrNoScreenSize      = "Could not determine the device screen size"
	widgetDefaultWiFiPortValue = 5555
)

// widgetKeyNames is actions.KEYCODES in Python dict order. The order is not
// decoration: the "Supported: ..." half of the unknown-key error is
// ", ".join(KEYCODES), so it is model-visible text.
//
// This is the ELEVEN-entry table. The scrcpy runtime's own table has ten and
// omits "paste", so scrcpymac_ui_key("paste") succeeds over adb and fails while
// streaming. That asymmetry is existing behaviour and is preserved.
var widgetKeyNames = []string{
	"back", "home", "recents", "enter", "delete",
	"tab", "menu", "power", "volume_up", "volume_down", "paste",
}

var widgetKeycodes = map[string]int{
	"back": 4, "home": 3, "recents": 187, "enter": 66, "delete": 67,
	"tab": 61, "menu": 82, "power": 26, "volume_up": 24, "volume_down": 25,
	"paste": 279,
}

// ---------------------------------------------------------------------------
// Tool inputs
// ---------------------------------------------------------------------------

type widgetOpenIn struct {
	DisplayMode string `json:"display_mode,omitempty"`
}

type widgetNoArgsIn struct{}

type widgetSelectDeviceIn struct {
	Serial string `json:"serial"`
}

type widgetSnapshotIn struct {
	MaxWidth int `json:"max_width,omitempty"`
	Quality  int `json:"quality,omitempty"`
}

type widgetTapIn struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type widgetSwipeIn struct {
	X1         float64 `json:"x1"`
	Y1         float64 `json:"y1"`
	X2         float64 `json:"x2"`
	Y2         float64 `json:"y2"`
	DurationMs int     `json:"duration_ms,omitempty"`
}

type widgetKeyIn struct {
	Name string `json:"name"`
}

type widgetPasteIn struct {
	Text string `json:"text"`
}

type widgetConnectWiFiIn struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// registerWidgetOpen adds the one widget tool the model can see.
//
// The widget RESOURCE itself is published by internal/mcpserver, before any tool
// group runs, because its _meta has to exist before a client can read it. Its
// CSP connect-domain list is resolved at marshal time from
// widget.SetLoopbackPort — see internal/widget for why that indirection exists.
func registerWidgetOpen(s *mcp.Server, _ *mcpserver.Env) error {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "open_scrcpymac",
		Title:       "Open ScrcpyMac",
		Description: "Open the complete ScrcpyMac phone workspace in Codex.",
		Meta:        OpenToolMeta,
		Annotations: Annotations(true, false, true, false),
		InputSchema: ObjectSchema("open_scrcpymacArguments",
			Prop{Name: "display_mode", Type: "string", Title: "Display Mode", Default: "fullscreen"},
		),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in widgetOpenIn) (*mcp.CallToolResult, any, error) {
		return Structured(widgetOpenPayload(in.DisplayMode), "Opened the ScrcpyMac widget.")
	})
	return nil
}

func registerWidgetAppTools(s *mcp.Server, env *mcpserver.Env) error {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_state",
		Title:       "Read ScrcpyMac UI state",
		Description: "Return device discovery and backend state for the ScrcpyMac widget.",
		Meta:        AppOnlyMeta,
		Annotations: Annotations(true, false, true, false),
		InputSchema: ObjectSchema("scrcpymac_ui_stateArguments"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ widgetNoArgsIn) (*mcp.CallToolResult, any, error) {
		payload, text := widgetState(ctx, env)
		// Deliberately Structured and not StructuredError even on failure: the
		// Python's failure branch here returns isError:false with ok:false so the
		// widget can still show which backend is live and why nothing was found.
		return Structured(payload, text)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_select_device",
		Title:       "Select ScrcpyMac device",
		Description: "Select the Android serial used by subsequent widget actions.",
		Meta:        AppOnlyMeta,
		Annotations: Annotations(false, false, true, false),
		InputSchema: ObjectSchema("scrcpymac_ui_select_deviceArguments",
			Prop{Name: "serial", Type: "string", Title: "Serial", Required: true},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in widgetSelectDeviceIn) (*mcp.CallToolResult, any, error) {
		payload, err := widgetSelectDevice(ctx, env, in.Serial)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, fmt.Sprintf("Selected Android device %s.", in.Serial))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_snapshot",
		Title:       "Capture ScrcpyMac preview frame",
		Description: "Capture a compressed frame for the ScrcpyMac widget.",
		Meta:        AppOnlyMeta,
		Annotations: Annotations(true, false, false, false),
		InputSchema: ObjectSchema("scrcpymac_ui_snapshotArguments",
			Prop{Name: "max_width", Type: "integer", Title: "Max Width", Default: widgetSnapshotDefaultMaxWidth},
			Prop{Name: "quality", Type: "integer", Title: "Quality", Default: widgetSnapshotDefaultQuality},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in widgetSnapshotIn) (*mcp.CallToolResult, any, error) {
		payload, err := widgetSnapshot(ctx, env, in.MaxWidth, in.Quality)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, "Captured a ScrcpyMac preview frame.")
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_tap",
		Title:       "Tap ScrcpyMac preview",
		Description: "Tap normalized preview coordinates.",
		Meta:        AppOnlyMeta,
		Annotations: Annotations(false, false, false, false),
		InputSchema: ObjectSchema("scrcpymac_ui_tapArguments",
			Prop{Name: "x", Type: "number", Title: "X", Required: true},
			Prop{Name: "y", Type: "number", Title: "Y", Required: true},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in widgetTapIn) (*mcp.CallToolResult, any, error) {
		payload, err := widgetTap(ctx, env, in.X, in.Y)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, "Tapped the Android screen.")
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_swipe",
		Title:       "Swipe ScrcpyMac preview",
		Description: "Swipe between normalized preview coordinates.",
		Meta:        AppOnlyMeta,
		Annotations: Annotations(false, false, false, false),
		InputSchema: ObjectSchema("scrcpymac_ui_swipeArguments",
			Prop{Name: "x1", Type: "number", Title: "X1", Required: true},
			Prop{Name: "y1", Type: "number", Title: "Y1", Required: true},
			Prop{Name: "x2", Type: "number", Title: "X2", Required: true},
			Prop{Name: "y2", Type: "number", Title: "Y2", Required: true},
			Prop{Name: "duration_ms", Type: "integer", Title: "Duration Ms", Default: 300},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in widgetSwipeIn) (*mcp.CallToolResult, any, error) {
		payload, err := widgetSwipe(ctx, env, in)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, "Swiped the Android screen.")
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_key",
		Title:       "Press ScrcpyMac navigation key",
		Description: "Press an Android navigation or hardware key.",
		Meta:        AppOnlyMeta,
		Annotations: Annotations(false, false, false, false),
		InputSchema: ObjectSchema("scrcpymac_ui_keyArguments",
			Prop{Name: "name", Type: "string", Title: "Name", Required: true},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in widgetKeyIn) (*mcp.CallToolResult, any, error) {
		payload, err := widgetKey(ctx, env, in.Name)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, fmt.Sprintf("Pressed Android key %s.", in.Name))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_paste",
		Title:       "Paste text through ScrcpyMac",
		Description: "Paste text into the focused Android field.",
		Meta:        AppOnlyMeta,
		Annotations: Annotations(false, false, false, false),
		InputSchema: ObjectSchema("scrcpymac_ui_pasteArguments",
			Prop{Name: "text", Type: "string", Title: "Text", Required: true},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in widgetPasteIn) (*mcp.CallToolResult, any, error) {
		payload, err := widgetPaste(ctx, env, in.Text)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, "Pasted text into Android.")
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "scrcpymac_ui_connect_wifi",
		Title:       "Connect ScrcpyMac over Wi-Fi",
		Description: "Connect adb to an Android device over Wi-Fi.",
		Meta:        AppOnlyMeta,
		// The only tool in the whole server with openWorldHint: true.
		Annotations: Annotations(false, false, false, true),
		InputSchema: ObjectSchema("scrcpymac_ui_connect_wifiArguments",
			Prop{Name: "host", Type: "string", Title: "Host", Required: true},
			Prop{Name: "port", Type: "integer", Title: "Port", Default: widgetDefaultWiFiPortValue},
		),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in widgetConnectWiFiIn) (*mcp.CallToolResult, any, error) {
		payload, err := widgetConnectWiFi(ctx, env, in.Host, in.Port)
		if err != nil {
			return StructuredError(err)
		}
		return Structured(payload, "Connected to Android over Wi-Fi.")
	})

	return nil
}

// StreamDiagnosticsEnv opts phone_stream_status in. Accepted true values are
// "1", "true", "yes" and "on", case-insensitively; anything else (including
// unset) leaves the tool unregistered.
const StreamDiagnosticsEnv = "PHONE_AGENT_STREAM_DIAGNOSTICS"

// StreamDiagnosticsEnabled reports whether phone_stream_status is registered.
func StreamDiagnosticsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(StreamDiagnosticsEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// registerWidgetDiagnostics adds phone_stream_status — but only when
// PHONE_AGENT_STREAM_DIAGNOSTICS opts in.
//
// The tool itself is an ADDITION, not a contract change: the Python's only
// stream-health tool (scrcpymac_ui_stream_status) is app-only, which is exactly
// why the ~1 FPS regression was so hard to diagnose — the model could see a
// frozen picture but had no way to ask whether frames were leaving the device,
// whether a client's queue was full, or whether the relay was dropping GOPs.
//
// It is OFF by default because it is model-visible, and a 25th phone_* tool is
// something Codex can see: the migration's hard constraint is that the Go server
// is indistinguishable from the Python one, and docs/contract.json freezes the
// surface at 36 tools. Exporting PHONE_AGENT_STREAM_DIAGNOSTICS=1 turns it on
// for a debugging session without changing what a Marketplace install ships.
func registerWidgetDiagnostics(s *mcp.Server, _ *mcpserver.Env) error {
	if !StreamDiagnosticsEnabled() {
		return nil
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "phone_stream_status",
		Description: "Diagnose the plugin's H.264 mirroring stream: state, measured device frame " +
			"and packet rate, total/keyframe counts, per-client queue depth and drop counters. " +
			"Read-only. Use it when the mirrored picture is frozen, stuttering or stale.",
		Annotations:  Annotations(true, false, true, false),
		InputSchema:  ObjectSchema("phone_stream_statusArguments"),
		OutputSchema: StringOutputSchema("phone_stream_status"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ widgetNoArgsIn) (*mcp.CallToolResult, StringOut, error) {
		return JSONPayload(widgetStreamStatusPayload(ActiveStreamRuntime().Diagnostics()))
	})
	return nil
}

// ---------------------------------------------------------------------------
// Tool bodies
// ---------------------------------------------------------------------------

// widgetOpenPayload normalises display_mode and builds the open payload.
// Anything that is not "inline" or "fullscreen" — including the empty string a
// client can send explicitly — becomes "fullscreen".
func widgetOpenPayload(displayMode string) *jsonresult.Obj {
	mode := displayMode
	if mode != "inline" && mode != "fullscreen" {
		mode = "fullscreen"
	}
	return jsonresult.Of(
		"ok", true,
		"widget", "scrcpymac-app",
		"preferredDisplayMode", mode,
		"phase", "standalone-h264-stream",
	)
}

// widgetState reports devices plus backend and stream state, and returns the
// text block alongside the payload.
//
// On failure it returns a populated payload with ok:false and the message as the
// text block, NOT an error: the caller must not turn this into isError.
func widgetState(ctx context.Context, env *mcpserver.Env) (*jsonresult.Obj, string) {
	runtime := ActiveStreamRuntime()
	backend := runtime.BackendName()
	status := runtime.Status()

	devices, err := widgetDevices(ctx, env)
	serial := widgetSelectedSerial(env, status)
	if err != nil {
		return jsonresult.Of(
			"ok", false,
			"backend", backend,
			"selectedSerial", serial,
			"devices", []adb.Device{},
			"stream", status,
			"error", err.Error(),
		), err.Error()
	}
	return jsonresult.Of(
		"ok", true,
		"backend", backend,
		"selectedSerial", serial,
		"devices", devices,
		"stream", status,
	), "Read ScrcpyMac device state."
}

// widgetSelectDevice pins the shared adb client to a serial after validating it
// against `adb devices -l`.
func widgetSelectDevice(ctx context.Context, env *mcpserver.Env, serial string) (*jsonresult.Obj, error) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return nil, &adb.Error{Msg: widgetErrEmptySerial}
	}
	client, err := env.ADB()
	if err != nil {
		return nil, err
	}
	devices, err := client.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.Serial != serial {
			continue
		}
		if device.State != adb.StateReady {
			// adb.Error, not a bare error: the Python raises AdbError here and
			// server.py's except clause is what turns it into {"ok": false, ...}.
			return nil, &adb.Error{Msg: fmt.Sprintf("Android device is %s: %s", device.State, serial)}
		}
		client.SetSerial(serial)
		widgetNotifyDeviceChange()
		return jsonresult.Of("ok", true, "serial", serial, "device", device), nil
	}
	return nil, &adb.Error{Msg: fmt.Sprintf("Android device not found: %s", serial)}
}

// widgetSnapshot captures a preview frame for the widget.
//
// The clamps happen before the capture, exactly as in the Python, so an
// out-of-range request still produces a usable frame rather than an error.
func widgetSnapshot(ctx context.Context, env *mcpserver.Env, maxWidth, quality int) (*jsonresult.Obj, error) {
	maxWidth = Clamp(maxWidth, widgetSnapshotMinMaxWidth, widgetSnapshotMaxMaxWidth)
	quality = Clamp(quality, widgetSnapshotMinQuality, widgetSnapshotMaxQuality)

	shot, err := widgetADBScreenshot(ctx, env)
	if err != nil {
		return nil, err
	}
	return widgetPreviewPayload(shot, maxWidth, quality)
}

// widgetTap taps normalised coordinates.
//
//	streaming -> {ok, action, serial, point, coordinateSpace, backend}
//	adb       -> {ok, coordinate_space, source, device_point, tap}
//
// Note the asymmetry, which is contract: the streaming path CLAMPS x/y into
// [0,1] while the adb path REJECTS anything outside it.
func widgetTap(ctx context.Context, env *mcpserver.Env, x, y float64) (*jsonresult.Obj, error) {
	runtime := ActiveStreamRuntime()
	if runtime.IsActive() {
		payload, err := runtime.TapRelative(ctx, x, y)
		if err != nil {
			return nil, err
		}
		widgetNotifyDeviceChange()
		return payload, nil
	}
	return widgetADBTapRelative(ctx, env, x, y)
}

// widgetSwipe drags between normalised coordinates.
//
//	streaming -> {ok, action, serial, from, to, durationMs, backend}
//	adb       -> {ok, action, from, to, duration_ms, serial}
//
// camelCase versus snake_case on the same tool is not a mistake to tidy up; the
// widget switches on it.
func widgetSwipe(ctx context.Context, env *mcpserver.Env, in widgetSwipeIn) (*jsonresult.Obj, error) {
	for _, v := range [...]float64{in.X1, in.Y1, in.X2, in.Y2} {
		if !(v >= 0.0 && v <= 1.0) {
			return nil, &adb.Error{Msg: widgetErrSwipeRange}
		}
	}
	duration := Clamp(in.DurationMs, 0, widgetSwipeMaxDurationMs)

	runtime := ActiveStreamRuntime()
	if runtime.IsActive() {
		payload, err := runtime.SwipeRelative(ctx, in.X1, in.Y1, in.X2, in.Y2, duration)
		if err != nil {
			return nil, err
		}
		widgetNotifyDeviceChange()
		return payload, nil
	}

	width, height, err := widgetDeviceScreenSize(ctx, env)
	if err != nil {
		return nil, err
	}
	return widgetADBSwipe(ctx, env,
		jsonresult.PyRoundInt(in.X1*float64(width-1)),
		jsonresult.PyRoundInt(in.Y1*float64(height-1)),
		jsonresult.PyRoundInt(in.X2*float64(width-1)),
		jsonresult.PyRoundInt(in.Y2*float64(height-1)),
		duration,
	)
}

// widgetKey presses a named key.
//
//	streaming -> {ok, action, key, serial, backend}   (10 keys, no "paste")
//	adb       -> {ok, action, key, code, serial}      (11 keys, with "paste")
func widgetKey(ctx context.Context, env *mcpserver.Env, name string) (*jsonresult.Obj, error) {
	runtime := ActiveStreamRuntime()
	if runtime.IsActive() {
		payload, err := runtime.Key(ctx, name)
		if err != nil {
			return nil, err
		}
		widgetNotifyDeviceChange()
		return payload, nil
	}
	return widgetADBKey(ctx, env, name)
}

// widgetPaste pastes into the focused field.
//
//	streaming -> {ok, action, length, serial, backend}
//	adb       -> {ok, action, length, serial}
//
// length is a rune count on both paths — len() in Python counts code points, and
// this tool exists mainly for Chinese text.
func widgetPaste(ctx context.Context, env *mcpserver.Env, text string) (*jsonresult.Obj, error) {
	if text == "" {
		// Checked here rather than once per branch. Both the runtime and the adb
		// path raise the identical message in the Python, so hoisting it changes
		// nothing observable and protects the runtime path from forgetting.
		return nil, &adb.Error{Msg: widgetErrEmptyText}
	}
	runtime := ActiveStreamRuntime()
	if runtime.IsActive() {
		payload, err := runtime.Paste(ctx, text)
		if err != nil {
			return nil, err
		}
		widgetNotifyDeviceChange()
		return payload, nil
	}
	return widgetADBPaste(ctx, env, text)
}

// widgetConnectWiFi pairs with a device over TCP/IP.
//
// host is trimmed here. phone_connect_wifi deliberately does NOT trim; the two
// tools differ and both behaviours are reproduced in their own files.
func widgetConnectWiFi(ctx context.Context, env *mcpserver.Env, host string, port int) (*jsonresult.Obj, error) {
	host = strings.TrimSpace(host)
	client, err := env.ADB()
	if err != nil {
		return nil, err
	}
	// No EnsureDevice: connecting over Wi-Fi is what you do when nothing is
	// attached yet.
	output, err := client.ConnectWiFi(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"action", "connect_wifi",
		"target", widgetWiFiTarget(host, port),
		"output", output,
	), nil
}

// widgetWiFiTarget is `host if ":" in host else f"{host}:{port}"`, the same rule
// adb.Client.ConnectWiFi applies internally. It is recomputed rather than
// returned by that call because the payload has to echo it.
func widgetWiFiTarget(host string, port int) string {
	if strings.Contains(host, ":") {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// ---------------------------------------------------------------------------
// adb fallbacks
// ---------------------------------------------------------------------------

// widgetDevices lists devices, normalising nil to an empty slice so the payload
// carries [] and never null.
func widgetDevices(ctx context.Context, env *mcpserver.Env) ([]adb.Device, error) {
	client, err := env.ADB()
	if err != nil {
		return nil, err
	}
	devices, err := client.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	if devices == nil {
		devices = []adb.Device{}
	}
	return devices, nil
}

// widgetSelectedSerial is PhoneActions.selected_serial: the streaming serial if
// there is one, then the adb client's pinned serial, then PHONE_AGENT_SERIAL.
//
// It deliberately performs no adb call — it is read on every scrcpymac_ui_state,
// including the failure path where adb is what just failed.
func widgetSelectedSerial(env *mcpserver.Env, status *jsonresult.Obj) string {
	if serial := widgetObjString(status, "serial"); serial != "" {
		return serial
	}
	if client, err := env.ADB(); err == nil {
		if serial := client.Serial(); serial != "" {
			return serial
		}
	}
	return os.Getenv(adb.SerialEnv)
}

// widgetShot is one captured frame plus the geometry the payload reports.
type widgetShot struct {
	Serial  string
	Backend string
	// Width/Height come from `wm size`, NOT from the PNG header. Every relative
	// coordinate is mapped into that same space, so they have to agree.
	Width, Height int
	// PNG is the raw screencap.
	PNG []byte
	// Image, when set, is an ALREADY-ENCODED frame (the H.264-derived path in
	// mcp_ui._preview_frame). It is passed through untouched and skips decode,
	// resize and re-encode entirely.
	Image                   []byte
	MIMEType                string
	FrameWidth, FrameHeight int
}

// widgetADBScreenshot is PhoneActions._adb_screenshot: a full-resolution PNG
// plus the screen size. preview_frame ignores max_width/quality on this path —
// the downscale happens afterwards, in widgetPreviewPayload.
func widgetADBScreenshot(ctx context.Context, env *mcpserver.Env) (widgetShot, error) {
	client, err := env.ADB()
	if err != nil {
		return widgetShot{}, err
	}
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return widgetShot{}, err
	}
	png, err := client.ScreenshotPNG(ctx)
	if err != nil {
		return widgetShot{}, err
	}
	width, height, err := client.ScreenSize(ctx)
	if err != nil {
		return widgetShot{}, err
	}
	return widgetShot{Serial: serial, Backend: "adb", Width: width, Height: height, PNG: png}, nil
}

// widgetPreviewPayload is mcp_ui._preview_frame.
//
// The base64 lands in structuredContent.dataBase64 only; it must never reach the
// text block, which the existing pytest asserts explicitly.
func widgetPreviewPayload(shot widgetShot, maxWidth, quality int) (*jsonresult.Obj, error) {
	backend := shot.Backend
	if backend == "" {
		backend = "adb"
	}
	if len(shot.Image) > 0 {
		mime := shot.MIMEType
		if mime == "" {
			mime = "image/jpeg"
		}
		return widgetFramePayload(shot.Serial, backend, shot.Width, shot.Height,
			shot.FrameWidth, shot.FrameHeight, mime, shot.Image), nil
	}

	encoded, frameWidth, frameHeight, err := widgetCompressPreview(shot.PNG, maxWidth, quality)
	if err != nil {
		return nil, err
	}
	return widgetFramePayload(shot.Serial, backend, shot.Width, shot.Height,
		frameWidth, frameHeight, "image/jpeg", encoded), nil
}

func widgetFramePayload(serial, backend string, deviceWidth, deviceHeight, frameWidth, frameHeight int, mime string, data []byte) *jsonresult.Obj {
	return jsonresult.Of(
		"ok", true,
		"serial", serial,
		"backend", backend,
		"deviceWidth", deviceWidth,
		"deviceHeight", deviceHeight,
		"frameWidth", frameWidth,
		"frameHeight", frameHeight,
		"mimeType", mime,
		"dataBase64", base64.StdEncoding.EncodeToString(data),
		"sizeBytes", len(data),
	)
}

// widgetADBTapRelative is PhoneActions.tap_relative(x, y, verify=False,
// retries=0) — the only way scrcpymac_ui_tap ever calls it.
func widgetADBTapRelative(ctx context.Context, env *mcpserver.Env, x, y float64) (*jsonresult.Obj, error) {
	if !(x >= 0.0 && x <= 1.0 && y >= 0.0 && y <= 1.0) {
		return nil, &adb.Error{Msg: widgetErrTapRange}
	}
	width, height, err := widgetRequireScreenSize(ctx, env)
	if err != nil {
		return nil, err
	}
	// Python's round() is banker's rounding. math.Round would shift the tap by a
	// pixel on every exact .5 — round(0.5*1077) is 538, not 539.
	deviceX := jsonresult.PyRoundInt(x * float64(width-1))
	deviceY := jsonresult.PyRoundInt(y * float64(height-1))

	tap, err := widgetADBTap(ctx, env, deviceX, deviceY, width, height)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"coordinate_space", "relative",
		// Python floats: 0.0 must not serialise as 0.
		"source", []any{jsonresult.Float(x), jsonresult.Float(y)},
		"device_point", []int{deviceX, deviceY},
		"tap", tap,
	), nil
}

// widgetADBTap is PhoneActions._tap_once on the adb path, with the clamp
// PhoneActions.tap applies first.
func widgetADBTap(ctx context.Context, env *mcpserver.Env, x, y, width, height int) (*jsonresult.Obj, error) {
	x = Clamp(x, 0, width-1)
	y = Clamp(y, 0, height-1)
	client, err := env.ADB()
	if err != nil {
		return nil, err
	}
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, fmt.Sprintf("input tap %d %d", x, y)); err != nil {
		return nil, err
	}
	widgetNotifyDeviceChange()
	return jsonresult.Of("ok", true, "action", "tap", "x", x, "y", y, "serial", serial), nil
}

// widgetADBSwipe is PhoneActions.swipe on the adb path. The echoed coordinates
// are the device pixels, already rounded by the caller.
func widgetADBSwipe(ctx context.Context, env *mcpserver.Env, x1, y1, x2, y2, durationMs int) (*jsonresult.Obj, error) {
	client, err := env.ADB()
	if err != nil {
		return nil, err
	}
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, durationMs)); err != nil {
		return nil, err
	}
	widgetNotifyDeviceChange()
	return jsonresult.Of(
		"ok", true,
		"action", "swipe",
		"from", []int{x1, y1},
		"to", []int{x2, y2},
		"duration_ms", durationMs,
		"serial", serial,
	), nil
}

// widgetADBKey is PhoneActions.key on the adb path.
func widgetADBKey(ctx context.Context, env *mcpserver.Env, name string) (*jsonresult.Obj, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	code, ok := widgetKeycodes[key]
	if !ok {
		return nil, &adb.Error{Msg: fmt.Sprintf("Unknown key %s. Supported: %s",
			widgetPyRepr(name), strings.Join(widgetKeyNames, ", "))}
	}
	client, err := env.ADB()
	if err != nil {
		return nil, err
	}
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, fmt.Sprintf("input keyevent %d", code)); err != nil {
		return nil, err
	}
	widgetNotifyDeviceChange()
	return jsonresult.Of(
		"ok", true,
		"action", "key",
		"key", key,
		"code", code,
		"serial", serial,
	), nil
}

// widgetADBPaste is PhoneActions.paste on the adb path: set the clipboard, wait
// for the clipboard service, then press KEYCODE_PASTE.
//
// adb.Quote is what keeps Chinese text, spaces and shell metacharacters intact —
// the command is parsed by /system/bin/sh on the phone, not by the host.
func widgetADBPaste(ctx context.Context, env *mcpserver.Env, text string) (*jsonresult.Obj, error) {
	client, err := env.ADB()
	if err != nil {
		return nil, err
	}
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.Shell(ctx, "cmd clipboard set-text "+adb.Quote(text)); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(widgetPasteSettle):
	}
	if _, err := client.Shell(ctx, fmt.Sprintf("input keyevent %d", widgetKeycodes["paste"])); err != nil {
		return nil, err
	}
	widgetNotifyDeviceChange()
	return jsonresult.Of(
		"ok", true,
		"action", "paste",
		"length", jsonresult.RuneLen(text),
		"serial", serial,
	), nil
}

// widgetScreenSize is PhoneActions._screen_size: the streaming geometry when a
// session is live, otherwise `wm size`, and (false) rather than an error when
// neither works.
func widgetScreenSize(ctx context.Context, env *mcpserver.Env) (int, int, bool) {
	status := ActiveStreamRuntime().Status()
	if widgetObjString(status, "state") == "streaming" {
		width := widgetObjInt(status, "deviceWidth")
		height := widgetObjInt(status, "deviceHeight")
		if width != 0 && height != 0 {
			return width, height, true
		}
	}
	client, err := env.ADB()
	if err != nil {
		return 0, 0, false
	}
	if _, err := client.EnsureDevice(ctx); err != nil {
		return 0, 0, false
	}
	width, height, err := client.ScreenSize(ctx)
	if err != nil {
		return 0, 0, false
	}
	return width, height, true
}

// widgetRequireScreenSize is PhoneActions._required_screen_size. It deliberately
// discards the underlying adb error: the model sees the generic message, which
// is what the Python reports.
func widgetRequireScreenSize(ctx context.Context, env *mcpserver.Env) (int, int, error) {
	width, height, ok := widgetScreenSize(ctx, env)
	if !ok {
		return 0, 0, &adb.Error{Msg: widgetErrNoScreenSize}
	}
	return width, height, nil
}

// widgetDeviceScreenSize is the screen half of PhoneActions.device_info, which
// is what scrcpymac_ui_swipe's adb path uses. Unlike widgetScreenSize it
// PROPAGATES the adb error, so a missing device reports "No Android device
// connected. ..." rather than the generic screen-size message. The two tools
// really do report different errors for the same condition.
//
// DEVIATION: device_info() also probes the foreground app, whose result the
// swipe payload never uses. That adb round trip (and its failure mode) is
// skipped here.
func widgetDeviceScreenSize(ctx context.Context, env *mcpserver.Env) (int, int, error) {
	status := ActiveStreamRuntime().Status()
	if widgetObjString(status, "state") == "streaming" {
		width := widgetObjInt(status, "deviceWidth")
		height := widgetObjInt(status, "deviceHeight")
		if width > 0 && height > 0 {
			return max(1, width), max(1, height), nil
		}
	}
	client, err := env.ADB()
	if err != nil {
		return 0, 0, err
	}
	if _, err := client.EnsureDevice(ctx); err != nil {
		return 0, 0, err
	}
	width, height, err := client.ScreenSize(ctx)
	if err != nil {
		return 0, 0, err
	}
	return max(1, width), max(1, height), nil
}

// ---------------------------------------------------------------------------
// phone_stream_status payload
// ---------------------------------------------------------------------------

// widgetStreamStatusPayload renders the relay diagnostics.
//
// Rates are rounded to one decimal like ScrcpyRuntime.status()["fps"] and are
// jsonresult.Float so 0.0 stays "0.0". Counters are plain integers.
func widgetStreamStatusPayload(d StreamDiagnostics) *jsonresult.Obj {
	clients := make([]*jsonresult.Obj, 0, len(d.Clients))
	for _, c := range d.Clients {
		clients = append(clients, jsonresult.Of(
			"id", c.ID,
			"connectedS", jsonresult.Float(jsonresult.PyRound(c.ConnectedS, 1)),
			"queueDepth", c.QueueDepth,
			"queueCapacity", c.QueueCapacity,
			"packetsSent", c.PacketsSent,
			"bytesSent", c.BytesSent,
			"droppedGops", c.DroppedGOPs,
			"droppedPackets", c.DroppedPackets,
			"waitingForKeyFrame", c.WaitingForKeyFrame,
		))
	}

	payload := jsonresult.Of(
		"ok", true,
		"state", d.State,
		"backend", d.Backend,
		"serial", d.Serial,
		"codec", d.Codec,
		"deviceWidth", d.DeviceWidth,
		"deviceHeight", d.DeviceHeight,
		"frameWidth", d.FrameWidth,
		"frameHeight", d.FrameHeight,
		"uptimeS", jsonresult.Float(jsonresult.PyRound(d.UptimeS, 1)),
		"packets", d.Packets,
		"frames", d.Frames,
		"keyFrames", d.KeyFrames,
		"bytes", d.Bytes,
		"packetsPerSecond", jsonresult.Float(jsonresult.PyRound(d.PacketsPerSecond, 1)),
		"framesPerSecond", jsonresult.Float(jsonresult.PyRound(d.FramesPerSecond, 1)),
		"droppedGops", d.DroppedGOPs,
		"droppedPackets", d.DroppedPackets,
		"clientCount", len(clients),
		"clients", clients,
		"error", d.LastError,
	)
	if hint := widgetStreamHint(d); hint != "" {
		payload.Set("hint", hint)
	}
	return payload
}

// widgetStreamHint names the likeliest cause when the numbers look wrong, so the
// model does not have to know how an H.264 relay is supposed to behave.
//
// The two failure modes are genuinely different and the fix is different too:
// a slow device encoder means nothing arrives to forward, while a backed-up
// client queue means frames arrive and are then thrown away.
func widgetStreamHint(d StreamDiagnostics) string {
	if d.State != "streaming" {
		return ""
	}
	for _, c := range d.Clients {
		if c.QueueCapacity > 0 && c.QueueDepth >= c.QueueCapacity {
			return "A viewer's send queue is full, so whole GOPs are being dropped and the " +
				"picture will freeze until the next keyframe. The bottleneck is the client " +
				"or the loopback socket, not the device."
		}
	}
	if d.Frames > 0 && d.FramesPerSecond > 0 && d.FramesPerSecond < 5 {
		return "The device is delivering fewer than 5 frames per second. The bottleneck is " +
			"upstream of the relay: the device encoder, max_fps, or the adb transport."
	}
	if d.Frames == 0 {
		return "No frames have arrived from the device yet."
	}
	return ""
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// widgetObjString reads a string field out of a runtime payload.
func widgetObjString(o *jsonresult.Obj, key string) string {
	if o == nil {
		return ""
	}
	v, ok := o.Get(key)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// widgetObjInt reads an integer field out of a runtime payload, accepting the
// numeric types a hand-built or JSON-decoded payload can hold.
func widgetObjInt(o *jsonresult.Obj, key string) int {
	if o == nil {
		return 0
	}
	v, ok := o.Get(key)
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return 0
}

// widgetPyRepr renders a string the way Python's {!r} does, because the
// unknown-key error reaches the model verbatim: Unknown key 'foo'.
//
// Python prefers single quotes and switches to double quotes only when the value
// contains a single quote and no double quote.
func widgetPyRepr(s string) string {
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
