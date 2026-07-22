package mcpserver_test

// Adversarial pass over the whole registered surface.
//
// Every tool is called with hostile arguments against an adb that always fails,
// and the session must survive all of it: no panic, no hang, no result that
// breaks the shapes the widget and the model parse. A tool that panics takes the
// process with it — main's deferred Env.Shutdown never runs, so the scrcpy
// process, the adb forwards and the loopback port all leak.
//
// SAFETY: a real Android device may be attached while this runs. ADB_PATH is
// pointed at a stub that fails on every invocation and PHONE_AGENT_SERIAL at a
// serial that cannot exist, and the test refuses to run unless resolution
// actually landed on the stub. Nothing here can reach a phone.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

// stubADB writes an executable that fails every invocation and returns its path.
func stubADB(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "adb")
	script := "#!/bin/sh\necho 'stub adb: refusing to talk to a device' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub adb: %v", err)
	}
	return path
}

// hostileSession builds a server whose adb can only fail.
func hostileSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	stub := stubADB(t)
	t.Setenv(adb.PathEnv, stub)
	t.Setenv(adb.AndroidADBEnv, stub)
	t.Setenv(adb.SerialEnv, "no-such-device-0000")
	t.Setenv("ANDROID_SERIAL", "no-such-device-0000")

	// Refuse to run against a real adb: every tool below issues device commands.
	resolved, err := adb.ResolvePath()
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if resolved != stub {
		t.Fatalf("adb resolved to %q, not the stub %q; refusing to drive a real device", resolved, stub)
	}

	server, err := mcpserver.New(context.Background(), mcpserver.Options{
		LoopbackPort: 0,
		Log:          mcpserver.NewLogger(os.Stderr),
	})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	// Deliberately no Env.Shutdown: the scrcpy group registers Close on the
	// PROCESS-WIDE runtime and a closed runtime never binds a listener again.
	// Nothing was started, so nothing needs releasing.

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCP.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	session, err := mcp.NewClient(&mcp.Implementation{Name: "robustness", Version: "0"}, nil).
		Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// hostileArguments are the argument sets every tool is called with. A tool
// ignores the keys it does not declare; the schema rejects the ones whose type
// is wrong, which is itself a path worth exercising.
func hostileArguments() []map[string]any {
	long := strings.Repeat("字", 4096)
	return []map[string]any{
		{},
		{"x": 0, "y": 0},
		{"x": -2147483648, "y": -2147483648, "x1": -1, "y1": -1, "x2": -1, "y2": -1},
		{"x": 2147483647, "y": 2147483647, "x1": 2147483647, "y1": 2147483647,
			"x2": 2147483647, "y2": 2147483647},
		{"x": 0.5, "y": 0.5, "x1": 0.0, "y1": 0.0, "x2": 1.0, "y2": 1.0},
		{"x": 1e308, "y": -1e308},
		{"image_width": 0, "image_height": 0, "x": 0, "y": 0},
		{"image_width": -5, "image_height": -5, "x": 0, "y": 0},
		{"name": ""},
		{"name": "'; rm -rf /; #"},
		{"name": long},
		{"text": ""},
		{"text": "你好 $(whoami) `id` && rm -rf / ; echo 'x' \"y\" \\ | tee"},
		{"text": long},
		{"command": "echo '<&>' | cat"},
		{"package": "com.example; rm -rf /", "activity": ".Main`id`"},
		{"contact": "", "message": ""},
		{"contact": "张三", "message": "你好 & <b> 'quoted'"},
		{"host": "", "port": 0},
		{"host": "1.2.3.4:9999", "port": 70000},
		{"host": strings.Repeat("a", 2048)},
		{"serial": ""},
		{"serial": "no-such-device-0000"},
		{"duration_ms": -1}, {"duration_ms": 1 << 40},
		{"max_width": -1, "quality": -1},
		{"max_width": 1 << 30, "quality": 1 << 30},
		{"compact": false}, {"compact": true},
		{"timeout_s": 0}, {"timeout_s": -1}, {"timeout_s": 0.0001},
		{"text": "needle", "timeout_s": 0},
		{"index": -1, "scroll_to_find": -1, "text": "needle", "timeout_s": 0},
		{"display_mode": "nonsense"},
		{"include_image": false},
		{"retries": -100, "verify": true, "x": 1, "y": 1},
		{"retries": 100, "verify": true, "x": 1, "y": 1},
	}
}

