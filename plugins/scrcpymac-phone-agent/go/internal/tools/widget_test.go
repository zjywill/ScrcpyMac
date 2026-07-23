package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/widget"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// widgetTestSession builds a server carrying ONLY this file's registrations and
// drives it over the in-memory transport — the same code path stdio uses, so an
// AddTool panic or a malformed schema fails the test instead of the shipped
// binary.
func widgetTestSession(t *testing.T, loopbackPort int) *mcp.ClientSession {
	t.Helper()
	session, _ := widgetTestServer(t, loopbackPort)
	return session
}

// widgetTestServer is widgetTestSession plus the live Env, for the handlers that
// are worth calling directly (the ones whose key ORDER matters: the client
// decodes structuredContent into a Go map, so wire order is not observable
// through a session).
func widgetTestServer(t *testing.T, loopbackPort int) (*mcp.ClientSession, *mcpserver.Env) {
	t.Helper()
	registry := mcpserver.NewRegistry()
	registry.Add(
		mcpserver.Registration{Name: "widget-open", Order: mcpserver.OrderWidgetTool, Apply: registerWidgetOpen},
		mcpserver.Registration{Name: "widget-app", Order: mcpserver.OrderAppTools, Apply: registerWidgetAppTools},
		mcpserver.Registration{Name: "widget-diagnostics", Order: mcpserver.OrderPhoneTools + 90, Apply: registerWidgetDiagnostics},
	)
	server, err := mcpserver.New(context.Background(), mcpserver.Options{
		Registry:     registry,
		LoopbackPort: loopbackPort,
	})
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

	session, err := mcp.NewClient(&mcp.Implementation{Name: "widget-test", Version: "0"}, nil).
		Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, server.Env
}

func widgetTools(t *testing.T, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

// widgetCall invokes a tool. args must never be nil: a nil map marshals to
// "arguments": null, which the SDK's default application panics on — see
// TestNullArgumentsIsAnSDKHazard.
func widgetCall(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return res
}

// widgetStructured decodes structuredContent.
//
// Only the key SET survives the round trip: the SDK hands the payload to
// encoding/json and the client decodes it into a Go map, so wire order is not
// observable here. Key ORDER is asserted against the payload builders directly,
// where it is meaningful.
func widgetStructured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatal("structuredContent is nil; the widget reads it first")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	return decoded
}

// hasExactKeys reports whether payload's key set is exactly want.
func hasExactKeys(payload map[string]any, want []string) bool {
	if len(payload) != len(want) {
		return false
	}
	for _, key := range want {
		if _, ok := payload[key]; !ok {
			return false
		}
	}
	return true
}

func widgetText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content block")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text
}

// fakeStreamRuntime stands in for the scrcpy runtime so the streaming branch of
// every input tool is exercised without a device, adb or scrcpy-server.
type fakeStreamRuntime struct {
	active bool
	fail   error
	calls  []string
	diag   StreamDiagnostics
}

func (f *fakeStreamRuntime) BackendName() string {
	if f.active {
		return "plugin-h264"
	}
	return "adb"
}

func (f *fakeStreamRuntime) IsActive() bool { return f.active }

func (f *fakeStreamRuntime) Status() *jsonresult.Obj {
	state := "idle"
	if f.active {
		state = "streaming"
	}
	return jsonresult.Of(
		"ok", true,
		"state", state,
		"backend", f.BackendName(),
		"encoding", "H.264",
		"error", "",
		"fps", jsonresult.Float(59.9),
		"frames", 1234,
		"serial", "2f019965",
		"deviceWidth", 1080,
		"deviceHeight", 2280,
		"frameWidth", 540,
		"frameHeight", 1140,
	)
}

func (f *fakeStreamRuntime) TapRelative(_ context.Context, x, y float64) (*jsonresult.Obj, error) {
	f.calls = append(f.calls, "tap")
	if f.fail != nil {
		return nil, f.fail
	}
	return jsonresult.Of(
		"ok", true,
		"action", "tap",
		"serial", "2f019965",
		"point", []int{jsonresult.PyRoundInt(x * 539), jsonresult.PyRoundInt(y * 1139)},
		"coordinateSpace", []int{540, 1140},
		"backend", "plugin-control",
	), nil
}

func (f *fakeStreamRuntime) SwipeRelative(_ context.Context, x1, y1, x2, y2 float64, durationMs int) (*jsonresult.Obj, error) {
	f.calls = append(f.calls, "swipe")
	if f.fail != nil {
		return nil, f.fail
	}
	return jsonresult.Of(
		"ok", true,
		"action", "swipe",
		"serial", "2f019965",
		"from", []int{jsonresult.PyRoundInt(x1 * 539), jsonresult.PyRoundInt(y1 * 1139)},
		"to", []int{jsonresult.PyRoundInt(x2 * 539), jsonresult.PyRoundInt(y2 * 1139)},
		"durationMs", durationMs,
		"backend", "plugin-control",
	), nil
}

func (f *fakeStreamRuntime) Key(_ context.Context, name string) (*jsonresult.Obj, error) {
	f.calls = append(f.calls, "key")
	if f.fail != nil {
		return nil, f.fail
	}
	return jsonresult.Of(
		"ok", true, "action", "key", "key", name, "serial", "2f019965", "backend", "plugin-control",
	), nil
}

func (f *fakeStreamRuntime) Paste(_ context.Context, text string) (*jsonresult.Obj, error) {
	f.calls = append(f.calls, "paste")
	if f.fail != nil {
		return nil, f.fail
	}
	return jsonresult.Of(
		"ok", true, "action", "paste", "length", jsonresult.RuneLen(text),
		"serial", "2f019965", "backend", "plugin-control",
	), nil
}

func (f *fakeStreamRuntime) Diagnostics() StreamDiagnostics { return f.diag }

// useFakeRuntime installs a fake for the duration of one test.
func useFakeRuntime(t *testing.T, rt *fakeStreamRuntime) *fakeStreamRuntime {
	t.Helper()
	SetStreamRuntime(rt)
	t.Cleanup(func() { SetStreamRuntime(nil) })
	return rt
}

// ---------------------------------------------------------------------------
// the registered surface
// ---------------------------------------------------------------------------

// widgetAppToolNames are the eight app-only tools this file owns. The other
// three scrcpymac_ui_* tools (start_stream, stream_status, stop_stream) belong
// to the scrcpy runtime's own file.
var widgetAppToolNames = []string{
	"scrcpymac_ui_state",
	"scrcpymac_ui_select_device",
	"scrcpymac_ui_snapshot",
	"scrcpymac_ui_tap",
	"scrcpymac_ui_swipe",
	"scrcpymac_ui_key",
	"scrcpymac_ui_paste",
	"scrcpymac_ui_connect_wifi",
}

