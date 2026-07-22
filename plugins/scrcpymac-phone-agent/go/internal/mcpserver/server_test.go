package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/widget"
)

// newTestServer builds a server with an isolated registry so a test never
// depends on whatever internal/tools registers.
func newTestServer(t *testing.T, registry *Registry, loopbackPort int) *Server {
	t.Helper()
	if registry == nil {
		registry = NewRegistry()
	}
	server, err := New(context.Background(), Options{Registry: registry, LoopbackPort: loopbackPort})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// connect drives the server over an in-memory transport, the same code path the
// stdio transport uses. It is what turns an AddTool/AddResource panic into a
// test failure instead of a broken shipped binary.
func connect(t *testing.T, server *Server) *mcp.ClientSession {
	t.Helper()
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

func TestServerStartsAndListsFeatures(t *testing.T) {
	server := newTestServer(t, nil, 0)
	session := connect(t, server)

	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	res, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if len(res.Resources) != 1 {
		t.Fatalf("want exactly 1 resource, got %d", len(res.Resources))
	}
	got := res.Resources[0]
	if got.URI != widget.URI {
		t.Errorf("resource URI = %q, want %q", got.URI, widget.URI)
	}
	if got.Name != "scrcpymac-app" || got.Title != "ScrcpyMac" {
		t.Errorf("resource identity = %q/%q", got.Name, got.Title)
	}
	if got.MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("resource mimeType = %q", got.MIMEType)
	}
	if _, ok := got.Meta["openai/widgetCSP"]; !ok {
		t.Errorf("resource _meta lost openai/widgetCSP: %v", got.Meta)
	}
}

func TestReadWidgetResource(t *testing.T) {
	server := newTestServer(t, nil, 51234)
	session := connect(t, server)

	res, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: widget.URI})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("want 1 content block, got %d", len(res.Contents))
	}
	if !strings.Contains(res.Contents[0].Text, "<") || len(res.Contents[0].Text) < 1000 {
		t.Fatalf("widget HTML looks wrong (%d bytes)", len(res.Contents[0].Text))
	}
}

func TestConnectDomainsCarryTheLoopbackPort(t *testing.T) {
	server := newTestServer(t, nil, 51234)
	session := connect(t, server)

	res, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	ui, ok := res.Resources[0].Meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui missing or wrong type: %#v", res.Resources[0].Meta)
	}
	csp, ok := ui["csp"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui.csp missing: %#v", ui)
	}
	domains, ok := csp["connectDomains"].([]any)
	if !ok {
		t.Fatalf("_meta.ui.csp.connectDomains missing: %#v", csp)
	}
	if len(domains) != 8 {
		t.Fatalf("want 8 connect domains, got %d: %v", len(domains), domains)
	}
	if domains[0] != "http://127.0.0.1:51234" {
		t.Errorf("connectDomains[0] = %v", domains[0])
	}
	if domains[7] != "ws://localhost:*" {
		t.Errorf("connectDomains[7] = %v", domains[7])
	}
}

func TestRegistrationsApplyInOrder(t *testing.T) {
	var order []string
	registry := NewRegistry()
	registry.Add(
		Registration{Name: "phone", Order: OrderPhoneTools, Apply: func(*mcp.Server, *Env) error {
			order = append(order, "phone")
			return nil
		}},
		Registration{Name: "widget", Order: OrderWidgetTool, Apply: func(*mcp.Server, *Env) error {
			order = append(order, "widget")
			return nil
		}},
		Registration{Name: "app", Order: OrderAppTools, Apply: func(*mcp.Server, *Env) error {
			order = append(order, "app")
			return nil
		}},
	)
	newTestServer(t, registry, 0)

	want := []string{"widget", "app", "phone"}
	if len(order) != len(want) {
		t.Fatalf("applied %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("applied %v, want %v", order, want)
		}
	}
}

func TestShutdownRunsCleanupsInReverseAndOnce(t *testing.T) {
	server := newTestServer(t, nil, 0)
	var order []string
	server.Env.OnShutdown("first", func() error { order = append(order, "first"); return nil })
	server.Env.OnShutdown("second", func() error { order = append(order, "second"); return nil })

	server.Env.Shutdown()
	server.Env.Shutdown() // idempotent

	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("cleanup order = %v, want [second first]", order)
	}
}

func TestIsCleanShutdown(t *testing.T) {
	if !IsCleanShutdown(nil) {
		t.Error("nil must be a clean shutdown (stdin EOF after handshake)")
	}
	if !IsCleanShutdown(context.Canceled) {
		t.Error("context.Canceled must be a clean shutdown (SIGTERM/SIGINT)")
	}
	if !IsCleanShutdown(errString("server is closing: EOF")) {
		t.Error("pre-handshake EOF must be a clean shutdown")
	}
	if IsCleanShutdown(errString("write tcp: broken pipe")) {
		t.Error("a real transport failure must not be reported as clean")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