// TestEveryToolSurvivesHostileArguments is the panic/hang sweep.
func TestEveryToolSurvivesHostileArguments(t *testing.T) {
	session := hostileSession(t)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("no tools registered")
	}

	for _, tool := range listed.Tools {
		for i, args := range hostileArguments() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			started := time.Now()
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
			elapsed := time.Since(started)
			timedOut := ctx.Err() != nil
			cancel()

			if timedOut {
				t.Errorf("%s with argument set %d did not return within 30s (took %v)",
					tool.Name, i, elapsed)
				continue
			}
			if err != nil {
				// A schema violation is a legitimate protocol error; a panic that
				// the recover middleware turned into an error is not.
				if strings.Contains(err.Error(), "internal error handling") {
					t.Errorf("%s with argument set %d PANICKED: %v", tool.Name, i, err)
				}
				continue
			}
			assertWellFormedResult(t, tool, res, i)
		}
	}

	// The session must still be usable after all of that.
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("the session died during the sweep: %v", err)
	}
}

// assertWellFormedResult checks the two invariants every result shape shares:
// there is at least one content block, and a declared outputSchema is matched by
// structuredContent that actually carries the payload text.
func assertWellFormedResult(t *testing.T, tool *mcp.Tool, res *mcp.CallToolResult, set int) {
	t.Helper()
	if len(res.Content) == 0 {
		t.Errorf("%s argument set %d: no content blocks", tool.Name, set)
		return
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Errorf("%s argument set %d: first content block is %T, want text", tool.Name, set, res.Content[0])
		return
	}

	if tool.OutputSchema == nil {
		return
	}
	if res.IsError {
		// A schema rejection: the SDK replaces the result with the validation
		// message and drops structuredContent. FastMCP did the same for a
		// pydantic failure, so this is the matching shape, not a regression.
		return
	}
	// Shape A: content[0].text is the bare payload and structuredContent is
	// {"result": "<that same text>"}. Both halves are contract; the widget and
	// Codex's output validation read different ones.
	var payload map[string]any
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Errorf("%s argument set %d: text block is not JSON: %v\n%s", tool.Name, set, err, text.Text)
		return
	}
	if _, hasOK := payload["ok"]; !hasOK {
		t.Errorf("%s argument set %d: payload has no \"ok\" key: %s", tool.Name, set, text.Text)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Errorf("%s argument set %d: structuredContent will not marshal: %v", tool.Name, set, err)
		return
	}
	var wrapper struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Errorf("%s argument set %d: structuredContent is not the {result} wrapper: %v", tool.Name, set, err)
		return
	}
	if wrapper.Result != text.Text {
		t.Errorf("%s argument set %d: structuredContent.result differs from the text block", tool.Name, set)
	}
}

// TestFailingADBNeverLosesTheErrorText checks the other half of the contract:
// when adb cannot run, the model must still receive {"ok": false, "error": ...}
// with the message intact, and isError must stay false — failure lives in the
// JSON body, exactly as it did in Python.
func TestFailingADBNeverLosesTheErrorText(t *testing.T) {
	session := hostileSession(t)

	for _, name := range []string{
		"phone_list_devices", "phone_device_info", "phone_current_app",
		"phone_shell", "phone_get_device_ip", "phone_ui_tree",
	} {
		args := map[string]any{}
		if name == "phone_shell" {
			args["command"] = "echo hi"
		}
		res, err := session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if res.IsError {
			t.Errorf("%s: isError must stay false; failure belongs in the JSON body", name)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		var payload struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Errorf("%s: %v\n%s", name, err, text)
			continue
		}
		if payload.OK {
			t.Errorf("%s reported ok:true with a broken adb: %s", name, text)
		}
		if payload.Error == "" {
			t.Errorf("%s reported ok:false with no error text: %s", name, text)
		}
		// The stub's stderr must survive into the message the model reads.
		if !strings.Contains(payload.Error, "stub adb") {
			t.Errorf("%s lost the adb error text: %q", name, payload.Error)
		}
	}
}