func TestWidgetToolsAreRegistered(t *testing.T) {
	tools := widgetTools(t, widgetTestSession(t, 0))
	for _, name := range append([]string{"open_scrcpymac"}, widgetAppToolNames...) {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %s is not registered", name)
		}
	}
	if len(tools) != len(widgetAppToolNames)+1 {
		t.Errorf("registered %d tools, want %d", len(tools), len(widgetAppToolNames)+1)
	}
}

// TestStreamDiagnosticsIsOffByDefault is the guard on the migration's hard
// constraint: phone_stream_status is model-visible, so registering it
// unconditionally would give Codex a 25th phone_* tool the Python server never
// had. docs/contract.json freezes the surface at 37 tools; this keeps it there
// unless a human opts in.
func TestStreamDiagnosticsIsOffByDefault(t *testing.T) {
	t.Setenv(StreamDiagnosticsEnv, "")
	if StreamDiagnosticsEnabled() {
		t.Fatal("phone_stream_status must be opt-in")
	}
	if _, ok := widgetTools(t, widgetTestSession(t, 0))["phone_stream_status"]; ok {
		t.Error("phone_stream_status is registered without PHONE_AGENT_STREAM_DIAGNOSTICS")
	}

	for _, on := range []string{"1", "true", "YES", "On"} {
		t.Setenv(StreamDiagnosticsEnv, on)
		if !StreamDiagnosticsEnabled() {
			t.Errorf("%s=%q must enable the diagnostic", StreamDiagnosticsEnv, on)
		}
	}
	for _, off := range []string{"", "0", "false", "no", "off", "maybe"} {
		t.Setenv(StreamDiagnosticsEnv, off)
		if StreamDiagnosticsEnabled() {
			t.Errorf("%s=%q must NOT enable the diagnostic", StreamDiagnosticsEnv, off)
		}
	}
}

// TestAppToolsAreInvisibleToTheModel is the Go twin of
// tests/test_mcp_ui.py::test_internal_tools_are_app_only. visibility:["app"] is
// the ONLY thing keeping these tools out of Codex's tool list, so if the SDK
// ever stops round-tripping unknown _meta keys the model silently gains eight
// tools it was never meant to call.
func TestAppToolsAreInvisibleToTheModel(t *testing.T) {
	tools := widgetTools(t, widgetTestSession(t, 0))
	for _, name := range widgetAppToolNames {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("%s missing", name)
		}
		ui, ok := tool.Meta["ui"].(map[string]any)
		if !ok {
			t.Errorf("%s: _meta.ui missing or wrong type: %#v", name, tool.Meta)
			continue
		}
		visibility, ok := ui["visibility"].([]any)
		if !ok || len(visibility) != 1 || visibility[0] != "app" {
			t.Errorf("%s: _meta.ui.visibility = %#v, want [\"app\"]", name, ui["visibility"])
		}
		if _, present := ui["resourceUri"]; present {
			t.Errorf("%s: _meta.ui must not carry resourceUri", name)
		}
		if tool.OutputSchema != nil {
			t.Errorf("%s: app tools declare no outputSchema", name)
		}
	}
}

func TestOpenToolMetaPointsAtTheWidget(t *testing.T) {
	tool := widgetTools(t, widgetTestSession(t, 0))["open_scrcpymac"]
	if tool == nil {
		t.Fatal("open_scrcpymac missing")
	}
	ui, ok := tool.Meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui missing: %#v", tool.Meta)
	}
	if ui["resourceUri"] != widget.URI {
		t.Errorf("_meta.ui.resourceUri = %v, want %s", ui["resourceUri"], widget.URI)
	}
	visibility, _ := ui["visibility"].([]any)
	if len(visibility) != 2 || visibility[0] != "model" || visibility[1] != "app" {
		t.Errorf("_meta.ui.visibility = %#v, want [model app]", ui["visibility"])
	}
	if tool.Meta["openai/outputTemplate"] != widget.URI {
		t.Errorf("_meta[openai/outputTemplate] = %v", tool.Meta["openai/outputTemplate"])
	}
	if tool.Meta["openai/widgetAccessible"] != true {
		t.Errorf("_meta[openai/widgetAccessible] = %v, want true", tool.Meta["openai/widgetAccessible"])
	}
	if tool.Meta["openai/toolInvocation/invoking"] != "Opening ScrcpyMac..." {
		t.Errorf("_meta invoking = %v", tool.Meta["openai/toolInvocation/invoking"])
	}
	if tool.Meta["openai/toolInvocation/invoked"] != "ScrcpyMac ready" {
		t.Errorf("_meta invoked = %v", tool.Meta["openai/toolInvocation/invoked"])
	}
	if tool.OutputSchema != nil {
		t.Error("open_scrcpymac declares no outputSchema")
	}
}

// TestToolTitlesAndDescriptions pins the model- and app-visible strings. They
// are contract: Codex renders the title and the model reads the description.
func TestToolTitlesAndDescriptions(t *testing.T) {
	want := map[string][2]string{
		"open_scrcpymac":             {"Open ScrcpyMac", "Open the complete ScrcpyMac phone workspace in Codex."},
		"scrcpymac_ui_state":         {"Read ScrcpyMac UI state", "Return device discovery and backend state for the ScrcpyMac widget."},
		"scrcpymac_ui_select_device": {"Select ScrcpyMac device", "Select the Android serial used by subsequent widget actions."},
		"scrcpymac_ui_snapshot":      {"Capture ScrcpyMac preview frame", "Capture a compressed frame for the ScrcpyMac widget."},
		"scrcpymac_ui_tap":           {"Tap ScrcpyMac preview", "Tap normalized preview coordinates."},
		"scrcpymac_ui_swipe":         {"Swipe ScrcpyMac preview", "Swipe between normalized preview coordinates."},
		"scrcpymac_ui_key":           {"Press ScrcpyMac navigation key", "Press an Android navigation or hardware key."},
		"scrcpymac_ui_paste":         {"Paste text through ScrcpyMac", "Paste text into the focused Android field."},
		"scrcpymac_ui_connect_wifi":  {"Connect ScrcpyMac over Wi-Fi", "Connect adb to an Android device over Wi-Fi."},
	}
	tools := widgetTools(t, widgetTestSession(t, 0))
	for name, pair := range want {
		tool := tools[name]
		if tool == nil {
			t.Errorf("%s missing", name)
			continue
		}
		if tool.Title != pair[0] {
			t.Errorf("%s title = %q, want %q", name, tool.Title, pair[0])
		}
		if tool.Description != pair[1] {
			t.Errorf("%s description = %q, want %q", name, tool.Description, pair[1])
		}
	}
}

