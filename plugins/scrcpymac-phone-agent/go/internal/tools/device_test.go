package tools

// device_test.go — the device/introspection tool group, driven over a real MCP
// session with a fake adb Runner. No adb binary and no device are involved.
//
// The contract assertions read docs/contract.json directly, so a drift between
// the registered surface and the frozen Python contract fails here rather than
// in Codex.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

// deviceToolNames is the group this file owns, in docs/contract.json order.
var deviceToolNames = []string{
	"phone_backend",
	"phone_doctor",
	"phone_list_devices",
	"phone_device_info",
	"phone_screenshot",
	"phone_shell",
	"phone_current_app",
	"phone_launch_app",
}

const deviceTestSerial = "2f019965"

// ---------------------------------------------------------------------------
// Fake adb
// ---------------------------------------------------------------------------

// deviceFake is an adb.Runner that answers from canned output and records every
// invocation. It deliberately fails a `shell` call carrying more than one
// argument, because that is what host-side splitting of a device command would
// look like — pipes and redirection would silently stop working on the phone.
type deviceFake struct {
	mu    sync.Mutex
	argvs [][]string
	calls [][]string

	PNG     []byte
	Devices string
	Screen  string
	Dumpsys string

	// Override answers first when it returns true; otherwise the default
	// behaviour applies.
	Override func(args []string) (adb.Output, bool)
}

func newDeviceFake(t *testing.T) *deviceFake {
	t.Helper()
	return &deviceFake{
		PNG:     devicePNGFixture(t, 1080, 2280),
		Devices: "List of devices attached\r\n" + deviceTestSerial + "\tdevice product:OnePlus6 model:ONEPLUS_A6003 device:OnePlus6 transport_id:1\r\n\r\n",
		Screen:  "Physical size: 1080x2280\r\n",
		Dumpsys: "  mCurrentFocus=Window{2c1a1f5 u0 com.tencent.mm/com.tencent.mm.ui.LauncherUI}\r\n",
	}
}

func (f *deviceFake) RunADB(_ context.Context, argv []string, _ time.Duration) (adb.Output, error) {
	args := argv[1:]
	if len(args) >= 2 && args[0] == "-s" {
		args = args[2:]
	}

	f.mu.Lock()
	f.argvs = append(f.argvs, append([]string(nil), argv...))
	f.calls = append(f.calls, append([]string(nil), args...))
	override := f.Override
	f.mu.Unlock()

	if override != nil {
		if out, handled := override(args); handled {
			return out, nil
		}
	}
	return f.defaultOutput(args), nil
}

func (f *deviceFake) defaultOutput(args []string) adb.Output {
	if len(args) == 0 {
		return adb.Output{Stderr: []byte("fake adb: no arguments"), ExitCode: 127}
	}
	switch args[0] {
	case "devices":
		return adb.Output{Stdout: []byte(f.Devices)}
	case "version":
		return adb.Output{Stdout: []byte("Android Debug Bridge version 1.0.41\r\nVersion 37.0.0-14910828\r\n")}
	case "exec-out":
		return adb.Output{Stdout: f.PNG}
	case "shell":
		if len(args) != 2 {
			return adb.Output{
				Stderr:   []byte("fake adb: shell command was split into multiple argv elements"),
				ExitCode: 9,
			}
		}
		command := args[1]
		switch {
		case command == adbScreenSizeCommandForTest():
			return adb.Output{Stdout: []byte(f.Screen)}
		case strings.HasPrefix(command, "dumpsys window"):
			return adb.Output{Stdout: []byte(f.Dumpsys)}
		default:
			// Echo it back so a test can assert the exact device-side command.
			return adb.Output{Stdout: []byte(command + "\r\n")}
		}
	}
	return adb.Output{Stderr: []byte("fake adb: unhandled " + strings.Join(args, " ")), ExitCode: 127}
}

func adbScreenSizeCommandForTest() string {
	return "wm size; dumpsys window displays | grep 'init=' || true"
}

// Calls returns the recorded invocations with the adb path and -s serial
// stripped.
func (f *deviceFake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// FullArgv returns the complete recorded command lines.
func (f *deviceFake) FullArgv() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.argvs))
	copy(out, f.argvs)
	return out
}

// ShellCommands returns the device-side command of every `adb shell` call.
func (f *deviceFake) ShellCommands() []string {
	var out []string
	for _, call := range f.Calls() {
		if len(call) == 2 && call[0] == "shell" {
			out = append(out, call[1])
		}
	}
	return out
}

// devicePNGFixture builds a real PNG so image/png decoding is exercised.
func devicePNGFixture(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xff})
	var buf strings.Builder
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png fixture: %v", err)
	}
	return []byte(buf.String())
}

// ---------------------------------------------------------------------------
// Test server
// ---------------------------------------------------------------------------

