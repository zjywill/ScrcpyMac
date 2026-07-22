// Package tools holds the MCP tool implementations and the helpers they share.
//
// # How to add tools (read this before writing a new file here)
//
// Create your OWN file in this package — never edit someone else's — with one
// init() that registers a group:
//
//	// phone_input.go
//	func init() {
//	    mcpserver.Register(mcpserver.Registration{
//	        Name:  "phone-input",
//	        Order: mcpserver.OrderPhoneTools + 20,
//	        Apply: registerPhoneInput,
//	    })
//	}
//
//	func registerPhoneInput(s *mcp.Server, env *mcpserver.Env) error {
//	    mcp.AddTool(s, &mcp.Tool{
//	        Name:         "phone_tap",
//	        Description:  "Tap native device pixels. ...",
//	        InputSchema:  ObjectSchema("phone_tapArguments",
//	            Prop{Name: "x", Type: "integer", Title: "X", Required: true},
//	            Prop{Name: "y", Type: "integer", Title: "Y", Required: true},
//	            Prop{Name: "verify", Type: "boolean", Title: "Verify", Default: true},
//	            Prop{Name: "retries", Type: "integer", Title: "Retries", Default: 2},
//	        ),
//	        OutputSchema: StringOutputSchema("phone_tap"),
//	    }, func(ctx context.Context, _ *mcp.CallToolRequest, in tapIn) (*mcp.CallToolResult, StringOut, error) {
//	        payload, err := doTap(ctx, env, in)
//	        return JSON(payload, err)
//	    })
//	    return nil
//	}
//
// Keep every symbol you declare prefixed by your group (tapIn, doTap) so
// parallel work in this package cannot collide.
//
// # The three result shapes
//
// The Python server emits three structurally different shapes, and which one a
// tool uses is contract:
//
//   - Shape A — the 23 phone_* tools annotated `-> str`. FastMCP synthesises an
//     outputSchema of {"result": string}; content[0].text is the BARE payload
//     while structuredContent is {"result": "<that same JSON text>"}, i.e.
//     double-encoded. Use StringOutputSchema + JSON/JSONFail.
//   - Shape B — phone_screenshot only: no outputSchema, no structuredContent,
//     one or two content blocks. Use Structured/TextAndImage with Out = any.
//   - Shape C — open_scrcpymac and the 11 scrcpymac_ui_* tools: no outputSchema,
//     a hand-built structuredContent object and a human sentence as the text
//     block. Use Structured/StructuredError with Out = any.
//
// Three SDK behaviours make the shapes easy to break:
//
//   - Returning a non-nil error makes the SDK call SetError: IsError=true,
//     Content replaced by the error text, StructuredContent DISCARDED. Any tool
//     that needs a structured error payload must return (result, out, nil) and
//     set IsError by hand — that is what StructuredError does.
//   - A typed Out silently overwrites res.StructuredContent. Hand-built
//     structuredContent therefore requires Out = any with a nil out.
//   - If res.Content is nil, the SDK fills it with the serialized out. For
//     Shape A that would put the {"result": ...} wrapper in the text block
//     instead of the bare payload, so Content is always set explicitly.
package tools

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/widget"
)

// WidgetURI is the widget resource the app-facing tools render into.
const WidgetURI = widget.URI

// OpenToolMeta is the _meta of open_scrcpymac: visible to both the model and the
// app, and carrying the widget template.
var OpenToolMeta = mcp.Meta{
	"ui": map[string]any{
		"resourceUri": WidgetURI,
		"visibility":  []string{"model", "app"},
	},
	"ui/resourceUri":                 WidgetURI,
	"openai/outputTemplate":          WidgetURI,
	"openai/widgetAccessible":        true,
	"openai/toolInvocation/invoking": "Opening ScrcpyMac...",
	"openai/toolInvocation/invoked":  "ScrcpyMac ready",
}