// TestInputSchemas pins every parameter name, type, title and default. FastMCP
// derived these from the Python signatures; a drift here is a silent client
// incompatibility, since Codex validates arguments against them.
func TestInputSchemas(t *testing.T) {
	tools := widgetTools(t, widgetTestSession(t, 0))
	cases := []struct {
		tool string
		want string
	}{
		{"open_scrcpymac", `{"type":"object","title":"open_scrcpymacArguments","properties":{"display_mode":{"type":"string","title":"Display Mode","default":"fullscreen"}}}`},
		{"scrcpymac_ui_state", `{"type":"object","title":"scrcpymac_ui_stateArguments","properties":{}}`},
		{"scrcpymac_ui_select_device", `{"type":"object","title":"scrcpymac_ui_select_deviceArguments","required":["serial"],"properties":{"serial":{"type":"string","title":"Serial"}}}`},
		{"scrcpymac_ui_snapshot", `{"type":"object","title":"scrcpymac_ui_snapshotArguments","properties":{"max_width":{"type":"integer","title":"Max Width","default":540},"quality":{"type":"integer","title":"Quality","default":60}}}`},
		{"scrcpymac_ui_tap", `{"type":"object","title":"scrcpymac_ui_tapArguments","required":["x","y"],"properties":{"x":{"type":"number","title":"X"},"y":{"type":"number","title":"Y"}}}`},
		{"scrcpymac_ui_swipe", `{"type":"object","title":"scrcpymac_ui_swipeArguments","required":["x1","y1","x2","y2"],"properties":{"x1":{"type":"number","title":"X1"},"y1":{"type":"number","title":"Y1"},"x2":{"type":"number","title":"X2"},"y2":{"type":"number","title":"Y2"},"duration_ms":{"type":"integer","title":"Duration Ms","default":300}}}`},
		{"scrcpymac_ui_key", `{"type":"object","title":"scrcpymac_ui_keyArguments","required":["name"],"properties":{"name":{"type":"string","title":"Name"}}}`},
		{"scrcpymac_ui_paste", `{"type":"object","title":"scrcpymac_ui_pasteArguments","required":["text"],"properties":{"text":{"type":"string","title":"Text"}}}`},
		{"scrcpymac_ui_connect_wifi", `{"type":"object","title":"scrcpymac_ui_connect_wifiArguments","required":["host"],"properties":{"host":{"type":"string","title":"Host"},"port":{"type":"integer","title":"Port","default":5555}}}`},
	}
	for _, tc := range cases {
		tool := tools[tc.tool]
		if tool == nil {
			t.Errorf("%s missing", tc.tool)
			continue
		}
		got, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Errorf("%s: marshal inputSchema: %v", tc.tool, err)
			continue
		}
		if !jsonEqual(t, got, []byte(tc.want)) {
			t.Errorf("%s inputSchema =\n  %s\nwant\n  %s", tc.tool, got, tc.want)
		}
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	x, _ := json.Marshal(av)
	y, _ := json.Marshal(bv)
	return string(x) == string(y)
}

// TestAnnotations pins the hints. scrcpymac_ui_connect_wifi is the only tool in
// the entire server with openWorldHint: true.
func TestAnnotations(t *testing.T) {
	t.Setenv(StreamDiagnosticsEnv, "1") // phone_stream_status is opt-in
	tools := widgetTools(t, widgetTestSession(t, 0))
	type hints struct{ readOnly, destructive, idempotent, openWorld bool }
	want := map[string]hints{
		"open_scrcpymac":             {true, false, true, false},
		"scrcpymac_ui_state":         {true, false, true, false},
		"scrcpymac_ui_select_device": {false, false, true, false},
		"scrcpymac_ui_snapshot":      {true, false, false, false},
		"scrcpymac_ui_tap":           {false, false, false, false},
		"scrcpymac_ui_swipe":         {false, false, false, false},
		"scrcpymac_ui_key":           {false, false, false, false},
		"scrcpymac_ui_paste":         {false, false, false, false},
		"scrcpymac_ui_connect_wifi":  {false, false, false, true},
		"phone_stream_status":        {true, false, true, false},
	}
	for name, w := range want {
		tool := tools[name]
		if tool == nil || tool.Annotations == nil {
			t.Errorf("%s has no annotations", name)
			continue
		}
		a := tool.Annotations
		if a.ReadOnlyHint != w.readOnly {
			t.Errorf("%s readOnlyHint = %v, want %v", name, a.ReadOnlyHint, w.readOnly)
		}
		if a.IdempotentHint != w.idempotent {
			t.Errorf("%s idempotentHint = %v, want %v", name, a.IdempotentHint, w.idempotent)
		}
		if a.DestructiveHint == nil || *a.DestructiveHint != w.destructive {
			t.Errorf("%s destructiveHint = %v, want %v", name, a.DestructiveHint, w.destructive)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint != w.openWorld {
			t.Errorf("%s openWorldHint = %v, want %v", name, a.OpenWorldHint, w.openWorld)
		}
	}
}

// ---------------------------------------------------------------------------
// the widget resource and the loopback-port ordering
// ---------------------------------------------------------------------------

func TestWidgetResourceIdentity(t *testing.T) {
	session := widgetTestSession(t, 0)
	res, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if len(res.Resources) != 1 {
		t.Fatalf("want exactly 1 resource, got %d", len(res.Resources))
	}
	got := res.Resources[0]
	if got.URI != "ui://widget/scrcpymac/app.html" {
		t.Errorf("uri = %q", got.URI)
	}
	if got.Name != "scrcpymac-app" || got.Title != "ScrcpyMac" {
		t.Errorf("name/title = %q/%q", got.Name, got.Title)
	}
	if got.Description != "Interactive Android mirroring and control workspace." {
		t.Errorf("description = %q", got.Description)
	}
	if got.MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("mimeType = %q", got.MIMEType)
	}

	ui, _ := got.Meta["ui"].(map[string]any)
	if ui == nil || ui["prefersBorder"] != false {
		t.Errorf("_meta.ui = %#v", got.Meta["ui"])
	}
	csp, _ := ui["csp"].(map[string]any)
	if csp == nil {
		t.Fatalf("_meta.ui.csp missing: %#v", ui)
	}
	// camelCase here, snake_case in openai/widgetCSP, identical values. The split
	// is contract, not an inconsistency to tidy up.
	if _, ok := csp["connectDomains"]; !ok {
		t.Errorf("_meta.ui.csp.connectDomains missing: %#v", csp)
	}
	if !equalStrings(toStrings(csp["resourceDomains"]), []string{"data:", "blob:"}) {
		t.Errorf("_meta.ui.csp.resourceDomains = %#v", csp["resourceDomains"])
	}
	openaiCSP, _ := got.Meta["openai/widgetCSP"].(map[string]any)
	if openaiCSP == nil {
		t.Fatalf("_meta[openai/widgetCSP] missing")
	}
	if _, ok := openaiCSP["connect_domains"]; !ok {
		t.Errorf("openai/widgetCSP.connect_domains missing: %#v", openaiCSP)
	}
	if !equalStrings(toStrings(openaiCSP["resource_domains"]), []string{"data:", "blob:"}) {
		t.Errorf("openai/widgetCSP.resource_domains = %#v", openaiCSP["resource_domains"])
	}
	if got.Meta["openai/widgetDescription"] != "Full ScrcpyMac phone control workspace." {
		t.Errorf("openai/widgetDescription = %v", got.Meta["openai/widgetDescription"])
	}
	if got.Meta["openai/widgetPrefersBorder"] != false {
		t.Errorf("openai/widgetPrefersBorder = %v", got.Meta["openai/widgetPrefersBorder"])
	}
}

