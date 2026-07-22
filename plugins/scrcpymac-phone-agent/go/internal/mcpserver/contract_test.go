// Contract test: the migration safety net.
//
// docs/contract.json is the frozen MCP surface of the Python server, captured
// from the live process rather than read off the source. This test builds the
// real Go server — the package-level registry that internal/tools populates from
// init(), the same one cmd/phone-agent uses — drives it over a real MCP session,
// and compares what a client actually receives against that file, field by
// field.
//
// It fails on a missing tool, an unexpected extra tool, a renamed or retyped
// parameter, a changed default, a reordered parameter list, an altered
// annotation or _meta block, a changed description or title, a changed
// input/output schema, and on any drift in the server identity, capabilities or
// widget resource.
//
// It is an EXTERNAL test package (mcpserver_test) on purpose: internal/tools
// imports mcpserver, so an in-package test could not import it.
//
// Comparisons run against the RAW WIRE JSON, captured through a transport
// wrapper, not against client-side decoded structs. That matters for property
// order: jsonschema.Schema.PropertyOrder is `json:"-"`, so it controls what the
// server emits but is lost the moment a client decodes it — a decoded-then-
// re-marshaled schema is alphabetical and would hide a reordering.
package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"

	// Registers every tool group, exactly as cmd/phone-agent does. Without this
	// blank import the server would come up empty and this file would be
	// asserting nothing.
	_ "github.com/zjywill/scrcpyMac/phone-agent/internal/tools"
)

// contractLoopbackPort is substituted for the ${LOOPBACK_PORT} token in the
// contract's widget CSP. The real port is ephemeral; the contract records the
// shape and the ordering dependency, not the literal.
const contractLoopbackPort = 51234

// deliberateAdditions are tools this server may expose that the Python did not.
//
// Everything here is a conscious product decision that has to survive review at
// cutover: the map is the record of that decision and the reason it was made.
// Any tool NOT listed here and NOT in the contract fails the test, which is the
// point — an accidental extra tool is exactly the "Codex can tell the
// difference" regression this file exists to catch.
//
// The entries are ALLOWANCES, not expectations: every one of them must be
// opt-in, so a default build publishes exactly the contract's 36 tools. See
// TestDefaultSurfaceIsExactlyTheContract, which fails if any of them registers
// itself without being asked.
var deliberateAdditions = map[string]string{
	"phone_stream_status": "Read-only H.264 relay diagnostic added for the 1 FPS " +
		"investigation: queue depth, dropped GOPs and pump rates per client, with a " +
		"hint naming which side is behind. Model-visible, so it WOULD be a 25th " +
		"phone_* tool Codex can see — hence gated behind " +
		"PHONE_AGENT_STREAM_DIAGNOSTICS and off by default.",
}

// ---------------------------------------------------------------------------
// contract.json
// ---------------------------------------------------------------------------

type contractDoc struct {
	Server struct {
		Name         string         `json:"name"`
		Instructions string         `json:"instructions"`
		Capabilities map[string]any `json:"capabilities"`
	} `json:"server"`
	Resource struct {
		URI         string         `json:"uri"`
		Name        string         `json:"name"`
		Title       string         `json:"title"`
		Description string         `json:"description"`
		MIMEType    string         `json:"mimeType"`
		Meta        map[string]any `json:"meta"`
	} `json:"resource"`
	ToolOrder  []string `json:"toolOrder"`
	ToolCounts struct {
		Total         int `json:"total"`
		Phone         int `json:"phone"`
		OpenScrcpymac int `json:"openScrcpymac"`
		ScrcpymacUI   int `json:"scrcpymacUi"`
	} `json:"toolCounts"`
	Tools []contractTool `json:"tools"`
}

type contractTool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Params       []contractParam `json:"params"`
	InputSchema  map[string]any  `json:"inputSchema"`
	OutputSchema map[string]any  `json:"outputSchema"`
	Annotations  map[string]any  `json:"annotations"`
	Meta         map[string]any  `json:"meta"`
	Visibility   []string        `json:"visibility"`
}

