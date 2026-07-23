package tools

// device.go — the device/introspection tool group:
//
//	phone_backend, phone_doctor, phone_list_devices, phone_device_info,
//	phone_screenshot, phone_shell, phone_current_app, phone_launch_app
//
// Ported from server/phone_agent/server.py (the @mcp.tool wrappers) and
// server/phone_agent/actions.py (backend, devices, device_info, screenshot,
// shell, current_app, launch_app). docs/contract.json is the frozen surface
// these must match; device_test.go asserts against it.
//
// Seven of the eight are Shape A (`-> str`): the text block is the bare JSON
// payload and structuredContent is {"result": "<that same text>"}.
// phone_screenshot is Shape B — the only tool in the Python server with no
// return annotation, so it has no outputSchema and no structuredContent, and it
// emits one or two content blocks.
//
// Payload key ORDER is contract, not cosmetics: every payload is built with
// jsonresult.Obj, and OK() appends "ok" last exactly where Python's
// payload.setdefault("ok", True) did.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/doctor"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

func init() {
	mcpserver.Register(mcpserver.Registration{
		Name:  "phone-device",
		Order: mcpserver.OrderPhoneTools,
		Apply: registerPhoneDevice,
	})
}

// ---------------------------------------------------------------------------
// Seams the scrcpy runtime plugs into
// ---------------------------------------------------------------------------

// DeviceStreamStatus is the slice of ScrcpyRuntime.status() that the device
// tools read. It exists so this file compiles and behaves correctly before the
// scrcpy runtime lands: with no provider installed every tool takes the adb
// path, which is exactly what the Python does when nothing is streaming.
//
// Field meanings are the runtime status keys of the same name: DeviceWidth /
// DeviceHeight are native device pixels, FrameWidth / FrameHeight are the
// encoded video size, FPS is already rounded to one decimal by the runtime.
type DeviceStreamStatus struct {
	Serial       string
	DeviceWidth  int
	DeviceHeight int
	FrameWidth   int
	FrameHeight  int
	FPS          float64
	Codec        string
}

// DeviceStreamStatusFunc reports the live stream status. A false second result
// means "not streaming" — the adb fallback path.
type DeviceStreamStatusFunc func() (DeviceStreamStatus, bool)

// DeviceStateChangeHook is called after a tool changes what is on screen, so
// caches built from the screen (the ui_tree cache) can drop their contents.
// It replaces actions.py's _invalidate_ui_tree_cache().
type DeviceStateChangeHook func()

var (
	deviceHooksMu    sync.RWMutex
	deviceStreamFn   DeviceStreamStatusFunc
	deviceStateHooks []DeviceStateChangeHook
)

// SetDeviceStreamStatus installs the scrcpy runtime's status accessor. Passing
// nil restores the adb-only behaviour. Safe to call at any time; tool handlers
// read it under a lock.
func SetDeviceStreamStatus(fn DeviceStreamStatusFunc) {
	deviceHooksMu.Lock()
	defer deviceHooksMu.Unlock()
	deviceStreamFn = fn
}

// RegisterDeviceStateChangeHook adds a callback run after phone_launch_app (and
// any future device-mutating tool in this file) changes the screen. Hooks run in
// registration order and must not block.
func RegisterDeviceStateChangeHook(hook DeviceStateChangeHook) {
	if hook == nil {
		return
	}
	deviceHooksMu.Lock()
	defer deviceHooksMu.Unlock()
	deviceStateHooks = append(deviceStateHooks, hook)
}

func deviceStreamStatus() (DeviceStreamStatus, bool) {
	deviceHooksMu.RLock()
	fn := deviceStreamFn
	deviceHooksMu.RUnlock()
	if fn == nil {
		return DeviceStreamStatus{}, false
	}
	return fn()
}