// TestResourceCSPFollowsALateBoundLoopbackPort is the ordering guarantee.
//
// The Python computes the CSP on the first line of register_scrcpymac_app,
// which works only because its loopback is bound at import time. In Go the
// listener belongs to the scrcpy runtime, which is registered AFTER the
// resource, so a value captured at construction time would always be zero and
// the CSP would name a port the widget never connects to. The connect-domain
// lists are therefore resolved at marshal time from widget.SetLoopbackPort.
func TestResourceCSPFollowsALateBoundLoopbackPort(t *testing.T) {
	session := widgetTestSession(t, 0)

	// Before anything binds: only the four wildcards, and no bogus ":0" entry.
	domains := resourceConnectDomains(t, session)
	if len(domains) != 4 {
		t.Fatalf("unbound: want 4 wildcard domains, got %d: %v", len(domains), domains)
	}
	for _, d := range domains {
		if !strings.HasSuffix(d, ":*") {
			t.Errorf("unbound: %q is not a wildcard", d)
		}
	}

	// Now the runtime binds and publishes its port — after the resource was
	// registered, which is the whole point.
	widget.SetLoopbackPort(51999)
	t.Cleanup(func() { widget.SetLoopbackPort(0) })

	domains = resourceConnectDomains(t, session)
	want := []string{
		"http://127.0.0.1:51999", "ws://127.0.0.1:51999",
		"http://localhost:51999", "ws://localhost:51999",
		"http://127.0.0.1:*", "ws://127.0.0.1:*",
		"http://localhost:*", "ws://localhost:*",
	}
	if !equalStrings(domains, want) {
		t.Errorf("bound: connectDomains =\n  %v\nwant\n  %v", domains, want)
	}

	// resources/read must carry the same _meta as resources/list.
	read, err := session.ReadResource(context.Background(),
		&mcp.ReadResourceParams{URI: "ui://widget/scrcpymac/app.html"})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	ui, _ := read.Meta["ui"].(map[string]any)
	csp, _ := ui["csp"].(map[string]any)
	if !equalStrings(toStrings(csp["connectDomains"]), want) {
		t.Errorf("resources/read _meta connectDomains = %#v", csp["connectDomains"])
	}
	if len(read.Contents) != 1 || len(read.Contents[0].Text) < 1000 {
		t.Fatalf("widget HTML looks wrong: %d contents", len(read.Contents))
	}
	if read.Contents[0].MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("contents mimeType = %q", read.Contents[0].MIMEType)
	}
}

// TestOptionsLoopbackPortStillWins covers the other ordering: a runtime that
// binds before mcpserver.New and passes the port through Options.
func TestOptionsLoopbackPortStillWins(t *testing.T) {
	session := widgetTestSession(t, 51234)
	t.Cleanup(func() { widget.SetLoopbackPort(0) })
	domains := resourceConnectDomains(t, session)
	if len(domains) != 8 || domains[0] != "http://127.0.0.1:51234" {
		t.Errorf("connectDomains = %v", domains)
	}
}

func resourceConnectDomains(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	res, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	ui, _ := res.Resources[0].Meta["ui"].(map[string]any)
	csp, _ := ui["csp"].(map[string]any)
	list := toStrings(csp["connectDomains"])
	// The snake_case block must always carry the identical values.
	openaiCSP, _ := res.Resources[0].Meta["openai/widgetCSP"].(map[string]any)
	if !equalStrings(toStrings(openaiCSP["connect_domains"]), list) {
		t.Errorf("ui.csp.connectDomains and openai/widgetCSP.connect_domains disagree:\n  %v\n  %v",
			list, openaiCSP["connect_domains"])
	}
	return list
}

func toStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// open_scrcpymac
// ---------------------------------------------------------------------------