// AppOnlyMeta hides a tool from the model: it is the ONLY thing keeping the 11
// scrcpymac_ui_* tools out of Codex's tool list. If it ever fails to round-trip,
// the model sees eleven extra tools.
var AppOnlyMeta = mcp.Meta{
	"ui": map[string]any{
		"visibility": []string{"app"},
	},
}

// StringOut is the Shape A structuredContent: {"result": "<json text>"}.
type StringOut struct {
	Result string `json:"result"`
}

// OK stamps ok:true onto a payload unless it is already present, appending it
// LAST — Python's payload.setdefault("ok", True). Payloads that need ok first
// must set it first themselves.
func OK(payload *jsonresult.Obj) *jsonresult.Obj {
	if payload == nil {
		payload = jsonresult.New()
	}
	return payload.SetDefault("ok", true)
}

// Fail builds the standard failure payload, with ok first: {"ok": false,
// "error": "<message>"}. The message is model-visible and must match the Python
// error strings byte for byte.
func Fail(err error) *jsonresult.Obj {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return jsonresult.Of("ok", false, "error", msg)
}

// JSON returns a Shape A result, converting a non-nil error into the
// {"ok": false, "error": ...} payload.
//
// Note the deliberate difference from the Python: server.py catches only
// AdbError and OSError, so any other exception escapes to FastMCP and becomes
// isError:true with no structured payload. Routing every error through this
// helper turns those into the same well-formed payload. That is a fix, not an
// accident — flag it if a tool genuinely needs the escaping behaviour.
func JSON(payload *jsonresult.Obj, err error) (*mcp.CallToolResult, StringOut, error) {
	if err != nil {
		return JSONFail(err)
	}
	return JSONPayload(payload)
}

// JSONPayload returns a Shape A success result.
func JSONPayload(payload *jsonresult.Obj) (*mcp.CallToolResult, StringOut, error) {
	text := jsonresult.Text(payload)
	return &mcp.CallToolResult{
		// Content must be set explicitly, or the SDK fills it with the
		// {"result": ...} wrapper instead of the bare payload.
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, StringOut{Result: text}, nil
}

// JSONFail returns a Shape A failure result. isError stays false: in the Python
// all 24 phone_* tools report isError:false even when the body says ok:false,
// and failure lives only inside the JSON.
func JSONFail(err error) (*mcp.CallToolResult, StringOut, error) {
	return JSONPayload(Fail(err))
}

// Structured returns a Shape C result: a hand-built structuredContent object
// plus a human-readable text block.
func Structured(payload *jsonresult.Obj, text string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: payloadValue(payload),
	}, nil, nil
}

// StructuredError returns a Shape C failure: {"ok": false, "error": msg} as
// structuredContent, the bare message as the text block, and isError set by
// hand.
//
// Not every failing app tool uses this: scrcpymac_ui_state's failure branch
// deliberately returns isError:false with ok:false, error and devices:[], which
// is Structured with a failure payload, not StructuredError.
func StructuredError(err error) (*mcp.CallToolResult, any, error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: msg}},
		StructuredContent: map[string]any{"ok": false, "error": msg},
		IsError:           true,
	}, nil, nil
}

// TextAndImage returns the phone_screenshot shape: the JSON payload as text,
// then an image block, in that order, with no structuredContent at all.
//
// data is RAW bytes. encoding/json base64-encodes []byte on its own; pre-encoding
// ships double-base64 and a broken screenshot.
func TextAndImage(payload *jsonresult.Obj, data []byte, mimeType string) (*mcp.CallToolResult, any, error) {
	content := []mcp.Content{&mcp.TextContent{Text: jsonresult.Text(payload)}}
	if len(data) > 0 {
		if mimeType == "" {
			mimeType = "image/png"
		}
		content = append(content, &mcp.ImageContent{Data: data, MIMEType: mimeType})
	}
	return &mcp.CallToolResult{Content: content}, nil, nil
}