func deviceStateChanged() {
	deviceHooksMu.RLock()
	hooks := make([]DeviceStateChangeHook, len(deviceStateHooks))
	copy(hooks, deviceStateHooks)
	deviceHooksMu.RUnlock()
	for _, hook := range hooks {
		hook()
	}
}

// ---------------------------------------------------------------------------
// Tool arguments
// ---------------------------------------------------------------------------

// deviceNoArgs is the parameter type of the five tools that take none.
type deviceNoArgs struct{}

type deviceScreenshotIn struct {
	IncludeImage bool `json:"include_image"`
}

type deviceShellIn struct {
	Command string `json:"command"`
}

type deviceLaunchAppIn struct {
	Package  string `json:"package"`
	Activity string `json:"activity,omitempty"`
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func registerPhoneDevice(s *mcp.Server, env *mcpserver.Env) error {
	mcp.AddTool(s, &mcp.Tool{
		Name:         "phone_backend",
		Description:  "Report whether the standalone H.264 runtime or adb fallback is active.",
		InputSchema:  ObjectSchema("phone_backendArguments"),
		OutputSchema: StringOutputSchema("phone_backend"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ deviceNoArgs) (*mcp.CallToolResult, StringOut, error) {
		return JSONPayload(deviceBackendPayload())
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:         "phone_doctor",
		Description:  "Run environment diagnostics for adb, the connected device, and bundled runtime assets.",
		InputSchema:  ObjectSchema("phone_doctorArguments"),
		OutputSchema: StringOutputSchema("phone_doctor"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ deviceNoArgs) (*mcp.CallToolResult, StringOut, error) {
		// Deliberately not wrapped: phone_doctor is the one tool with no
		// try/except in the Python, and doctor.Run turns every internal failure
		// into a failing check instead of an error. "ok" is FIRST here because
		// the payload carries it already — _ok() is not involved.
		return JSONPayload(doctor.Run(ctx))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:         "phone_list_devices",
		Description:  "List Android devices visible to adb.",
		InputSchema:  ObjectSchema("phone_list_devicesArguments"),
		OutputSchema: StringOutputSchema("phone_list_devices"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ deviceNoArgs) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(deviceListDevicesPayload(ctx, client))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:         "phone_device_info",
		Description:  "Get connected device serial, screen size, and foreground app.",
		InputSchema:  ObjectSchema("phone_device_infoArguments"),
		OutputSchema: StringOutputSchema("phone_device_info"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ deviceNoArgs) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(deviceInfoPayload(ctx, client))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_screenshot",
		Description: "Capture the device screen. Returns metadata JSON and optionally a PNG image.",
		InputSchema: ObjectSchema("phone_screenshotArguments",
			Prop{Name: "include_image", Type: "boolean", Title: "Include Image", Default: true},
		),
		// No OutputSchema: phone_screenshot is the only server.py tool without a
		// return annotation, so FastMCP emitted none and structuredContent is
		// null on every path. Out is `any` and stays nil for the same reason.
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deviceScreenshotIn) (*mcp.CallToolResult, any, error) {
		return deviceScreenshotResult(ctx, env, in.IncludeImage)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_shell",
		Description: "Execute an adb shell command on the device.",
		InputSchema: ObjectSchema("phone_shellArguments",
			Prop{Name: "command", Type: "string", Title: "Command", Required: true},
		),
		OutputSchema: StringOutputSchema("phone_shell"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deviceShellIn) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(deviceShellPayload(ctx, client, in.Command))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:         "phone_current_app",
		Description:  "Get the foreground app package and activity.",
		InputSchema:  ObjectSchema("phone_current_appArguments"),
		OutputSchema: StringOutputSchema("phone_current_app"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ deviceNoArgs) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(deviceCurrentAppPayload(ctx, client))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_launch_app",
		Description: "Launch an Android app by package name. Example WeChat: com.tencent.mm.",
		InputSchema: ObjectSchema("phone_launch_appArguments",
			Prop{Name: "package", Type: "string", Title: "Package", Required: true},
			Prop{Name: "activity", Type: "string", Title: "Activity", Default: ""},
		),
		OutputSchema: StringOutputSchema("phone_launch_app"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deviceLaunchAppIn) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(deviceLaunchAppPayload(ctx, client, in.Package, in.Activity))
	})

	return nil
}

// ---------------------------------------------------------------------------
// phone_backend
// ---------------------------------------------------------------------------

// deviceBackendName is ScrcpyRuntime.backend_name(): "plugin-h264" while the
// plugin H.264 session is streaming, "adb" otherwise.
func deviceBackendName() string {
	if _, streaming := deviceStreamStatus(); streaming {
		return "plugin-h264"
	}
	return "adb"
}

// deviceBackendPayload → {"backend": "...", "ok": true}. It touches no device,
// so unlike the Python it cannot fail.
func deviceBackendPayload() *jsonresult.Obj {
	return OK(jsonresult.Of("backend", deviceBackendName()))
}

// ---------------------------------------------------------------------------
// phone_list_devices
// ---------------------------------------------------------------------------

// deviceListDevicesPayload → {"devices": [...], "ok": true}.
//
// Like actions.devices() this uses the client directly rather than _ready(), so
// it succeeds with zero devices connected and reports an empty list. The slice
// is never nil: adb.ParseDevices returns []Device{}, which marshals to [] and
// not null.
func deviceListDevicesPayload(ctx context.Context, client *adb.Client) (*jsonresult.Obj, error) {
	devices, err := client.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	if devices == nil {
		devices = []adb.Device{}
	}
	return OK(jsonresult.Of("devices", devices)), nil
}

// ---------------------------------------------------------------------------
// phone_device_info
// ---------------------------------------------------------------------------

// deviceInfoPayload reproduces actions.device_info().
//
// Streaming → {serial, screen, video, foreground, backend:"plugin-h264", ok};
// adb → {serial, screen, foreground, backend:"adb", ok}. The video sub-object
// exists only on the streaming branch; the screen sub-object exists on both,
// because scrcpymac_ui_swipe's adb fallback reads screen.width/height.
//
// Both branches read the foreground app over adb — the streaming branch is not
// device-free.
func deviceInfoPayload(ctx context.Context, client *adb.Client) (*jsonresult.Obj, error) {
	if status, streaming := deviceStreamStatus(); streaming {
		app, err := deviceForegroundApp(ctx, client)
		if err != nil {
			return nil, err
		}
		return OK(jsonresult.Of(
			"serial", status.Serial,
			"screen", jsonresult.Of("width", status.DeviceWidth, "height", status.DeviceHeight),
			"video", jsonresult.Of(
				"width", status.FrameWidth,
				"height", status.FrameHeight,
				// fps is a Python float rounded to 1dp: 0.0 must not print as 0.
				"fps", jsonresult.Float(status.FPS),
				"codec", status.Codec,
			),
			"foreground", app,
			"backend", "plugin-h264",
		)), nil
	}

	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	width, height, err := client.ScreenSize(ctx)
	if err != nil {
		return nil, err
	}
	app, err := client.CurrentApp(ctx)
	if err != nil {
		return nil, err
	}
	return OK(jsonresult.Of(
		"serial", serial,
		"screen", jsonresult.Of("width", width, "height", height),
		"foreground", deviceAppObj(app),
		"backend", "adb",
	)), nil
}

// ---------------------------------------------------------------------------
// phone_screenshot
// ---------------------------------------------------------------------------

// deviceShot is one adb screen capture. PNG stays out of every JSON payload:
// it is actions.py's in-process-only "png_bytes" key.
type deviceShot struct {
	Serial string
	Width  int
	Height int
	PNG    []byte
}

// deviceCaptureScreenshot is actions._adb_screenshot().
//
// There is no scrcpy-stream path here: even while the H.264 session runs,
// screenshots go through `adb exec-out screencap`. Width and height come from
// the PNG header, which is the exact coordinate space visible to the caller.
func deviceCaptureScreenshot(ctx context.Context, client *adb.Client) (deviceShot, error) {
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return deviceShot{}, err
	}
	// Raw bytes with no newline translation — CRLF normalisation corrupts a PNG.
	data, err := client.ScreenshotPNG(ctx)
	if err != nil {
		return deviceShot{}, err
	}
	width, height, err := deviceRequirePNGSize(data)
	if err != nil {
		return deviceShot{}, err
	}
	return deviceShot{Serial: serial, Width: width, Height: height, PNG: data}, nil
}

// deviceScreenshotPayload builds the metadata payload.
//
// include_image=true omits "base64" (the bytes travel as an image content block
// instead); include_image=false inserts it BEFORE "ok", which _ok() appends
// last. Both orders are pinned by docs/contract.json.
func deviceScreenshotPayload(shot deviceShot, includeImage bool) *jsonresult.Obj {
	payload := jsonresult.Of(
		"serial", shot.Serial,
		"width", shot.Width,
		"height", shot.Height,
		"format", "png",
		"size_bytes", len(shot.PNG),
	)
	if !includeImage {
		payload.Set("base64", base64.StdEncoding.EncodeToString(shot.PNG))
	}
	return OK(payload)
}

func deviceScreenshotResult(ctx context.Context, env *mcpserver.Env, includeImage bool) (*mcp.CallToolResult, any, error) {
	client, err := env.ADB()
	if err != nil {
		return deviceTextOnly(Fail(err))
	}
	shot, err := deviceCaptureScreenshot(ctx, client)
	if err != nil {
		// Failure is a single text block with {"ok": false, "error": ...} and
		// no image block, still isError=false.
		return deviceTextOnly(Fail(err))
	}

	payload := deviceScreenshotPayload(shot, includeImage)
	if includeImage {
		return deviceTextAndPNG(payload, shot.PNG)
	}
	return deviceTextOnly(payload)
}

// deviceTextOnly emits exactly one text block and no structuredContent.
func deviceTextOnly(payload *jsonresult.Obj) (*mcp.CallToolResult, any, error) {
	return TextAndImage(payload, nil, "")
}

// deviceTextAndPNG emits the text block followed by the image block.
//
// Data is RAW bytes: encoding/json base64-encodes []byte on its own, so
// pre-encoding here would ship double-base64 and a broken screenshot.
func deviceTextAndPNG(payload *jsonresult.Obj, data []byte) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: jsonresult.Text(payload)},
			&mcp.ImageContent{Data: data, MIMEType: "image/png"},
		},
	}, nil, nil
}