func TestOpenScrcpymacNormalisesDisplayMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fullscreen", "fullscreen"},
		{"inline", "inline"},
		{"", "fullscreen"},
		{"pip", "fullscreen"},
		{"Inline", "fullscreen"}, // case-sensitive, like the Python set membership
	}
	for _, tc := range cases {
		got, _ := widgetOpenPayload(tc.in).Get("preferredDisplayMode")
		if got != tc.want {
			t.Errorf("display_mode %q -> %v, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpenScrcpymacResultShape(t *testing.T) {
	session := widgetTestSession(t, 0)
	res := widgetCall(t, session, "open_scrcpymac", map[string]any{"display_mode": "inline"})

	if res.IsError {
		t.Error("open_scrcpymac cannot fail")
	}
	if got := widgetText(t, res); got != "Opened the ScrcpyMac widget." {
		t.Errorf("text block = %q", got)
	}
	payload := widgetStructured(t, res)
	wantKeys := []string{"ok", "widget", "preferredDisplayMode", "phase"}
	if !hasExactKeys(payload, wantKeys) {
		t.Errorf("keys = %v, want exactly %v", sortedKeys(payload), wantKeys)
	}
	if payload["ok"] != true || payload["widget"] != "scrcpymac-app" ||
		payload["preferredDisplayMode"] != "inline" || payload["phase"] != "standalone-h264-stream" {
		t.Errorf("payload = %#v", payload)
	}
	// Key order is contract but is not observable through a session, so it is
	// pinned on the builder.
	if !equalStrings(widgetOpenPayload("inline").Keys(), wantKeys) {
		t.Errorf("payload key order = %v, want %v", widgetOpenPayload("inline").Keys(), wantKeys)
	}

	// The default applies when the argument is omitted.
	res = widgetCall(t, session, "open_scrcpymac", map[string]any{})
	payload = widgetStructured(t, res)
	if payload["preferredDisplayMode"] != "fullscreen" {
		t.Errorf("default display_mode = %v, want fullscreen", payload["preferredDisplayMode"])
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSchemaDefaultsReachTheHandler proves the declared defaults are applied by
// the SDK before the handler runs, which is what lets the Go structs omit them.
//
// It also documents a crash that is NOT this package's to fix but that the whole
// binary is exposed to: a client that sends "arguments": null (rather than
// omitting the field or sending {}) makes mcp.applySchema unmarshal null into
// its map, producing a NIL map, and jsonschema-go's ApplyDefaults then panics on
// SetMapIndex. The panic is on the SDK's handler goroutine, before any tool code
// runs, and nothing recovers it — the process dies. It affects every tool that
// declares a schema default, which is most of this server. Hence: always send
// {}, never null.
func TestSchemaDefaultsReachTheHandler(t *testing.T) {
	useFakeRuntime(t, &fakeStreamRuntime{active: true})
	session := widgetTestSession(t, 0)

	// duration_ms omitted -> the schema default of 300 must arrive.
	res := widgetCall(t, session, "scrcpymac_ui_swipe",
		map[string]any{"x1": 0.5, "y1": 0.8, "x2": 0.5, "y2": 0.2})
	if res.IsError {
		t.Fatalf("unexpected isError: %s", widgetText(t, res))
	}
	if got := widgetStructured(t, res)["durationMs"]; got != float64(300) {
		t.Errorf("durationMs = %v, want the schema default 300", got)
	}
}

// ---------------------------------------------------------------------------
// the streaming branch of every input tool
// ---------------------------------------------------------------------------

func TestInputToolsUseTheRuntimeWhileStreaming(t *testing.T) {
	fake := useFakeRuntime(t, &fakeStreamRuntime{active: true})
	session := widgetTestSession(t, 0)

	cases := []struct {
		tool     string
		args     map[string]any
		wantText string
		wantKeys []string
		check    func(t *testing.T, payload map[string]any)
	}{
		{
			tool: "scrcpymac_ui_tap", args: map[string]any{"x": 0.5, "y": 0.25},
			wantText: "Tapped the Android screen.",
			wantKeys: []string{"ok", "action", "serial", "point", "coordinateSpace", "backend"},
			check: func(t *testing.T, p map[string]any) {
				if p["backend"] != "plugin-control" {
					t.Errorf("backend = %v", p["backend"])
				}
				// The adb path's keys must not leak into the streaming payload.
				if _, snake := p["coordinate_space"]; snake {
					t.Error("streaming payload must use coordinateSpace, not coordinate_space")
				}
			},
		},
		{
			tool:     "scrcpymac_ui_swipe",
			args:     map[string]any{"x1": 0.5, "y1": 0.8, "x2": 0.5, "y2": 0.2, "duration_ms": 250},
			wantText: "Swiped the Android screen.",
			wantKeys: []string{"ok", "action", "serial", "from", "to", "durationMs", "backend"},
			check: func(t *testing.T, p map[string]any) {
				// camelCase on this path; the adb path uses duration_ms.
				if _, snake := p["duration_ms"]; snake {
					t.Error("streaming payload must use durationMs, not duration_ms")
				}
				if p["durationMs"] != float64(250) {
					t.Errorf("durationMs = %v", p["durationMs"])
				}
			},
		},
		{
			tool: "scrcpymac_ui_key", args: map[string]any{"name": "back"},
			wantText: "Pressed Android key back.",
			wantKeys: []string{"ok", "action", "key", "serial", "backend"},
			check: func(t *testing.T, p map[string]any) {
				if _, present := p["code"]; present {
					t.Error("the streaming key payload carries no keycode")
				}
			},
		},
		{
			tool: "scrcpymac_ui_paste", args: map[string]any{"text": "你好世界"},
			wantText: "Pasted text into Android.",
			wantKeys: []string{"ok", "action", "length", "serial", "backend"},
			check: func(t *testing.T, p map[string]any) {
				// len() in Python counts code points; four Chinese runes are 12 bytes.
				if p["length"] != float64(4) {
					t.Errorf("length = %v, want 4 runes (not 12 bytes)", p["length"])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			res := widgetCall(t, session, tc.tool, tc.args)
			if res.IsError {
				t.Fatalf("unexpected isError: %s", widgetText(t, res))
			}
			if got := widgetText(t, res); got != tc.wantText {
				t.Errorf("text block = %q, want %q", got, tc.wantText)
			}
			payload := widgetStructured(t, res)
			if !hasExactKeys(payload, tc.wantKeys) {
				t.Errorf("keys = %v, want exactly %v", sortedKeys(payload), tc.wantKeys)
			}
			tc.check(t, payload)
		})
	}

	wantCalls := []string{"tap", "swipe", "key", "paste"}
	if !equalStrings(fake.calls, wantCalls) {
		t.Errorf("runtime calls = %v, want %v (a tool took the adb path while streaming)",
			fake.calls, wantCalls)
	}
}

// TestRuntimeFailureBecomesAStructuredError checks the Shape C error: isError
// true, the bare message as the text block, {"ok":false,"error":msg} as
// structuredContent. Returning a Go error instead would make the SDK discard the
// structured payload entirely.
func TestRuntimeFailureBecomesAStructuredError(t *testing.T) {
	useFakeRuntime(t, &fakeStreamRuntime{active: true, fail: errors.New("scrcpy control send failed: broken pipe")})
	session := widgetTestSession(t, 0)

	res := widgetCall(t, session, "scrcpymac_ui_tap", map[string]any{"x": 0.1, "y": 0.1})
	if !res.IsError {
		t.Error("want isError: true")
	}
	if got := widgetText(t, res); got != "scrcpy control send failed: broken pipe" {
		t.Errorf("text block = %q", got)
	}
	payload := widgetStructured(t, res)
	if !hasExactKeys(payload, []string{"ok", "error"}) {
		t.Errorf("keys = %v, want exactly [ok error]", sortedKeys(payload))
	}
	if payload["ok"] != false || payload["error"] != "scrcpy control send failed: broken pipe" {
		t.Errorf("payload = %#v", payload)
	}
}

// ---------------------------------------------------------------------------
// validation that happens before any device access
// ---------------------------------------------------------------------------

func TestSwipeRejectsOutOfRangeCoordinates(t *testing.T) {
	useFakeRuntime(t, &fakeStreamRuntime{active: true})
	ctx := context.Background()
	cases := []widgetSwipeIn{
		{X1: -0.01, Y1: 0.5, X2: 0.5, Y2: 0.5},
		{X1: 0.5, Y1: 1.01, X2: 0.5, Y2: 0.5},
		{X1: 0.5, Y1: 0.5, X2: 2, Y2: 0.5},
		{X1: 0.5, Y1: 0.5, X2: 0.5, Y2: -1},
	}
	for _, in := range cases {
		_, err := widgetSwipe(ctx, nil, in)
		if err == nil || err.Error() != "swipe coordinates must be between 0 and 1" {
			t.Errorf("%+v: err = %v, want the range message", in, err)
		}
	}
}

func TestSwipeClampsDuration(t *testing.T) {
	useFakeRuntime(t, &fakeStreamRuntime{active: true})
	for _, tc := range []struct{ in, want int }{
		{-5, 0}, {0, 0}, {300, 300}, {10_000, 10_000}, {99_999, 10_000},
	} {
		payload, err := widgetSwipe(context.Background(), nil,
			widgetSwipeIn{X1: 0, Y1: 0, X2: 1, Y2: 1, DurationMs: tc.in})
		if err != nil {
			t.Fatalf("duration %d: %v", tc.in, err)
		}
		if got, _ := payload.Get("durationMs"); got != tc.want {
			t.Errorf("duration %d -> %v, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPasteRejectsEmptyText(t *testing.T) {
	useFakeRuntime(t, &fakeStreamRuntime{active: true})
	_, err := widgetPaste(context.Background(), nil, "")
	if err == nil || err.Error() != "text must not be empty" {
		t.Errorf("err = %v, want \"text must not be empty\"", err)
	}
}

func TestSelectDeviceRejectsEmptySerial(t *testing.T) {
	_, err := widgetSelectDevice(context.Background(), nil, "   ")
	if err == nil || err.Error() != "device serial must not be empty" {
		t.Errorf("err = %v, want \"device serial must not be empty\"", err)
	}
}

// TestUnknownKeyMessage pins the model-visible error, including Python's !r
// quoting and the exact eleven-name list in dict order.
func TestUnknownKeyMessage(t *testing.T) {
	_, err := widgetADBKey(context.Background(), nil, "teleport")
	want := "Unknown key 'teleport'. Supported: back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down, paste"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	if !adb.IsError(err) {
		t.Error("an unknown key must surface as an adb.Error, like Python's AdbError")
	}
}

// TestKeyTablesDiffer documents the asymmetry the contract calls out: "paste"
// works over adb and fails while streaming, because the runtime's own table
// omits it.
func TestKeyTablesDiffer(t *testing.T) {
	if _, ok := widgetKeycodes["paste"]; !ok {
		t.Error("the adb key table must contain paste (keycode 279)")
	}
	if widgetKeycodes["paste"] != 279 {
		t.Errorf("paste keycode = %d, want 279", widgetKeycodes["paste"])
	}
	if len(widgetKeycodes) != len(widgetKeyNames) {
		t.Errorf("%d keycodes but %d names", len(widgetKeycodes), len(widgetKeyNames))
	}
	for _, name := range widgetKeyNames {
		if _, ok := widgetKeycodes[name]; !ok {
			t.Errorf("%q is listed in the error message but has no keycode", name)
		}
	}
}

func TestPyRepr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"back", "'back'"},
		{"", "''"},
		{"it's", `"it's"`},
		{`say "hi"`, `'say "hi"'`},
		{`both ' and "`, `'both \' and "'`},
		{"line\nbreak", `'line\nbreak'`},
		{"tab\there", `'tab\there'`},
		{`back\slash`, `'back\\slash'`},
		{"你好", "'你好'"},
	}
	for _, tc := range cases {
		if got := widgetPyRepr(tc.in); got != tc.want {
			t.Errorf("widgetPyRepr(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestWiFiTarget(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"192.168.1.100", 5555, "192.168.1.100:5555"},
		{"192.168.1.100:4444", 5555, "192.168.1.100:4444"},
		{"phone.local", 5037, "phone.local:5037"},
	}
	for _, tc := range cases {
		if got := widgetWiFiTarget(tc.host, tc.port); got != tc.want {
			t.Errorf("widgetWiFiTarget(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// scrcpymac_ui_state
// ---------------------------------------------------------------------------

// TestStateShapeHoldsOnBothPaths runs the real tool against the real adb
// resolution on this machine. It asserts only what is true whether or not a
// device is attached, so it is safe while another investigation is starting and
// stopping a stream: the key set, that devices is an array and never null, and —
// the part that is easy to get wrong — that a failure is reported with
// isError:false so the widget can still render the backend state.
func TestStateShapeHoldsOnBothPaths(t *testing.T) {
	useFakeRuntime(t, &fakeStreamRuntime{})
	session, env := widgetTestServer(t, 0)

	res := widgetCall(t, session, "scrcpymac_ui_state", map[string]any{})
	if res.IsError {
		t.Error("scrcpymac_ui_state never sets isError, even when adb fails")
	}
	payload := widgetStructured(t, res)

	base := []string{"ok", "backend", "selectedSerial", "devices", "stream"}
	switch payload["ok"] {
	case true:
		if !hasExactKeys(payload, base) {
			t.Errorf("success keys = %v, want exactly %v", sortedKeys(payload), base)
		}
		if widgetText(t, res) != "Read ScrcpyMac device state." {
			t.Errorf("success text block = %q", widgetText(t, res))
		}
	case false:
		if !hasExactKeys(payload, append(append([]string{}, base...), "error")) {
			t.Errorf("failure keys = %v", sortedKeys(payload))
		}
		if widgetText(t, res) != payload["error"] {
			t.Errorf("failure text block must be the bare message, got %q", widgetText(t, res))
		}
	default:
		t.Fatalf("ok = %#v", payload["ok"])
	}

	if _, isArray := payload["devices"].([]any); !isArray {
		t.Errorf("devices = %#v, want a JSON array (never null)", payload["devices"])
	}
	if _, isObject := payload["stream"].(map[string]any); !isObject {
		t.Errorf("stream = %#v, want the runtime status object", payload["stream"])
	}
	if payload["backend"] != "adb" {
		t.Errorf("backend = %v, want adb while the fake runtime is idle", payload["backend"])
	}

	// Key order, pinned on the builder rather than the wire.
	built, text := widgetState(context.Background(), env)
	wantOrder := base
	if ok, _ := built.Get("ok"); ok == false {
		wantOrder = append(append([]string{}, base...), "error")
		if errText, _ := built.Get("error"); errText != text {
			t.Errorf("failure text block = %q, want the bare error %q", text, errText)
		}
	}
	if !equalStrings(built.Keys(), wantOrder) {
		t.Errorf("payload key order = %v, want %v", built.Keys(), wantOrder)
	}
}

// ---------------------------------------------------------------------------
// snapshot payload
// ---------------------------------------------------------------------------

// TestPreviewPayloadPassesEncodedFramesThrough covers _preview_frame's
// image_bytes branch: an already-encoded frame is forwarded untouched, keeping
// its own mimeType and frame size.
func TestPreviewPayloadPassesEncodedFramesThrough(t *testing.T) {
	frame := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}
	payload, err := widgetPreviewPayload(widgetShot{
		Serial: "2f019965", Backend: "plugin-h264",
		Width: 1080, Height: 2280,
		Image: frame, MIMEType: "image/webp",
		FrameWidth: 540, FrameHeight: 1140,
	}, 540, 60)
	if err != nil {
		t.Fatalf("widgetPreviewPayload: %v", err)
	}
	wantKeys := []string{"ok", "serial", "backend", "deviceWidth", "deviceHeight",
		"frameWidth", "frameHeight", "mimeType", "dataBase64", "sizeBytes"}
	if !equalStrings(payload.Keys(), wantKeys) {
		t.Errorf("key order = %v, want %v", payload.Keys(), wantKeys)
	}
	if got, _ := payload.Get("mimeType"); got != "image/webp" {
		t.Errorf("mimeType = %v, want the shot's own type", got)
	}
	if got, _ := payload.Get("sizeBytes"); got != len(frame) {
		t.Errorf("sizeBytes = %v, want %d", got, len(frame))
	}
	encoded, _ := payload.Get("dataBase64")
	decoded, err := base64.StdEncoding.DecodeString(encoded.(string))
	if err != nil || string(decoded) != string(frame) {
		t.Errorf("dataBase64 does not round-trip: %v %v", decoded, err)
	}
}

// TestPreviewPayloadEncodesPNG covers the normal adb path end to end and pins
// the two things the pytest asserts: the frame is downscaled to max_width and
// the base64 never appears outside dataBase64.
func TestPreviewPayloadEncodesPNG(t *testing.T) {
	shot := widgetShot{
		Serial: "2f019965", Backend: "adb",
		Width: 1080, Height: 2280,
		PNG: makeTestPNG(t, 1080, 2280),
	}
	payload, err := widgetPreviewPayload(shot, 540, 70)
	if err != nil {
		t.Fatalf("widgetPreviewPayload: %v", err)
	}
	if got, _ := payload.Get("frameWidth"); got != 540 {
		t.Errorf("frameWidth = %v, want 540", got)
	}
	if got, _ := payload.Get("frameHeight"); got != 1140 {
		t.Errorf("frameHeight = %v, want 1140", got)
	}
	if got, _ := payload.Get("deviceWidth"); got != 1080 {
		t.Errorf("deviceWidth = %v, want the source PNG width", got)
	}
	if got, _ := payload.Get("mimeType"); got != "image/jpeg" {
		t.Errorf("mimeType = %v", got)
	}

	// The text block is a sentence; the base64 lives only in structuredContent.
	_, _, err2 := Structured(payload, "Captured a ScrcpyMac preview frame.")
	if err2 != nil {
		t.Fatalf("Structured: %v", err2)
	}
	encoded, _ := payload.Get("dataBase64")
	if strings.Contains("Captured a ScrcpyMac preview frame.", encoded.(string)) {
		t.Error("the base64 must never reach the text block")
	}
}

func TestSnapshotClamps(t *testing.T) {
	// The clamps are applied before capture, so they are observable without a
	// device by checking the resulting frame width for a known source size.
	for _, tc := range []struct {
		maxWidth  int
		wantWidth int
	}{
		{0, 320},     // below the floor
		{100, 320},   // below the floor
		{540, 540},   // the default
		{9000, 1080}, // above the ceiling, but the source is only 1080 wide
	} {
		clamped := Clamp(tc.maxWidth, widgetSnapshotMinMaxWidth, widgetSnapshotMaxMaxWidth)
		w, _ := widgetPreviewSize(1080, 2280, clamped)
		if w != tc.wantWidth {
			t.Errorf("max_width %d -> frame width %d, want %d", tc.maxWidth, w, tc.wantWidth)
		}
	}
	for _, tc := range []struct{ in, want int }{
		{0, 45}, {44, 45}, {60, 60}, {90, 90}, {1000, 90},
	} {
		if got := Clamp(tc.in, widgetSnapshotMinQuality, widgetSnapshotMaxQuality); got != tc.want {
			t.Errorf("quality %d -> %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// phone_stream_status
// ---------------------------------------------------------------------------

func TestStreamStatusIsModelVisibleAndShapeA(t *testing.T) {
	t.Setenv(StreamDiagnosticsEnv, "1")
	session := widgetTestSession(t, 0)
	tool := widgetTools(t, session)["phone_stream_status"]
	if tool == nil {
		t.Fatal("phone_stream_status is not registered")
	}
	if tool.Meta != nil {
		t.Errorf("phone_stream_status must carry no _meta (app-only visibility would hide it): %#v", tool.Meta)
	}
	if tool.OutputSchema == nil {
		t.Error("phone_* tools declare the {\"result\": string} output schema")
	}

	res := widgetCall(t, session, "phone_stream_status", map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected isError: %s", widgetText(t, res))
	}
	// Shape A: the text block is the BARE payload and structuredContent wraps the
	// same text under "result".
	text := widgetText(t, res)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("text block is not the bare payload: %v\n%s", err, text)
	}
	if payload["state"] != "idle" {
		t.Errorf("state = %v, want idle with no runtime installed", payload["state"])
	}
	structured := widgetStructured(t, res)
	if structured["result"] != text {
		t.Error("structuredContent.result must be the same JSON text as the content block")
	}
}

func TestStreamStatusPayload(t *testing.T) {
	diag := StreamDiagnostics{
		State: "streaming", Backend: "plugin-h264", Serial: "2f019965",
		Codec: "avc1.42E01E", DeviceWidth: 1080, DeviceHeight: 2280,
		FrameWidth: 540, FrameHeight: 1140,
		UptimeS: 12.34, Packets: 900, Frames: 880, KeyFrames: 9, Bytes: 4_500_000,
		PacketsPerSecond: 60.04, FramesPerSecond: 59.96,
		DroppedGOPs: 2, DroppedPackets: 41,
		Clients: []StreamClientDiagnostics{{
			ID: "c1", ConnectedS: 9.87, QueueDepth: 3, QueueCapacity: 4096,
			PacketsSent: 870, BytesSent: 4_400_000, DroppedGOPs: 2, DroppedPackets: 41,
			WaitingForKeyFrame: true,
		}},
	}
	payload := widgetStreamStatusPayload(diag)
	wantOrder := []string{
		"ok", "state", "backend", "serial", "codec",
		"deviceWidth", "deviceHeight", "frameWidth", "frameHeight",
		"uptimeS", "packets", "frames", "keyFrames", "bytes",
		"packetsPerSecond", "framesPerSecond",
		"droppedGops", "droppedPackets", "clientCount", "clients", "error",
	}
	if !equalStrings(payload.Keys(), wantOrder) {
		t.Errorf("key order = %v\nwant       %v", payload.Keys(), wantOrder)
	}

	text := jsonresult.Text(payload)
	// Rates round to one decimal and keep Python float formatting.
	for _, want := range []string{
		`"uptimeS": 12.3`, `"packetsPerSecond": 60.0`, `"framesPerSecond": 60.0`,
		`"droppedGops": 2`, `"clientCount": 1`, `"waitingForKeyFrame": true`,
		`"queueDepth": 3`, `"queueCapacity": 4096`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("payload is missing %s:\n%s", want, text)
		}
	}
}

// TestStreamStatusEmptyClientsIsAnArray guards the nil-slice trap: a nil Go
// slice marshals to null, and the model would read that as "unknown" rather than
// "nobody is watching".
func TestStreamStatusEmptyClientsIsAnArray(t *testing.T) {
	payload := widgetStreamStatusPayload(StreamDiagnostics{State: "idle", Backend: "adb"})
	text := jsonresult.Text(payload)
	if !strings.Contains(text, `"clients": []`) {
		t.Errorf("clients must serialise as [], not null:\n%s", text)
	}
	if !strings.Contains(text, `"framesPerSecond": 0.0`) {
		t.Errorf("a Python float 0.0 must not become 0:\n%s", text)
	}
}

// TestStreamStatusHint checks the derived diagnosis, which is the reason this
// tool exists: the model should be told which side of the relay is at fault.
func TestStreamStatusHint(t *testing.T) {
	cases := []struct {
		name    string
		diag    StreamDiagnostics
		wantSub string
	}{
		{"idle has no hint", StreamDiagnostics{State: "idle"}, ""},
		{
			"healthy has no hint",
			StreamDiagnostics{State: "streaming", Frames: 500, FramesPerSecond: 58},
			"",
		},
		{
			"slow device",
			StreamDiagnostics{State: "streaming", Frames: 30, FramesPerSecond: 1.1},
			"upstream of the relay",
		},
		{
			"full client queue wins over the device diagnosis",
			StreamDiagnostics{
				State: "streaming", Frames: 30, FramesPerSecond: 1.1,
				Clients: []StreamClientDiagnostics{{QueueDepth: 4096, QueueCapacity: 4096}},
			},
			"send queue is full",
		},
		{
			"no frames yet",
			StreamDiagnostics{State: "streaming"},
			"No frames have arrived",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := widgetStreamHint(tc.diag)
			if tc.wantSub == "" {
				if got != "" {
					t.Errorf("want no hint, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("hint = %q, want it to mention %q", got, tc.wantSub)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the runtime seam
// ---------------------------------------------------------------------------

func TestIdleRuntimeIsAlwaysInstalled(t *testing.T) {
	SetStreamRuntime(nil)
	rt := ActiveStreamRuntime()
	if rt == nil {
		t.Fatal("ActiveStreamRuntime must never return nil")
	}
	if rt.IsActive() {
		t.Error("the idle runtime is never active")
	}
	if rt.BackendName() != "adb" {
		t.Errorf("backend = %q, want adb", rt.BackendName())
	}
	status := rt.Status()
	wantKeys := []string{"ok", "state", "backend", "encoding", "error", "fps", "frames"}
	if !equalStrings(status.Keys(), wantKeys) {
		t.Errorf("status keys = %v, want %v", status.Keys(), wantKeys)
	}
	if !strings.Contains(jsonresult.Text(status), `"fps": 0.0`) {
		t.Errorf("fps must render as a Python float:\n%s", jsonresult.Text(status))
	}
	for _, err := range []error{
		errFrom(rt.TapRelative(context.Background(), 0, 0)),
		errFrom(rt.SwipeRelative(context.Background(), 0, 0, 1, 1, 100)),
		errFrom(rt.Key(context.Background(), "back")),
		errFrom(rt.Paste(context.Background(), "x")),
	} {
		if !errors.Is(err, ErrStreamNotRunning) {
			t.Errorf("idle control call err = %v, want ErrStreamNotRunning", err)
		}
	}
}

func errFrom(_ *jsonresult.Obj, err error) error { return err }

func TestDeviceChangeHookFires(t *testing.T) {
	useFakeRuntime(t, &fakeStreamRuntime{active: true})

	fired := 0
	WidgetOnDeviceChange(func() { fired++ })
	t.Cleanup(func() {
		widgetDeviceChangeMu.Lock()
		widgetDeviceChangeFns = nil
		widgetDeviceChangeMu.Unlock()
	})

	ctx := context.Background()
	if _, err := widgetTap(ctx, nil, 0.5, 0.5); err != nil {
		t.Fatalf("tap: %v", err)
	}
	if _, err := widgetKey(ctx, nil, "home"); err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := widgetPaste(ctx, nil, "hi"); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if fired != 3 {
		t.Errorf("hook fired %d times, want 3 (the ui_tree cache would go stale)", fired)
	}
}

func TestObjHelpers(t *testing.T) {
	obj := jsonresult.Of("s", "hello", "i", 42, "i64", int64(7), "f", 3.9, "b", true)
	if got := widgetObjString(obj, "s"); got != "hello" {
		t.Errorf("widgetObjString = %q", got)
	}
	if got := widgetObjString(obj, "missing"); got != "" {
		t.Errorf("missing string = %q, want empty", got)
	}
	if got := widgetObjString(obj, "i"); got != "" {
		t.Errorf("wrong-typed string = %q, want empty", got)
	}
	if got := widgetObjInt(obj, "i"); got != 42 {
		t.Errorf("widgetObjInt(int) = %d", got)
	}
	if got := widgetObjInt(obj, "i64"); got != 7 {
		t.Errorf("widgetObjInt(int64) = %d", got)
	}
	if got := widgetObjInt(obj, "f"); got != 3 {
		t.Errorf("widgetObjInt(float64) = %d, want a truncation to 3", got)
	}
	if got := widgetObjInt(obj, "b"); got != 0 {
		t.Errorf("widgetObjInt(bool) = %d, want 0", got)
	}
	if got := widgetObjInt(nil, "i"); got != 0 {
		t.Errorf("widgetObjInt(nil) = %d", got)
	}
}