type contractParam struct {
	Name        string `json:"name"`
	JSONType    string `json:"jsonType"`
	Required    bool   `json:"required"`
	Default     any    `json:"default"`
	SchemaTitle string `json:"schemaTitle"`
}

func loadContract(t *testing.T) *contractDoc {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "contract.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc contractDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep 2 an integer, so a default of 2 never reads as 2.0
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Tools) == 0 {
		t.Fatalf("%s declares no tools", path)
	}
	return &doc
}

// ---------------------------------------------------------------------------
// The live surface, captured as raw wire JSON
// ---------------------------------------------------------------------------

// liveSurface is what a client receives from the assembled server.
type liveSurface struct {
	tools      map[string]json.RawMessage // tool name -> its object in tools/list
	toolNames  []string                   // in tools/list order
	resources  []json.RawMessage
	initResult *mcp.InitializeResult
}

var (
	surfaceOnce sync.Once
	surfaceVal  *liveSurface
	surfaceErr  error
)

// captureSurface builds the server once per test binary and caches what it
// published.
//
// Once, not per test, for two reasons: the registry is process-global and every
// caller would get an identical surface anyway, and internal/tools' scrcpy group
// registers Runtime.Close as a shutdown cleanup on the process-wide runtime — so
// a per-test build-and-shut-down would close it out from under the next one.
func captureSurface(t *testing.T) *liveSurface {
	t.Helper()
	surfaceOnce.Do(func() { surfaceVal, surfaceErr = buildSurface() })
	if surfaceErr != nil {
		t.Fatalf("build the server surface: %v", surfaceErr)
	}
	return surfaceVal
}

