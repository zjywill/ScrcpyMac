package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

// defaultedSchema has an optional property with a schema default. The default is
// what makes jsonschema-go run ApplyDefaults, which is where the nil-map panic
// lives — a tool with no defaults never reaches it.
func defaultedSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:          "object",
		Title:         "defaultedArguments",
		PropertyOrder: []string{"mode"},
		Properties: map[string]*jsonschema.Schema{
			"mode": {Type: "string", Title: "Mode", Default: json.RawMessage(`"fullscreen"`)},
		},
	}
}

// modeArgs is the typed input of the test tool. The generic mcp.AddTool is what
// runs the schema through applySchema/ApplyDefaults; a raw ToolHandler skips
// both, so it would not exercise the bug at all.
type modeArgs struct {
	Mode string `json:"mode,omitempty"`
}

type modeHandler func(context.Context, *mcp.CallToolRequest, modeArgs) (*mcp.CallToolResult, any, error)

// newHardenedSession builds a server with an isolated registry plus one tool of
// the caller's choosing, so these tests exercise the middleware rather than any
// real tool's behaviour, and returns a connected client session.
func newHardenedSession(t *testing.T, name string, handler modeHandler) *mcp.ClientSession {
	t.Helper()

	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{
		Name:  "test",
		Order: mcpserver.OrderPhoneTools,
		Apply: func(s *mcp.Server, _ *mcpserver.Env) error {
			mcp.AddTool(s, &mcp.Tool{Name: name, Description: "test", InputSchema: defaultedSchema()},
				mcp.ToolHandlerFor[modeArgs, any](handler))
			return nil
		},
	})

	var logs bytes.Buffer
	server, err := mcpserver.New(context.Background(), mcpserver.Options{
		Registry: registry,
		Log:      mcpserver.NewLogger(&logs),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() && logs.Len() > 0 {
			t.Logf("server log:\n%s", logs.String())
		}
	})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.MCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestNullArgumentsDoNotKillTheServer pins the fix for a process-killing SDK
// bug: go-sdk v1.6.1 applySchema unmarshals an explicit `"arguments": null` into
// a NIL map[string]any, and jsonschema-go v0.4.3 ApplyDefaults then calls
// SetMapIndex on it. Before mcpserver's normalising middleware this panicked on
// an SDK goroutine, so the whole binary died — taking the scrcpy process, the
// adb forwards and the loopback listener with it, since main's deferred
// Env.Shutdown never ran.
//
// The SDK's own client produces exactly this request: CallToolParams.Arguments
// is `any` with omitempty, so a typed nil map marshals to null rather than being
// omitted.
func TestNullArgumentsDoNotKillTheServer(t *testing.T) {
	var seen modeArgs
	session := newHardenedSession(t, "defaulted",
		func(_ context.Context, _ *mcp.CallToolRequest, in modeArgs) (*mcp.CallToolResult, any, error) {
			seen = in
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})

	var nilMap map[string]any
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "defaulted",
		Arguments: nilMap, // marshals to `"arguments": null`
	})
	if err != nil {
		t.Fatalf("tools/call with null arguments: %v", err)
	}
	if res.IsError {
		t.Fatalf("tools/call reported an error: %+v", res.Content)
	}

	// The schema default must still be applied — the middleware turns null into
	// "absent", which is the case the SDK already handles correctly.
	if seen.Mode != "fullscreen" {
		t.Errorf("handler received mode %q, want the schema default applied", seen.Mode)
	}
}

// TestOmittedAndEmptyArgumentsStillGetDefaults guards the shapes that already
// worked, so the normaliser cannot regress them.
func TestOmittedAndEmptyArgumentsStillGetDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		args any
		want string
	}{
		{"omitted", nil, "fullscreen"},
		{"empty object", map[string]any{}, "fullscreen"},
		{"explicit value", map[string]any{"mode": "pip"}, "pip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen modeArgs
			session := newHardenedSession(t, "defaulted",
				func(_ context.Context, _ *mcp.CallToolRequest, in modeArgs) (*mcp.CallToolResult, any, error) {
					seen = in
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
				})
			if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "defaulted",
				Arguments: tc.args,
			}); err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if seen.Mode != tc.want {
				t.Errorf("handler received mode %q, want %q", seen.Mode, tc.want)
			}
		})
	}
}

// TestPanicInAToolIsContained: a panic on the SDK's handler goroutine kills the
// process and skips every cleanup, because main's deferred Env.Shutdown is on a
// different stack. One failed call is strictly better, and the session must stay
// usable afterwards.
func TestPanicInAToolIsContained(t *testing.T) {
	calls := 0
	session := newHardenedSession(t, "boom",
		func(_ context.Context, _ *mcp.CallToolRequest, _ modeArgs) (*mcp.CallToolResult, any, error) {
			calls++
			if calls == 1 {
				panic("kaboom")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "second call ok"}}}, nil, nil
		})

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "boom"})
	if err == nil {
		t.Fatal("a panicking tool must surface as an error, not as success")
	}
	if !strings.Contains(err.Error(), "internal error handling tools/call") {
		t.Errorf("error = %v, want it to name the failed method", err)
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "boom"})
	if err != nil {
		t.Fatalf("the session died with the panicking call: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("second call returned no content")
	}
}