// deviceTestSession builds a server carrying only this file's tools, swaps the
// shared adb client's Runner for the fake, and connects a client over the
// in-memory transport — the same code path stdio uses, so an AddTool panic or a
// schema mistake fails here.
func deviceTestSession(t *testing.T) (*mcp.ClientSession, *deviceFake) {
	t.Helper()

	// A resolvable but never-executed adb: SetRunner replaces the backend, so
	// the path only has to satisfy adb.ResolvePath.
	stub := filepath.Join(t.TempDir(), "adb")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write adb stub: %v", err)
	}
	t.Setenv(adb.PathEnv, stub)
	t.Setenv(adb.SerialEnv, "")

	// No stream unless a test installs one; hooks must not leak between tests.
	SetDeviceStreamStatus(nil)
	deviceResetStateHooks(t)

	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{
		Name:  "phone-device",
		Order: mcpserver.OrderPhoneTools,
		Apply: registerPhoneDevice,
	})

	server, err := mcpserver.New(context.Background(), mcpserver.Options{Registry: registry})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	t.Cleanup(server.Env.Shutdown)

	fake := newDeviceFake(t)
	client, err := server.Env.ADB()
	if err != nil {
		t.Fatalf("env.ADB: %v", err)
	}
	client.SetRunner(fake)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCP.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	session, err := mcp.NewClient(&mcp.Implementation{Name: "device-test", Version: "0"}, nil).
		Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session, fake
}

// deviceLiveSession is deviceTestSession without the fake: it talks to whatever
// adb resolves to, against whatever device is plugged in. Only
// TestLiveDeviceReadOnlyTools uses it, and only when PHONE_AGENT_LIVE_DEVICE=1.
func deviceLiveSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	SetDeviceStreamStatus(nil)
	deviceResetStateHooks(t)

	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{
		Name:  "phone-device",
		Order: mcpserver.OrderPhoneTools,
		Apply: registerPhoneDevice,
	})
	server, err := mcpserver.New(context.Background(), mcpserver.Options{Registry: registry})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	t.Cleanup(server.Env.Shutdown)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCP.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	session, err := mcp.NewClient(&mcp.Implementation{Name: "device-live", Version: "0"}, nil).
		Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// deviceResetStateHooks clears the state-change hooks and restores them after
// the test, so a hook registered here cannot affect another test.
func deviceResetStateHooks(t *testing.T) {
	t.Helper()
	deviceHooksMu.Lock()
	saved := deviceStateHooks
	deviceStateHooks = nil
	deviceHooksMu.Unlock()
	t.Cleanup(func() {
		deviceHooksMu.Lock()
		deviceStateHooks = saved
		deviceHooksMu.Unlock()
		SetDeviceStreamStatus(nil)
	})
}

// ---------------------------------------------------------------------------
// Result helpers
// ---------------------------------------------------------------------------

func deviceCall(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if args == nil {
		// A typed nil map is not a nil interface, so ClientSession.CallTool's
		// "avoid sending nil over the wire" guard does not catch it and the
		// server receives `"arguments": null`. jsonschema-go v0.4.3 then applies
		// defaults to a nil map and PANICS, which takes the process down — see
		// the migration followups. Send {} like a real client does.
		args = map[string]any{}
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// deviceText returns the first content block's text.
func deviceText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("result has no content blocks")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text
}

// deviceShapeA asserts the Shape A invariant — the text block is the BARE
// payload while structuredContent is {"result": "<that same text>"} — and
// returns the payload text.
func deviceShapeA(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	text := deviceText(t, res)
	if len(res.Content) != 1 {
		t.Fatalf("want exactly 1 content block, got %d", len(res.Content))
	}
	if res.IsError {
		t.Fatalf("isError must stay false; failure lives inside the JSON: %s", text)
	}
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want an object", res.StructuredContent)
	}
	if got := structured["result"]; got != text {
		t.Fatalf("structuredContent.result != content[0].text\n got: %v\nwant: %v", got, text)
	}
	return text
}

// deviceKeyOrder returns the top-level keys of a JSON object in wire order.
// Key order is contract: Python dicts serialise in insertion order.
func deviceKeyOrder(t *testing.T, text string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		t.Fatalf("payload is not a JSON object: %q", text)
	}
	keys := []string{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key: %v", err)
		}
		name, ok := key.(string)
		if !ok {
			t.Fatalf("object key is %T", key)
		}
		keys = append(keys, name)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("skip value of %q: %v", name, err)
		}
	}
	return keys
}

func deviceWantKeys(t *testing.T, text string, want ...string) {
	t.Helper()
	got := deviceKeyOrder(t, text)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload key order\n got: %v\nwant: %v\npayload:\n%s", got, want, text)
	}
}

func deviceDecode(t *testing.T, text string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal %q: %v", text, err)
	}
	return payload
}

// ---------------------------------------------------------------------------
// Contract
// ---------------------------------------------------------------------------