func buildSurface() (*liveSurface, error) {
	server, err := mcpserver.New(context.Background(), mcpserver.Options{
		LoopbackPort: contractLoopbackPort,
		Log:          mcpserver.NewLogger(os.Stderr),
	})
	if err != nil {
		return nil, fmt.Errorf("mcpserver.New: %w", err)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()

	serverSession, err := server.MCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("server connect: %w", err)
	}
	defer func() { _ = serverSession.Close() }()

	rec := &rawRecorder{}
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &capturingTransport{inner: clientTransport, rec: rec}, nil)
	if err != nil {
		return nil, fmt.Errorf("client connect: %w", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.ListTools(ctx, nil); err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	if _, err := session.ListResources(ctx, nil); err != nil {
		return nil, fmt.Errorf("resources/list: %w", err)
	}

	surface := &liveSurface{tools: map[string]json.RawMessage{}, initResult: session.InitializeResult()}

	toolsRaw, err := rec.resultContaining("tools")
	if err != nil {
		return nil, err
	}
	var toolsList struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(toolsRaw, &toolsList); err != nil {
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}
	for _, raw := range toolsList.Tools {
		var head struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("decode tool: %w", err)
		}
		if _, dup := surface.tools[head.Name]; dup {
			return nil, fmt.Errorf("tool %q is registered twice", head.Name)
		}
		surface.tools[head.Name] = raw
		surface.toolNames = append(surface.toolNames, head.Name)
	}

	resourcesRaw, err := rec.resultContaining("resources")
	if err != nil {
		return nil, err
	}
	var resourceList struct {
		Resources []json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(resourcesRaw, &resourceList); err != nil {
		return nil, fmt.Errorf("decode resources/list: %w", err)
	}
	surface.resources = resourceList.Resources

	return surface, nil
}

// rawRecorder keeps the `result` member of every JSON-RPC response the client
// reads.
type rawRecorder struct {
	results []json.RawMessage
}

func (r *rawRecorder) add(result json.RawMessage) {
	r.results = append(r.results, result)
}

// resultContaining returns the last recorded result that has the given
// top-level key.
func (r *rawRecorder) resultContaining(key string) (json.RawMessage, error) {
	for i := len(r.results) - 1; i >= 0; i-- {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(r.results[i], &obj); err != nil {
			continue
		}
		if _, ok := obj[key]; ok {
			return r.results[i], nil
		}
	}
	return nil, fmt.Errorf("no captured JSON-RPC result carried a %q member", key)
}

type capturingTransport struct {
	inner mcp.Transport
	rec   *rawRecorder
}

func (t *capturingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &capturingConn{Connection: conn, rec: t.rec}, nil
}

type capturingConn struct {
	mcp.Connection
	rec *rawRecorder
}

func (c *capturingConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		return msg, err
	}
	if resp, ok := msg.(*jsonrpc.Response); ok && len(resp.Result) > 0 {
		c.rec.add(slices.Clone(resp.Result))
	}
	return msg, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestContractToolSetIsExact(t *testing.T) {
	doc := loadContract(t)
	live := captureSurface(t)

	contractNames := map[string]bool{}
	for _, tool := range doc.Tools {
		contractNames[tool.Name] = true
	}

	var missing []string
	for _, tool := range doc.Tools {
		if _, ok := live.tools[tool.Name]; !ok {
			missing = append(missing, tool.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("tools in docs/contract.json but NOT registered (%d): %v", len(missing), missing)
	}

	var unexpected []string
	for _, name := range live.toolNames {
		if contractNames[name] {
			continue
		}
		if reason, allowed := deliberateAdditions[name]; allowed {
			t.Logf("deliberate addition %q: %s", name, reason)
			continue
		}
		unexpected = append(unexpected, name)
	}
	if len(unexpected) > 0 {
		t.Errorf("tools registered but NOT in docs/contract.json and not declared in "+
			"deliberateAdditions (%d): %v\n"+
			"Codex must not be able to tell the Go server from the Python one. Either drop the "+
			"tool, hide it with tools.AppOnlyMeta, or add it to deliberateAdditions with a reason.",
			len(unexpected), unexpected)
	}

	// The contract's own counts, re-derived from what is actually registered.
	var phone, ui, open int
	for _, name := range live.toolNames {
		if _, extra := deliberateAdditions[name]; extra {
			continue
		}
		switch {
		case name == "open_scrcpymac":
			open++
		case strings.HasPrefix(name, "scrcpymac_ui_"):
			ui++
		case strings.HasPrefix(name, "phone_"):
			phone++
		}
	}
	if open != doc.ToolCounts.OpenScrcpymac || ui != doc.ToolCounts.ScrcpymacUI || phone != doc.ToolCounts.Phone {
		t.Errorf("tool counts = open %d / scrcpymac_ui_* %d / phone_* %d, contract wants %d / %d / %d",
			open, ui, phone, doc.ToolCounts.OpenScrcpymac, doc.ToolCounts.ScrcpymacUI, doc.ToolCounts.Phone)
	}
	if total := open + ui + phone; total != doc.ToolCounts.Total {
		t.Errorf("contract tools registered = %d, want %d", total, doc.ToolCounts.Total)
	}

	// Every contract tool must be reachable by name, in any order. tools/list
	// order is deliberately NOT asserted: the SDK sorts its feature set by name
	// while the Python emitted registration order, the MCP spec defines no
	// ordering, and no client may depend on one (README deviation 1).
	if len(doc.ToolOrder) != len(doc.Tools) {
		t.Errorf("contract toolOrder has %d entries but tools has %d", len(doc.ToolOrder), len(doc.Tools))
	}
}

// TestDefaultSurfaceIsExactlyTheContract is the strict form of the migration's
// hard constraint: with no environment set, a client must see the Python
// server's 36 tools and nothing else. Every deliberate addition therefore has to
// be opt-in; one that registers itself unconditionally fails here.
func TestDefaultSurfaceIsExactlyTheContract(t *testing.T) {
	doc := loadContract(t)
	live := captureSurface(t)

	if len(live.toolNames) != len(doc.Tools) {
		t.Errorf("a default build publishes %d tools, the contract has %d: %v",
			len(live.toolNames), len(doc.Tools), live.toolNames)
	}
	for _, name := range live.toolNames {
		if reason, extra := deliberateAdditions[name]; extra {
			t.Errorf("deliberate addition %q is registered in a DEFAULT build; it must be "+
				"opt-in so a Marketplace install matches the frozen surface.\nreason on record: %s",
				name, reason)
		}
	}
}

func TestContractToolFieldsAreIdentical(t *testing.T) {
	doc := loadContract(t)
	live := captureSurface(t)

	for _, want := range doc.Tools {
		raw, ok := live.tools[want.Name]
		if !ok {
			continue // reported by TestContractToolSetIsExact
		}
		t.Run(want.Name, func(t *testing.T) {
			var got struct {
				Name         string          `json:"name"`
				Title        string          `json:"title"`
				Description  string          `json:"description"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				OutputSchema json.RawMessage `json:"outputSchema"`
				Annotations  map[string]any  `json:"annotations"`
				Meta         map[string]any  `json:"_meta"`
			}
			decodeStrict(t, raw, &got)

			if got.Title != want.Title {
				t.Errorf("title = %q, want %q", got.Title, want.Title)
			}
			if got.Description != want.Description {
				t.Errorf("description mismatch\n got: %q\nwant: %q", got.Description, want.Description)
			}

			compareJSONValue(t, "inputSchema", got.InputSchema, want.InputSchema)
			compareJSONValue(t, "outputSchema", got.OutputSchema, want.OutputSchema)

			gotAnn := normalizeAnnotations(got.Annotations)
			wantAnn := normalizeAnnotations(want.Annotations)
			if !reflect.DeepEqual(gotAnn, wantAnn) {
				t.Errorf("annotations mismatch\n got: %s\nwant: %s", mustJSON(gotAnn), mustJSON(wantAnn))
			}

			if !reflect.DeepEqual(numberless(got.Meta), numberless(want.Meta)) {
				t.Errorf("_meta mismatch\n got: %s\nwant: %s", mustJSON(got.Meta), mustJSON(want.Meta))
			}
		})
	}
}

// TestContractParametersAreIdentical checks the parameter list one parameter at
// a time. TestContractToolFieldsAreIdentical already deep-compares the whole
// inputSchema, so this cannot fail on its own — but when a schema does drift it
// says which parameter and how, instead of printing two schemas to diff by eye.
// It additionally asserts property ORDER, which the raw capture preserves.
func TestContractParametersAreIdentical(t *testing.T) {
	doc := loadContract(t)
	live := captureSurface(t)

	for _, want := range doc.Tools {
		raw, ok := live.tools[want.Name]
		if !ok {
			continue
		}
		t.Run(want.Name, func(t *testing.T) {
			var envelope struct {
				InputSchema json.RawMessage `json:"inputSchema"`
			}
			decodeStrict(t, raw, &envelope)
			if len(envelope.InputSchema) == 0 {
				t.Fatalf("tool has no inputSchema; the SDK panics on a nil one, so this should be impossible")
			}

			var schema struct {
				Type       string                     `json:"type"`
				Title      string                     `json:"title"`
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			decodeStrict(t, envelope.InputSchema, &schema)

			if schema.Type != "object" {
				t.Errorf("inputSchema.type = %q, want \"object\"", schema.Type)
			}
			if wantTitle, _ := want.InputSchema["title"].(string); schema.Title != wantTitle {
				t.Errorf("inputSchema.title = %q, want %q", schema.Title, wantTitle)
			}

			order := propertyOrder(t, envelope.InputSchema)
			var wantOrder, wantRequired []string
			for _, p := range want.Params {
				wantOrder = append(wantOrder, p.Name)
				if p.Required {
					wantRequired = append(wantRequired, p.Name)
				}
			}
			if !slices.Equal(order, wantOrder) {
				t.Errorf("parameter order = %v, want %v", order, wantOrder)
			}
			if !slices.Equal(schema.Required, wantRequired) {
				t.Errorf("required = %v, want %v", schema.Required, wantRequired)
			}

			for _, p := range want.Params {
				prop, ok := schema.Properties[p.Name]
				if !ok {
					t.Errorf("parameter %q is missing (renamed or dropped); schema has %v", p.Name, order)
					continue
				}
				var got struct {
					Type    string          `json:"type"`
					Title   string          `json:"title"`
					Default json.RawMessage `json:"default"`
				}
				decodeStrict(t, prop, &got)

				if got.Type != p.JSONType {
					t.Errorf("parameter %q type = %q, want %q", p.Name, got.Type, p.JSONType)
				}
				if got.Title != p.SchemaTitle {
					t.Errorf("parameter %q title = %q, want %q", p.Name, got.Title, p.SchemaTitle)
				}
				if p.Required {
					if len(got.Default) != 0 {
						t.Errorf("parameter %q is required but declares default %s", p.Name, got.Default)
					}
					continue
				}
				if len(got.Default) == 0 {
					t.Errorf("parameter %q lost its default (want %v)", p.Name, p.Default)
					continue
				}
				var gotDefault any
				dec := json.NewDecoder(bytes.NewReader(got.Default))
				dec.UseNumber()
				if err := dec.Decode(&gotDefault); err != nil {
					t.Fatalf("parameter %q default is not JSON: %v", p.Name, err)
				}
				if !sameJSONScalar(gotDefault, p.Default) {
					t.Errorf("parameter %q default = %#v, want %#v", p.Name, gotDefault, p.Default)
				}
			}
		})
	}
}

// TestContractVisibilityHidesTheAppOnlyTools guards the single thing that keeps
// the eleven scrcpymac_ui_* tools out of the model's tool list. An SDK or helper
// change that dropped unknown _meta keys would expose all eleven to Codex, and
// nothing else in the surface would look wrong.
func TestContractVisibilityHidesTheAppOnlyTools(t *testing.T) {
	doc := loadContract(t)
	live := captureSurface(t)

	appOnly := 0
	for _, want := range doc.Tools {
		raw, ok := live.tools[want.Name]
		if !ok {
			continue
		}
		var got struct {
			Meta map[string]any `json:"_meta"`
		}
		decodeStrict(t, raw, &got)

		var visibility []string
		if ui, ok := got.Meta["ui"].(map[string]any); ok {
			if list, ok := ui["visibility"].([]any); ok {
				for _, v := range list {
					visibility = append(visibility, fmt.Sprint(v))
				}
			}
		}

		switch {
		case len(want.Visibility) == 1 && want.Visibility[0] == "model":
			// The 24 phone_* tools carry no _meta at all in the Python.
			if len(visibility) != 0 {
				t.Errorf("%s: model-visible tool declares _meta.ui.visibility %v", want.Name, visibility)
			}
		default:
			if !slices.Equal(visibility, want.Visibility) {
				t.Errorf("%s: _meta.ui.visibility = %v, want %v", want.Name, visibility, want.Visibility)
			}
			if slices.Equal(want.Visibility, []string{"app"}) {
				appOnly++
			}
		}
	}
	if appOnly != doc.ToolCounts.ScrcpymacUI {
		t.Errorf("%d tools are hidden from the model, want %d", appOnly, doc.ToolCounts.ScrcpymacUI)
	}
}

func TestContractResourceIsIdentical(t *testing.T) {
	doc := loadContract(t)
	live := captureSurface(t)

	if len(live.resources) != 1 {
		t.Fatalf("resources/list returned %d resources, want exactly 1", len(live.resources))
	}
	var got struct {
		URI         string         `json:"uri"`
		Name        string         `json:"name"`
		Title       string         `json:"title"`
		Description string         `json:"description"`
		MIMEType    string         `json:"mimeType"`
		Meta        map[string]any `json:"_meta"`
	}
	decodeStrict(t, live.resources[0], &got)

	for _, f := range []struct{ field, got, want string }{
		{"uri", got.URI, doc.Resource.URI},
		{"name", got.Name, doc.Resource.Name},
		{"title", got.Title, doc.Resource.Title},
		{"description", got.Description, doc.Resource.Description},
		{"mimeType", got.MIMEType, doc.Resource.MIMEType},
	} {
		if f.got != f.want {
			t.Errorf("resource %s = %q, want %q", f.field, f.got, f.want)
		}
	}

	want := substituteLoopbackPort(doc.Resource.Meta, contractLoopbackPort)
	if !reflect.DeepEqual(numberless(got.Meta), numberless(want)) {
		t.Errorf("resource _meta mismatch\n got: %s\nwant: %s", mustJSON(got.Meta), mustJSON(want))
	}
}

func TestContractServerIdentity(t *testing.T) {
	doc := loadContract(t)
	live := captureSurface(t)

	info := live.initResult.ServerInfo
	if info.Name != doc.Server.Name {
		t.Errorf("serverInfo.name = %q, want %q", info.Name, doc.Server.Name)
	}
	// serverInfo.version deliberately differs: FastMCP reported the installed
	// mcp SDK version because it passes none of its own. Go reports the plugin
	// version, which is what the field is for. Assert it is the plugin version
	// and not an SDK-looking string.
	if info.Version == "" || strings.HasPrefix(info.Version, "1.") {
		t.Errorf("serverInfo.version = %q; want the plugin version, not the SDK's", info.Version)
	}
	if live.initResult.Instructions != doc.Server.Instructions {
		t.Errorf("instructions mismatch\n got: %q\nwant: %q", live.initResult.Instructions, doc.Server.Instructions)
	}

	gotCaps := toMap(t, live.initResult.Capabilities)
	wantCaps := map[string]any{}
	for k, v := range doc.Server.Capabilities {
		// The SDK omits an empty experimental block; the Python emitted {}.
		// Spec-equivalent (README deviation 3).
		if k == "experimental" {
			if m, ok := v.(map[string]any); ok && len(m) == 0 {
				continue
			}
		}
		wantCaps[k] = v
	}
	// listChanged:false / subscribe:false are the spec defaults and the SDK
	// omits them, so compare on presence of each capability block plus any
	// explicitly true flag.
	for name, wantVal := range wantCaps {
		gotVal, ok := gotCaps[name]
		if !ok {
			t.Errorf("capabilities lost %q (want %v)", name, wantVal)
			continue
		}
		wantFlags, _ := wantVal.(map[string]any)
		gotFlags, _ := gotVal.(map[string]any)
		for flag, want := range wantFlags {
			got, present := gotFlags[flag]
			if !present {
				got = false // omitted booleans are false per the MCP schema
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("capabilities.%s.%s = %v, want %v", name, flag, got, want)
			}
		}
		for flag, got := range gotFlags {
			if _, expected := wantFlags[flag]; !expected && fmt.Sprint(got) != "false" {
				t.Errorf("capabilities.%s declares unexpected %s = %v", name, flag, got)
			}
		}
	}
	for name := range gotCaps {
		if _, expected := wantCaps[name]; !expected {
			t.Errorf("capabilities declares unexpected %q; the Python advertised only %v",
				name, slices.Sorted(maps(wantCaps)))
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// decodeStrict decodes with UseNumber so integer defaults keep their integer
// form: encoding/json would otherwise turn a default of 2 into float64(2) and a
// naive comparison against the contract's 2 would fail for the wrong reason.
func decodeStrict(t *testing.T, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// compareJSONValue deep-compares a raw JSON member against the contract's
// decoded value, treating an absent member and a null contract value as equal.
func compareJSONValue(t *testing.T, field string, raw json.RawMessage, want map[string]any) {
	t.Helper()
	trimmed := bytes.TrimSpace(raw)
	absent := len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
	if want == nil {
		if !absent {
			t.Errorf("%s should be absent, got %s", field, raw)
		}
		return
	}
	if absent {
		t.Errorf("%s is missing, want %s", field, mustJSON(want))
		return
	}
	var got any
	decodeStrict(t, raw, &got)
	if !reflect.DeepEqual(numberless(got), numberless(want)) {
		t.Errorf("%s mismatch\n got: %s\nwant: %s", field, raw, mustJSON(want))
	}
}

// propertyOrder returns the property names of a schema in the order they appear
// in the raw JSON. jsonschema.Schema.PropertyOrder is `json:"-"`: it drives what
// the server writes but never survives a decode, so the only way to see a
// reordering is to read the bytes.
func propertyOrder(t *testing.T, schema json.RawMessage) []string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(schema, &top); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	props, ok := top["properties"]
	if !ok {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(props))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("properties is not an object: %v", err)
	}
	var order []string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatalf("read property name: %v", err)
		}
		name, _ := key.(string)
		order = append(order, name)
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			t.Fatalf("skip property %q: %v", name, err)
		}
	}
	return order
}

// normalizeAnnotations expands both sides to every hint with its MCP-spec
// default, so absence and an explicit default compare equal.
//
// Two documented, spec-equivalent deltas make this necessary:
//   - mcp.ToolAnnotations declares ReadOnlyHint and IdempotentHint as plain bool
//     with omitempty, so an explicit false is dropped from tools/list where the
//     Python emitted it. Both default to false, so the meaning is unchanged.
//   - the Python emitted "title": null; the SDK omits the field.
//
// A hint that genuinely flips — destructiveHint or openWorldHint, whose spec
// defaults are TRUE — still fails, which is the case that matters.
func normalizeAnnotations(a map[string]any) map[string]any {
	out := map[string]any{
		"readOnlyHint":    false,
		"destructiveHint": true,
		"idempotentHint":  false,
		"openWorldHint":   true,
		"title":           "",
	}
	for k, v := range a {
		if v == nil {
			continue // "title": null is the same as no title
		}
		out[k] = v
	}
	return out
}

// numberless rewrites json.Number to a canonical string so a value decoded with
// UseNumber compares equal to the same value decoded without it.
func numberless(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return "num:" + strconv.FormatInt(i, 10)
		}
		f, _ := strconv.ParseFloat(t.String(), 64)
		return "num:" + strconv.FormatFloat(f, 'g', -1, 64)
	case float64:
		if t == float64(int64(t)) {
			return "num:" + strconv.FormatInt(int64(t), 10)
		}
		return "num:" + strconv.FormatFloat(t, 'g', -1, 64)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = numberless(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = numberless(val)
		}
		return out
	default:
		return v
	}
}

func sameJSONScalar(got, want any) bool {
	return reflect.DeepEqual(numberless(got), numberless(want))
}

// substituteLoopbackPort replaces the contract's ${LOOPBACK_PORT} token. The
// contract normalises the port because it is ephemeral — what is frozen is that
// the four concrete CSP entries carry the bound loopback port and the four
// wildcards follow them.
func substituteLoopbackPort(v any, port int) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "${LOOPBACK_PORT}", strconv.Itoa(port))
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = substituteLoopbackPort(val, port)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = substituteLoopbackPort(val, port)
		}
		return out
	default:
		return v
	}
}

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	decodeStrict(t, raw, &out)
	return out
}

func mustJSON(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<unmarshalable: %v>", err)
	}
	return string(raw)
}

func maps(m map[string]any) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