// payloadValue converts an ordered payload into what the SDK marshals as
// structuredContent. Key order is not observable there — the SDK marshals the
// value itself — but the Obj is kept so callers can build one payload and use it
// for both the text block and the structured content.
func payloadValue(payload *jsonresult.Obj) any {
	if payload == nil {
		return map[string]any{}
	}
	return payload.Map()
}

// Prop describes one input-schema property.
type Prop struct {
	Name string
	// Type is a JSON Schema type: "string", "integer", "number", "boolean",
	// "array" or "object".
	Type string
	// Title is the pydantic-style title FastMCP emitted, e.g. "Display Mode"
	// for display_mode. Keep it identical or tools/list drifts.
	Title string
	// Description is optional; most Python parameters have none.
	Description string
	// Default is the JSON default. It is ignored for a required property,
	// mirroring JSON Schema semantics (and the SDK, which skips defaults on
	// required properties).
	Default any
	// Items types the elements of an array property.
	Items *jsonschema.Schema
	// Required marks the property as required.
	Required bool
}

// ObjectSchema hand-builds an input schema.
//
// Hand-writing is not a style preference: jsonschema-go's `jsonschema:"..."`
// struct tag sets ONLY the description — there is no `default=` tag — a field
// without omitempty is inferred REQUIRED, and defaults are skipped on required
// properties. So `verify: bool = True` is unreachable by inference. AddTool uses
// a provided schema verbatim, and Schema.PropertyOrder is honoured by
// Schema.MarshalJSON, so declaration order survives to the wire.
func ObjectSchema(title string, props ...Prop) *jsonschema.Schema {
	schema := &jsonschema.Schema{
		Type:       "object",
		Title:      title,
		Properties: map[string]*jsonschema.Schema{},
	}
	for _, p := range props {
		ps := &jsonschema.Schema{Type: p.Type, Title: p.Title}
		if p.Description != "" {
			ps.Description = p.Description
		}
		if p.Items != nil {
			ps.Items = p.Items
		}
		if p.Required {
			schema.Required = append(schema.Required, p.Name)
		} else if p.Default != nil {
			ps.Default = mustRawJSON(p.Default)
		}
		schema.Properties[p.Name] = ps
		schema.PropertyOrder = append(schema.PropertyOrder, p.Name)
	}
	return schema
}

// StringOutputSchema is the Shape A output schema FastMCP synthesised for a
// tool annotated `-> str`.
func StringOutputSchema(toolName string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:  "object",
		Title: toolName + "Output",
		Properties: map[string]*jsonschema.Schema{
			"result": {Type: "string", Title: "Result"},
		},
		PropertyOrder: []string{"result"},
		Required:      []string{"result"},
	}
}

// Annotations builds a tool's annotation hints.
//
// DestructiveHint and OpenWorldHint must be pointers because their spec defaults
// are TRUE — passing them by value would invert the meaning. ReadOnlyHint and
// IdempotentHint are plain bools in the SDK with omitempty, so an explicit false
// is dropped from tools/list where Python emitted it; both spec defaults are
// false, so absent and false are semantically identical.
func Annotations(readOnly, destructive, idempotent, openWorld bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		DestructiveHint: BoolPtr(destructive),
		IdempotentHint:  idempotent,
		OpenWorldHint:   BoolPtr(openWorld),
	}
}

// BoolPtr returns a pointer to b.
func BoolPtr(b bool) *bool { return &b }

// StringOr returns v, or def when v is empty. Schema defaults are applied by the
// SDK before the handler runs, so this is a guard for clients that send an
// explicit empty string.
func StringOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Clamp constrains v to [lo, hi].
func Clamp[T int | int64 | float64](v, lo, hi T) T {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// mustRawJSON marshals a schema default. A default that cannot be marshalled is
// a programming error caught at startup, when the tool is registered.
func mustRawJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("tools: cannot marshal schema default " + err.Error())
	}
	return raw
}