type deviceContractTool struct {
	Name               string          `json:"name"`
	Title              *string         `json:"title"`
	Description        string          `json:"description"`
	InputSchema        json.RawMessage `json:"inputSchema"`
	OutputSchema       json.RawMessage `json:"outputSchema"`
	Annotations        json.RawMessage `json:"annotations"`
	Meta               json.RawMessage `json:"meta"`
	ResultKeys         []string        `json:"resultKeys"`
	ResultKeysFallback []string        `json:"resultKeysFallback"`
}

func deviceContract(t *testing.T) map[string]deviceContractTool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contract.json"))
	if err != nil {
		t.Fatalf("read contract.json: %v", err)
	}
	var file struct {
		Tools []deviceContractTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse contract.json: %v", err)
	}
	byName := make(map[string]deviceContractTool, len(file.Tools))
	for _, tool := range file.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range deviceToolNames {
		if _, ok := byName[name]; !ok {
			t.Fatalf("contract.json has no entry for %s", name)
		}
	}
	return byName
}

// deviceSameJSON compares two JSON documents structurally. Key order inside a
// schema is not part of the contract; the set of keys and values is.
func deviceSameJSON(t *testing.T, label string, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s: unmarshal got: %v (%s)", label, err, got)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("%s: unmarshal want: %v (%s)", label, err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("%s mismatch\n got: %s\nwant: %s", label, got, want)
	}
}