// devicePNGSize reads the dimensions out of a PNG header.
//
// png.DecodeConfig parses the IHDR chunk only, so this costs a few hundred
// bytes of work rather than decoding a multi-megabyte framebuffer.
func devicePNGSize(data []byte) (int, int, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

func deviceRequirePNGSize(data []byte) (int, int, error) {
	width, height, err := devicePNGSize(data)
	if err != nil {
		return 0, 0, fmt.Errorf("could not decode screenshot PNG: %w", err)
	}
	return width, height, nil
}

// ---------------------------------------------------------------------------
// phone_shell
// ---------------------------------------------------------------------------

// deviceShellPayload → {"ok": true, "output": "...", "serial": "..."}.
//
// The command is passed through verbatim, unquoted and unfiltered — that is the
// point of the tool — as ONE argv element, so the device's /system/bin/sh does
// the parsing. Splitting it host-side would break every pipe and redirection.
// The ui_tree cache is deliberately NOT invalidated here, matching the Python.
func deviceShellPayload(ctx context.Context, client *adb.Client, command string) (*jsonresult.Obj, error) {
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	output, err := client.Shell(ctx, command)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of("ok", true, "output", output, "serial", serial), nil
}

// ---------------------------------------------------------------------------
// phone_current_app
// ---------------------------------------------------------------------------

// deviceCurrentAppPayload → {"foreground": {...}, "serial": "...", "ok": true}.
// "ok" lands last because actions.current_app() does not set it.
func deviceCurrentAppPayload(ctx context.Context, client *adb.Client) (*jsonresult.Obj, error) {
	app, err := deviceForegroundApp(ctx, client)
	if err != nil {
		return nil, err
	}
	serial := deviceSelectedSerial(client)
	if serial == "" {
		if serial, err = client.EnsureDevice(ctx); err != nil {
			return nil, err
		}
	}
	return OK(jsonresult.Of("foreground", app, "serial", serial)), nil
}

// deviceSelectedSerial is actions.selected_serial(): the streaming serial, then
// the client's serial, then $PHONE_AGENT_SERIAL. It performs no adb call and
// never triggers adb resolution, which is what lets the app-facing state tool
// report a serial even on its error path.
func deviceSelectedSerial(client *adb.Client) string {
	if status, streaming := deviceStreamStatus(); streaming && status.Serial != "" {
		return status.Serial
	}
	if client != nil {
		if serial := client.Serial(); serial != "" {
			return serial
		}
	}
	return os.Getenv(adb.SerialEnv)
}

// ---------------------------------------------------------------------------
// phone_launch_app
// ---------------------------------------------------------------------------

// deviceLaunchSettle is the fixed pause actions.launch_app() takes between
// starting the app and reading the foreground app back. It is a var only so the
// tests do not have to spend a real second per launch.
var deviceLaunchSettle = time.Second

// deviceLaunchAppPayload reproduces actions.launch_app().
//
//	→ {ok, action:"launch", package, activity, foreground, serial}
//
// activity is JSON null when the caller passed the "" default, because
// server.py forwards `activity or None`.
//
// The package name and the package/activity component go through adb.Quote,
// which actions.py does not do (spec-actions BUG-4: a package argument
// containing shell metacharacters executes on the device). Every character of a
// legitimate package or activity name is in shlex's safe set, so the emitted
// command is byte-identical for all real input.
func deviceLaunchAppPayload(ctx context.Context, client *adb.Client, pkg, activity string) (*jsonresult.Obj, error) {
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}

	if activity != "" {
		_, err = client.Shell(ctx, "am start -n "+adb.Quote(pkg+"/"+activity))
	} else {
		_, err = client.Shell(ctx, "monkey -p "+adb.Quote(pkg)+" -c android.intent.category.LAUNCHER 1")
	}
	if err != nil {
		return nil, err
	}

	// Invalidate before the settle, exactly like the Python.
	deviceStateChanged()
	if err := deviceSleep(ctx, deviceLaunchSettle); err != nil {
		return nil, err
	}

	app, err := deviceForegroundApp(ctx, client)
	if err != nil {
		return nil, err
	}

	var activityValue any // nil marshals to null
	if activity != "" {
		activityValue = activity
	}
	return jsonresult.Of(
		"ok", true,
		"action", "launch",
		"package", pkg,
		"activity", activityValue,
		"foreground", app,
		"serial", serial,
	), nil
}

// deviceSleep is a cancellable time.Sleep. Python sleeps unconditionally; on
// shutdown Go reports the cancellation rather than issuing another adb call
// that is certain to fail.
func deviceSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// deviceForegroundApp is actions._foreground_app(): ensure a device, then read
// the focused window. Key order package, activity, raw is contract.
func deviceForegroundApp(ctx context.Context, client *adb.Client) (*jsonresult.Obj, error) {
	if _, err := client.EnsureDevice(ctx); err != nil {
		return nil, err
	}
	app, err := client.CurrentApp(ctx)
	if err != nil {
		return nil, err
	}
	return deviceAppObj(app), nil
}

func deviceAppObj(app adb.AppInfo) *jsonresult.Obj {
	return jsonresult.Of("package", app.Package, "activity", app.Activity, "raw", app.Raw)
}