func TestDeviceToolsMatchContract(t *testing.T) {
	session, _ := deviceTestSession(t)
	contract := deviceContract(t)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	byName := make(map[string]*mcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	if len(listed.Tools) != len(deviceToolNames) {
		t.Fatalf("registered %d tools, want %d: %v", len(listed.Tools), len(deviceToolNames), byName)
	}

	for _, name := range deviceToolNames {
		want := contract[name]
		got, ok := byName[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}

		if got.Description != want.Description {
			t.Errorf("%s description\n got: %q\nwant: %q", name, got.Description, want.Description)
		}
		if want.Title == nil && got.Title != "" {
			t.Errorf("%s title = %q, want none", name, got.Title)
		}

		gotSchema, err := json.Marshal(got.InputSchema)
		if err != nil {
			t.Fatalf("%s: marshal inputSchema: %v", name, err)
		}
		deviceSameJSON(t, name+" inputSchema", gotSchema, want.InputSchema)

		if string(want.OutputSchema) == "null" || len(want.OutputSchema) == 0 {
			if got.OutputSchema != nil {
				t.Errorf("%s declares an outputSchema; the contract has none", name)
			}
		} else {
			gotOutput, err := json.Marshal(got.OutputSchema)
			if err != nil {
				t.Fatalf("%s: marshal outputSchema: %v", name, err)
			}
			deviceSameJSON(t, name+" outputSchema", gotOutput, want.OutputSchema)
		}

		// Every tool in this group has annotations:null and _meta:null. _meta in
		// particular is what hides the app-only tools from the model, so an
		// accidental one here would be a visibility change.
		if string(want.Annotations) == "null" && got.Annotations != nil {
			t.Errorf("%s has annotations %+v; the contract has none", name, got.Annotations)
		}
		if string(want.Meta) == "null" && len(got.Meta) != 0 {
			t.Errorf("%s has _meta %v; the contract has none", name, got.Meta)
		}
	}
}

// deviceContractKeys returns the contract's expected key order, dropping keys
// the Go port deliberately does not emit.
func deviceContractKeys(t *testing.T, name string, drop ...string) []string {
	t.Helper()
	tool := deviceContract(t)[name]
	keys := []string{}
	for _, key := range tool.ResultKeys {
		skip := false
		for _, d := range drop {
			if key == d {
				skip = true
			}
		}
		if !skip {
			keys = append(keys, key)
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// phone_backend
// ---------------------------------------------------------------------------

func TestPhoneBackendReportsAdbWithoutAStream(t *testing.T) {
	session, _ := deviceTestSession(t)

	text := deviceShapeA(t, deviceCall(t, session, "phone_backend", nil))
	if want := "{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"; text != want {
		t.Fatalf("payload\n got: %q\nwant: %q", text, want)
	}
	// "ok" is appended LAST here because the action payload does not carry it.
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_backend")...)
}

func TestPhoneBackendReportsPluginH264WhileStreaming(t *testing.T) {
	session, _ := deviceTestSession(t)
	SetDeviceStreamStatus(func() (DeviceStreamStatus, bool) {
		return DeviceStreamStatus{Serial: deviceTestSerial}, true
	})

	text := deviceShapeA(t, deviceCall(t, session, "phone_backend", nil))
	if got := deviceDecode(t, text)["backend"]; got != "plugin-h264" {
		t.Fatalf("backend = %v, want plugin-h264", got)
	}
}

// ---------------------------------------------------------------------------
// phone_list_devices
// ---------------------------------------------------------------------------

func TestPhoneListDevices(t *testing.T) {
	session, fake := deviceTestSession(t)

	text := deviceShapeA(t, deviceCall(t, session, "phone_list_devices", nil))
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_list_devices")...)

	want := "{\n  \"devices\": [\n    {\n      \"serial\": \"" + deviceTestSerial +
		"\",\n      \"state\": \"device\",\n      \"model\": \"ONEPLUS_A6003\",\n      \"product\": \"OnePlus6\"\n    }\n  ],\n  \"ok\": true\n}"
	if text != want {
		t.Fatalf("payload\n got: %s\nwant: %s", text, want)
	}

	// devices() uses the client directly: no ensure_device, so listing works
	// with nothing connected.
	if calls := fake.Calls(); len(calls) != 1 || !reflect.DeepEqual(calls[0], []string{"devices", "-l"}) {
		t.Fatalf("adb calls = %v, want one `devices -l`", calls)
	}
}

func TestPhoneListDevicesEmitsAnEmptyArrayNotNull(t *testing.T) {
	session, fake := deviceTestSession(t)
	fake.Devices = "List of devices attached\r\n\r\n"

	text := deviceShapeA(t, deviceCall(t, session, "phone_list_devices", nil))
	if !strings.Contains(text, `"devices": []`) {
		t.Fatalf("empty device list must serialise as [], got:\n%s", text)
	}
}

func TestPhoneListDevicesFailureShape(t *testing.T) {
	session, fake := deviceTestSession(t)
	fake.Override = func(args []string) (adb.Output, bool) {
		if args[0] == "devices" {
			return adb.Output{Stderr: []byte("adb server version doesn't match"), ExitCode: 1}, true
		}
		return adb.Output{}, false
	}

	text := deviceShapeA(t, deviceCall(t, session, "phone_list_devices", nil))
	deviceWantKeys(t, text, "ok", "error")
	payload := deviceDecode(t, text)
	if payload["ok"] != false {
		t.Fatalf("ok = %v, want false", payload["ok"])
	}
	// The message template is model-visible: only the args, never the adb path.
	if want := "adb devices -l failed: adb server version doesn't match"; payload["error"] != want {
		t.Fatalf("error\n got: %v\nwant: %v", payload["error"], want)
	}
}

// ---------------------------------------------------------------------------
// phone_device_info
// ---------------------------------------------------------------------------

func TestPhoneDeviceInfoAdbPath(t *testing.T) {
	session, _ := deviceTestSession(t)

	text := deviceShapeA(t, deviceCall(t, session, "phone_device_info", nil))
	// The adb branch is contract.json's resultKeysFallback: no "video" key.
	deviceWantKeys(t, text, "serial", "screen", "foreground", "backend", "ok")

	payload := deviceDecode(t, text)
	if payload["backend"] != "adb" {
		t.Fatalf("backend = %v, want adb", payload["backend"])
	}
	screen := payload["screen"].(map[string]any)
	if screen["width"] != float64(1080) || screen["height"] != float64(2280) {
		t.Fatalf("screen = %v", screen)
	}
	foreground := payload["foreground"].(map[string]any)
	if foreground["package"] != "com.tencent.mm" || foreground["activity"] != "com.tencent.mm.ui.LauncherUI" {
		t.Fatalf("foreground = %v", foreground)
	}
	if _, ok := payload["video"]; ok {
		t.Fatalf("the adb branch must not carry a video object: %s", text)
	}
}

func TestPhoneDeviceInfoStreamingPath(t *testing.T) {
	session, _ := deviceTestSession(t)
	SetDeviceStreamStatus(func() (DeviceStreamStatus, bool) {
		return DeviceStreamStatus{
			Serial:       deviceTestSerial,
			DeviceWidth:  1080,
			DeviceHeight: 2280,
			FrameWidth:   540,
			FrameHeight:  1140,
			FPS:          60,
			Codec:        "avc1.42E01E",
		}, true
	})

	text := deviceShapeA(t, deviceCall(t, session, "phone_device_info", nil))
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_device_info")...)

	// fps is a Python float: 60.0 must not collapse to 60.
	if !strings.Contains(text, `"fps": 60.0`) {
		t.Fatalf("fps must keep its float form:\n%s", text)
	}
	payload := deviceDecode(t, text)
	if payload["backend"] != "plugin-h264" {
		t.Fatalf("backend = %v, want plugin-h264", payload["backend"])
	}
	video := payload["video"].(map[string]any)
	if video["width"] != float64(540) || video["codec"] != "avc1.42E01E" {
		t.Fatalf("video = %v", video)
	}
	if !strings.Contains(text, "\"video\": {\n    \"width\": 540,\n    \"height\": 1140,\n    \"fps\": 60.0,\n    \"codec\": \"avc1.42E01E\"\n  }") {
		t.Fatalf("video sub-object key order changed:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// phone_screenshot
// ---------------------------------------------------------------------------

func TestPhoneScreenshotIncludesTheImageByDefault(t *testing.T) {
	session, fake := deviceTestSession(t)

	// No arguments at all: the schema default (true) must be applied.
	res := deviceCall(t, session, "phone_screenshot", nil)
	if res.IsError {
		t.Fatalf("isError must stay false: %s", deviceText(t, res))
	}
	if res.StructuredContent != nil {
		t.Fatalf("phone_screenshot must not emit structuredContent: %#v", res.StructuredContent)
	}
	if len(res.Content) != 2 {
		t.Fatalf("want text+image content blocks, got %d", len(res.Content))
	}

	text := deviceText(t, res)
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_screenshot")...)
	payload := deviceDecode(t, text)
	if _, ok := payload["base64"]; ok {
		t.Fatalf("the base64 copy must be omitted when the image block is present:\n%s", text)
	}
	if payload["format"] != "png" || payload["size_bytes"] != float64(len(fake.PNG)) {
		t.Fatalf("payload = %v", payload)
	}
	if payload["width"] != float64(1080) || payload["height"] != float64(2280) {
		t.Fatalf("width/height must come from the PNG header: %v", payload)
	}

	img, ok := res.Content[1].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("content[1] is %T, want *mcp.ImageContent", res.Content[1])
	}
	if img.MIMEType != "image/png" {
		t.Fatalf("mimeType = %q", img.MIMEType)
	}
	// Raw bytes: a double-base64 bug shows up as a mismatch here.
	if !reflect.DeepEqual(img.Data, fake.PNG) {
		t.Fatalf("image data is not the captured PNG (%d vs %d bytes)", len(img.Data), len(fake.PNG))
	}
}

func TestPhoneScreenshotUsesPNGDimensionsWhenDisplaySizeDiffers(t *testing.T) {
	session, fake := deviceTestSession(t)
	fake.PNG = devicePNGFixture(t, 720, 1520)
	fake.Screen = "Physical size: 1080x2280\r\nOverride size: 720x1520\r\n"

	payload := deviceDecode(t, deviceText(t, deviceCall(t, session, "phone_screenshot", nil)))
	if payload["width"] != float64(720) || payload["height"] != float64(1520) {
		t.Fatalf("screenshot metadata = %vx%v, want PNG size 720x1520",
			payload["width"], payload["height"])
	}
	for _, call := range fake.Calls() {
		if len(call) == 2 && call[0] == "shell" && call[1] == adbScreenSizeCommandForTest() {
			t.Fatal("phone_screenshot must not replace PNG dimensions with wm/dumpsys dimensions")
		}
	}
}

func TestPhoneScreenshotRejectsAnInvalidPNG(t *testing.T) {
	session, fake := deviceTestSession(t)
	fake.PNG = []byte("not a png")

	res := deviceCall(t, session, "phone_screenshot", nil)
	if len(res.Content) != 1 {
		t.Fatalf("invalid PNG must return one error text block, got %d", len(res.Content))
	}
	payload := deviceDecode(t, deviceText(t, res))
	if payload["ok"] != false || !strings.Contains(payload["error"].(string), "could not decode screenshot PNG") {
		t.Fatalf("payload = %v", payload)
	}
}

func TestPhoneScreenshotWithoutImageCarriesBase64(t *testing.T) {
	session, fake := deviceTestSession(t)

	res := deviceCall(t, session, "phone_screenshot", map[string]any{"include_image": false})
	if len(res.Content) != 1 {
		t.Fatalf("want a single text block, got %d", len(res.Content))
	}
	if res.StructuredContent != nil {
		t.Fatalf("phone_screenshot must not emit structuredContent: %#v", res.StructuredContent)
	}

	text := deviceText(t, res)
	// base64 is inserted BEFORE the ok that _ok() appends last.
	deviceWantKeys(t, text, "serial", "width", "height", "format", "size_bytes", "base64", "ok")

	encoded, ok := deviceDecode(t, text)["base64"].(string)
	if !ok {
		t.Fatalf("base64 is missing:\n%s", text)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 does not decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, fake.PNG) {
		t.Fatalf("base64 does not round-trip to the captured PNG")
	}
}

func TestPhoneScreenshotFailureIsTextOnly(t *testing.T) {
	session, fake := deviceTestSession(t)
	fake.Override = func(args []string) (adb.Output, bool) {
		if args[0] == "exec-out" {
			return adb.Output{Stderr: []byte("closed"), ExitCode: 1}, true
		}
		return adb.Output{}, false
	}

	res := deviceCall(t, session, "phone_screenshot", nil)
	if res.IsError {
		t.Fatalf("failure must keep isError false")
	}
	if len(res.Content) != 1 {
		t.Fatalf("failure must emit no image block, got %d content blocks", len(res.Content))
	}
	text := deviceText(t, res)
	deviceWantKeys(t, text, "ok", "error")
	if want := "adb exec-out screencap -p failed: closed"; deviceDecode(t, text)["error"] != want {
		t.Fatalf("error\n got: %v\nwant: %v", deviceDecode(t, text)["error"], want)
	}
}

func TestDevicePNGSize(t *testing.T) {
	data := devicePNGFixture(t, 12, 34)
	width, height, err := devicePNGSize(data)
	if err != nil {
		t.Fatalf("devicePNGSize: %v", err)
	}
	if width != 12 || height != 34 {
		t.Fatalf("size = %dx%d, want 12x34", width, height)
	}
	if _, _, err := devicePNGSize([]byte("not a png")); err == nil {
		t.Fatal("a corrupt capture must be reported, not silently accepted")
	}
}

// ---------------------------------------------------------------------------
// phone_shell
// ---------------------------------------------------------------------------

func TestPhoneShellPassesTheCommandAsOneArgvElement(t *testing.T) {
	session, fake := deviceTestSession(t)

	command := "dumpsys battery | grep level; echo done"
	text := deviceShapeA(t, deviceCall(t, session, "phone_shell", map[string]any{"command": command}))
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_shell")...)

	payload := deviceDecode(t, text)
	if payload["output"] != command {
		t.Fatalf("output = %v, want the echoed command %q", payload["output"], command)
	}
	if payload["serial"] != deviceTestSerial {
		t.Fatalf("serial = %v", payload["serial"])
	}

	var shellArgv []string
	for _, argv := range fake.FullArgv() {
		if len(argv) >= 2 && argv[len(argv)-2] == "shell" {
			shellArgv = argv
		}
	}
	if shellArgv == nil {
		t.Fatalf("no shell invocation recorded: %v", fake.FullArgv())
	}
	// [adb, -s, serial, shell, <command>] — the command must arrive whole, or
	// the device shell never sees the pipe and the semicolon.
	if len(shellArgv) != 5 || shellArgv[1] != "-s" || shellArgv[3] != "shell" || shellArgv[4] != command {
		t.Fatalf("shell argv = %q", shellArgv)
	}
}

func TestPhoneShellKeepsRawUTF8AndHTMLCharacters(t *testing.T) {
	session, fake := deviceTestSession(t)
	// The device echoes whatever it is asked to run, so this exercises the
	// serialiser: Go escapes < > & by default and Python does not.
	command := `echo "<a href='x'>你好 & 再见</a>"`
	fake.Override = func(args []string) (adb.Output, bool) {
		if args[0] == "shell" && args[1] == command {
			return adb.Output{Stdout: []byte("<a href='x'>你好 & 再见</a>\r\n")}, true
		}
		return adb.Output{}, false
	}

	text := deviceShapeA(t, deviceCall(t, session, "phone_shell", map[string]any{"command": command}))
	if !strings.Contains(text, "<a href='x'>你好 & 再见</a>") {
		t.Fatalf("payload must carry raw UTF-8 and unescaped < > &:\n%s", text)
	}
	// Nothing in this payload legitimately needs escaping, so any backslash at
	// all means the serialiser HTML-escaped < > & or ASCII-escaped the Chinese
	// — the two ways Go's defaults diverge from
	// json.dumps(..., ensure_ascii=False).
	if strings.ContainsRune(text, '\\') {
		t.Fatalf("payload carries a JSON escape sequence; want raw UTF-8 and unescaped < > &:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// phone_current_app
// ---------------------------------------------------------------------------

func TestPhoneCurrentApp(t *testing.T) {
	session, _ := deviceTestSession(t)

	text := deviceShapeA(t, deviceCall(t, session, "phone_current_app", nil))
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_current_app")...)

	// foreground key order is package, activity, raw.
	if !strings.Contains(text, "\"foreground\": {\n    \"package\": \"com.tencent.mm\",\n    \"activity\": \"com.tencent.mm.ui.LauncherUI\",\n    \"raw\": ") {
		t.Fatalf("foreground shape changed:\n%s", text)
	}
	payload := deviceDecode(t, text)
	if payload["serial"] != deviceTestSerial {
		t.Fatalf("serial = %v", payload["serial"])
	}
	foreground := payload["foreground"].(map[string]any)
	// raw is the trimmed dumpsys line, with CRLF already normalised.
	if raw := foreground["raw"].(string); !strings.HasPrefix(raw, "mCurrentFocus=") || strings.ContainsAny(raw, "\r") {
		t.Fatalf("raw = %q", raw)
	}
}

func TestPhoneCurrentAppReportsAnEmptyForegroundWithoutFailing(t *testing.T) {
	session, fake := deviceTestSession(t)
	fake.Dumpsys = "\r\n"

	text := deviceShapeA(t, deviceCall(t, session, "phone_current_app", nil))
	foreground := deviceDecode(t, text)["foreground"].(map[string]any)
	if foreground["package"] != "" || foreground["activity"] != "" || foreground["raw"] != "" {
		t.Fatalf("an unmatched dumpsys line must yield empty strings, got %v", foreground)
	}
}

func TestDeviceSelectedSerialPrecedence(t *testing.T) {
	t.Setenv(adb.SerialEnv, "from-env")
	deviceResetStateHooks(t)

	client := adb.NewWithRunner("", "/nonexistent/adb", adb.RunnerFunc(
		func(context.Context, []string, time.Duration) (adb.Output, error) {
			return adb.Output{}, nil
		}))
	// NewWithRunner picks up PHONE_AGENT_SERIAL for an empty serial, which is
	// the third precedence step; clear it to test the fallback explicitly.
	client.SetSerial("")

	if got := deviceSelectedSerial(client); got != "from-env" {
		t.Fatalf("with no stream and no client serial: %q, want from-env", got)
	}

	client.SetSerial("from-client")
	if got := deviceSelectedSerial(client); got != "from-client" {
		t.Fatalf("the client serial must win over the environment: %q", got)
	}

	SetDeviceStreamStatus(func() (DeviceStreamStatus, bool) {
		return DeviceStreamStatus{Serial: "from-stream"}, true
	})
	if got := deviceSelectedSerial(client); got != "from-stream" {
		t.Fatalf("the streaming serial must win: %q", got)
	}
}

// ---------------------------------------------------------------------------
// phone_launch_app
// ---------------------------------------------------------------------------

func deviceFastLaunch(t *testing.T) {
	t.Helper()
	saved := deviceLaunchSettle
	deviceLaunchSettle = time.Millisecond
	t.Cleanup(func() { deviceLaunchSettle = saved })
}

func TestPhoneLaunchAppWithoutActivity(t *testing.T) {
	deviceFastLaunch(t)
	session, fake := deviceTestSession(t)

	hooks := 0
	RegisterDeviceStateChangeHook(func() { hooks++ })

	text := deviceShapeA(t, deviceCall(t, session, "phone_launch_app", map[string]any{"package": "com.tencent.mm"}))
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_launch_app")...)

	// activity is JSON null for the "" default (server.py passes `activity or None`).
	if !strings.Contains(text, "\"activity\": null") {
		t.Fatalf("activity must be null for the default:\n%s", text)
	}
	payload := deviceDecode(t, text)
	if payload["action"] != "launch" || payload["package"] != "com.tencent.mm" {
		t.Fatalf("payload = %v", payload)
	}

	want := "monkey -p com.tencent.mm -c android.intent.category.LAUNCHER 1"
	if got := fake.ShellCommands(); len(got) == 0 || got[0] != want {
		t.Fatalf("launch command\n got: %v\nwant: %q", got, want)
	}
	if hooks != 1 {
		t.Fatalf("state-change hooks fired %d times, want 1", hooks)
	}
}

func TestPhoneLaunchAppWithActivity(t *testing.T) {
	deviceFastLaunch(t)
	session, fake := deviceTestSession(t)

	text := deviceShapeA(t, deviceCall(t, session, "phone_launch_app", map[string]any{
		"package":  "com.tencent.mm",
		"activity": "com.tencent.mm.ui.LauncherUI",
	}))
	if got := deviceDecode(t, text)["activity"]; got != "com.tencent.mm.ui.LauncherUI" {
		t.Fatalf("activity = %v", got)
	}

	want := "am start -n com.tencent.mm/com.tencent.mm.ui.LauncherUI"
	if got := fake.ShellCommands(); len(got) == 0 || got[0] != want {
		t.Fatalf("launch command\n got: %v\nwant: %q", got, want)
	}
}

func TestPhoneLaunchAppQuotesAHostileArgument(t *testing.T) {
	deviceFastLaunch(t)
	session, fake := deviceTestSession(t)

	deviceCall(t, session, "phone_launch_app", map[string]any{"package": "a; rm -rf /sdcard"})

	want := `monkey -p 'a; rm -rf /sdcard' -c android.intent.category.LAUNCHER 1`
	if got := fake.ShellCommands(); len(got) == 0 || got[0] != want {
		t.Fatalf("hostile package must be quoted\n got: %v\nwant: %q", got, want)
	}
}

func TestPhoneLaunchAppFailureShape(t *testing.T) {
	deviceFastLaunch(t)
	session, fake := deviceTestSession(t)
	fake.Override = func(args []string) (adb.Output, bool) {
		if args[0] == "shell" && strings.HasPrefix(args[1], "monkey ") {
			return adb.Output{Stderr: []byte("No activities found to run"), ExitCode: 1}, true
		}
		return adb.Output{}, false
	}

	text := deviceShapeA(t, deviceCall(t, session, "phone_launch_app", map[string]any{"package": "com.nope"}))
	deviceWantKeys(t, text, "ok", "error")
	if got := deviceDecode(t, text)["error"]; !strings.HasSuffix(got.(string), "failed: No activities found to run") {
		t.Fatalf("error = %v", got)
	}
}

// ---------------------------------------------------------------------------
// phone_doctor
// ---------------------------------------------------------------------------

func TestPhoneDoctor(t *testing.T) {
	session, _ := deviceTestSession(t)

	text := deviceShapeA(t, deviceCall(t, session, "phone_doctor", nil))
	// "ok" is FIRST: phone_doctor bypasses the _ok() wrapper entirely. The Go
	// port drops the top-level uv_available key, which reported a Python
	// installer.
	deviceWantKeys(t, text, deviceContractKeys(t, "phone_doctor", "uv_available")...)

	payload := deviceDecode(t, text)
	checks, ok := payload["checks"].([]any)
	if !ok || len(checks) == 0 {
		t.Fatalf("checks = %v", payload["checks"])
	}
	first := checks[0].(map[string]any)
	for _, key := range []string{"name", "ok", "detail"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("check object is missing %q: %v", key, first)
		}
	}
	// The backend vocabulary is what the user reads; "adb" or "plugin-h264-ready".
	switch payload["backend"] {
	case "adb", "plugin-h264-ready":
	default:
		t.Fatalf("backend = %v, want adb or plugin-h264-ready", payload["backend"])
	}
	// Em dashes in the device messages must survive as raw UTF-8.
	if strings.Contains(text, `—`) {
		t.Fatalf("doctor payload escaped its em dash:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// Live device
// ---------------------------------------------------------------------------

// TestLiveDeviceReadOnlyTools exercises the read-only half of the group against
// a real phone. It is opt-in — set PHONE_AGENT_LIVE_DEVICE=1 — because CI has no
// device.
//
// Only read-only tools run: nothing is launched, no forward is created and no
// process is started, so it is safe to run while a scrcpy session is live on the
// same device. adb contention (a concurrent stream restarting the daemon) shows
// up as ok:false, which is reported as a skip rather than a failure; a wrong
// payload SHAPE still fails.
func TestLiveDeviceReadOnlyTools(t *testing.T) {
	if os.Getenv("PHONE_AGENT_LIVE_DEVICE") != "1" {
		t.Skip("set PHONE_AGENT_LIVE_DEVICE=1 to run against the attached phone")
	}
	if _, err := adb.ResolvePath(); err != nil {
		t.Skipf("no adb: %v", err)
	}
	session := deviceLiveSession(t)

	// live returns the payload, or skips when the device is momentarily busy.
	live := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res := deviceCall(t, session, name, args)
		var text string
		if name == "phone_screenshot" {
			// Shape B: one text block, never structuredContent.
			if res.StructuredContent != nil {
				t.Fatalf("phone_screenshot emitted structuredContent: %#v", res.StructuredContent)
			}
			text = deviceText(t, res)
		} else {
			text = deviceShapeA(t, res)
		}
		payload := deviceDecode(t, text)
		if payload["ok"] != true {
			t.Skipf("%s reported ok:false (adb contention?): %v", name, payload["error"])
		}
		// A full-resolution screenshot is a couple of megabytes of base64;
		// logging it whole would bury every other line.
		if len(text) > 4096 {
			text = text[:4096] + "... (truncated)"
		}
		t.Logf("%s -> %s", name, text)
		return payload
	}

	if got := live("phone_backend", nil)["backend"]; got != "adb" && got != "plugin-h264" {
		t.Fatalf("backend = %v", got)
	}

	doctor := live("phone_doctor", nil)
	if _, ok := doctor["checks"].([]any); !ok {
		t.Fatalf("doctor checks = %v", doctor["checks"])
	}

	devices, ok := live("phone_list_devices", nil)["devices"].([]any)
	if !ok {
		t.Fatalf("devices is not a list")
	}
	if len(devices) == 0 {
		t.Skip("no device attached")
	}

	info := live("phone_device_info", nil)
	screen, ok := info["screen"].(map[string]any)
	if !ok {
		t.Fatalf("screen = %v", info["screen"])
	}
	if width, _ := screen["width"].(float64); width <= 0 {
		t.Fatalf("screen width = %v", screen["width"])
	}

	if _, ok := live("phone_current_app", nil)["foreground"].(map[string]any); !ok {
		t.Fatal("foreground is not an object")
	}

	const marker = "scrcpymac-go-live-check"
	if got := live("phone_shell", map[string]any{"command": "echo " + marker})["output"]; got != marker {
		t.Fatalf("shell output = %v, want %q", got, marker)
	}

	shot := live("phone_screenshot", map[string]any{"include_image": false})
	encoded, ok := shot["base64"].(string)
	if !ok {
		t.Fatal("screenshot carries no base64")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	width, height, err := devicePNGSize(decoded)
	if err != nil {
		t.Fatalf("captured bytes are not a PNG: %v", err)
	}
	t.Logf("captured %dx%d PNG, %d bytes (tool reports %vx%v)",
		width, height, len(decoded), shot["width"], shot["height"])
	if shot["width"] != float64(width) || shot["height"] != float64(height) {
		t.Fatalf("screenshot metadata %vx%v does not match PNG %dx%d",
			shot["width"], shot["height"], width, height)
	}
	if float64(len(decoded)) != shot["size_bytes"] {
		t.Fatalf("size_bytes = %v, want %d", shot["size_bytes"], len(decoded))
	}
}
