# spec-go-sdk.md — MCP Go SDK recon for the phone-agent migration

Status: **verified by compiling and by running both servers side by side**, not recalled.
Date: 2026-07-22.

Everything in this document was checked in two ways:

1. A throwaway spike at `scratchpad/sdkspike` that `go build ./...`, `go vet ./...` and
   `go test ./...` all pass on, driven over real stdio JSON-RPC.
2. The **live Python server** (`server/phone_agent/server.py`) driven over the same stdio
   JSON-RPC and diffed against the Go output. The Python contract capture is the
   authority for every claim about "what Python emits".

---

## 1. Module path and version

| Item | Value |
| --- | --- |
| Module path | `github.com/modelcontextprotocol/go-sdk` |
| Version to pin | **`v1.6.1`** (published 2026-05-22, latest **stable**) |
| Import path used | `github.com/modelcontextprotocol/go-sdk/mcp` |
| Also needed directly | `github.com/google/jsonschema-go/jsonschema` (v0.4.3, pulled in by the SDK) |
| Transport-wrapper package | `github.com/modelcontextprotocol/go-sdk/jsonrpc` (only if §6 workaround is used) |
| SDK `go.mod` requires | `go 1.25.0` — our toolchain is go1.25.7, fine |
| License | Apache-2.0 with a residual MIT tail (project is mid-relicense). **Both must be reproduced in `THIRD_PARTY_NOTICES.md`.** |

Verified against the module proxy, which is authoritative:

```
$ curl -s https://proxy.golang.org/github.com/modelcontextprotocol/go-sdk/@latest
{"Version":"v1.6.1","Time":"2026-05-22T11:30:38Z", ...}
```

`v1.7.0-pre.1/2/3` exist but are prereleases. **Pin `v1.6.1`.** v1.0.0 formalised a
no-breaking-changes guarantee, so v1.x upgrades are safe, but a shipped Marketplace
binary should build reproducibly from a pinned version + `go.sum`.

Transitive dependency set (all pure Go, no cgo):

```
github.com/google/jsonschema-go v0.4.3
github.com/segmentio/encoding v0.5.4   // indirect
github.com/segmentio/asm v1.1.3        // indirect
github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
golang.org/x/oauth2 v0.35.0            // indirect  (auth package; unused by us)
golang.org/x/sys v0.41.0               // indirect
```

Protocol versions the SDK supports (`mcp/shared.go`):
`2025-11-25` (latest/default), `2025-06-18`, `2025-03-26`, `2024-11-05`.
Negotiation rule: echo the client's version if supported, else answer with `2025-11-25`.
Codex today sends `2025-06-18`, and both the Python and Go servers echo it. **No action
needed** — but note the Go server will answer `2025-11-25` to a client that asks for
something unknown, where Python's older SDK might not.

---

## 2. The contract to reproduce (captured from the live Python server)

36 tools: 24 `phone_*` + `open_scrcpymac` + 11 `scrcpymac_ui_*`. Three distinct shapes:

### Shape A — `phone_*` tools (24 of them)

Python declares `-> str` and returns `json_result(payload)`. FastMCP therefore
synthesises an **outputSchema wrapper**:

```jsonc
// tools/list entry
{
  "name": "phone_tap",
  "description": "Tap native device pixels. ...",
  "inputSchema": {
    "properties": {
      "x":       {"title": "X", "type": "integer"},
      "y":       {"title": "Y", "type": "integer"},
      "verify":  {"default": true, "title": "Verify",  "type": "boolean"},
      "retries": {"default": 2,    "title": "Retries", "type": "integer"}
    },
    "required": ["x", "y"],
    "title": "phone_tapArguments",
    "type": "object"
  },
  "outputSchema": {
    "properties": {"result": {"title": "Result", "type": "string"}},
    "required": ["result"],
    "title": "phone_tapOutput",
    "type": "object"
  }
}
```

```jsonc
// tools/call result
{
  "content": [{"type": "text", "text": "{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"}],
  "structuredContent": {"result": "{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"},
  "isError": false
}
```

Note the double encoding: `structuredContent.result` is a **JSON string containing JSON**,
and `content[0].text` is the *bare* payload, not the `{"result": ...}` wrapper.
No `title`, no `annotations`, no `_meta` on any `phone_*` tool.

### Shape B — `phone_screenshot` (the only one)

Python's return type is unannotated, so there is **no outputSchema and no
structuredContent at all**. With `include_image=true` the result is exactly two content
blocks in this order:

```jsonc
{
  "content": [
    {"type": "text",  "text": "{\n  \"serial\": \"2f019965\",\n  \"width\": 1080, ... }"},
    {"type": "image", "data": "iVBORw0KGgo...", "mimeType": "image/png"}
  ],
  "isError": false
}
```

### Shape C — `open_scrcpymac` + 11 `scrcpymac_ui_*`

`title`, full `annotations`, arbitrary `_meta`, hand-built `structuredContent`,
**no outputSchema**:

```jsonc
{
  "name": "open_scrcpymac",
  "title": "Open ScrcpyMac",
  "description": "Open the complete ScrcpyMac phone workspace in Codex.",
  "inputSchema": {"properties": {"display_mode": {"default": "fullscreen", "title": "Display Mode", "type": "string"}}, "title": "open_scrcpymacArguments", "type": "object"},
  "annotations": {"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
  "_meta": {
    "ui": {"resourceUri": "ui://widget/scrcpymac/app.html", "visibility": ["model", "app"]},
    "ui/resourceUri": "ui://widget/scrcpymac/app.html",
    "openai/outputTemplate": "ui://widget/scrcpymac/app.html",
    "openai/widgetAccessible": true,
    "openai/toolInvocation/invoking": "Opening ScrcpyMac...",
    "openai/toolInvocation/invoked": "ScrcpyMac ready"
  }
}
```

Error path (`_error()` in `mcp_ui.py`):

```jsonc
{
  "content": [{"type": "text", "text": "Android device not found: nope"}],
  "structuredContent": {"ok": false, "error": "Android device not found: nope"},
  "isError": true
}
```

### The resource

```jsonc
// resources/list
{
  "uri": "ui://widget/scrcpymac/app.html",
  "name": "scrcpymac-app",
  "title": "ScrcpyMac",
  "description": "Interactive Android mirroring and control workspace.",
  "mimeType": "text/html;profile=mcp-app",
  "_meta": { "ui": {...}, "openai/widgetDescription": ..., "openai/widgetCSP": {...} }
}
// resources/read -> contents[0] = {uri, mimeType, text: <the whole widget HTML>}
```

---

## 3. Server construction, stdio, instructions  *(capability a)*

```go
import "github.com/modelcontextprotocol/go-sdk/mcp"

server := mcp.NewServer(
    &mcp.Implementation{
        Name:    "scrcpymac-phone-agent",
        Title:   "ScrcpyMac Phone Agent",
        Version: "0.7.2",
    },
    &mcp.ServerOptions{
        Instructions: "Control a connected Android phone. The plugin owns its standalone " +
            "scrcpy H.264 and control session; ScrcpyMac.app is not required. " +
            "ADB remains available for screenshots and accessibility automation. " +
            "For Chinese text, use phone_paste. WeChat: com.tencent.mm.",
        Capabilities: &mcp.ServerCapabilities{
            Tools:     &mcp.ToolCapabilities{ListChanged: false},
            Resources: &mcp.ResourceCapabilities{ListChanged: false, Subscribe: false},
            Prompts:   &mcp.PromptCapabilities{ListChanged: false},
        },
        // Logger left nil => slog.DiscardHandler. Never point it at stdout.
    },
)
// ... AddTool / AddResource ...
err := server.Run(ctx, &mcp.StdioTransport{})
```

Two non-obvious points:

* **`Capabilities` must be set explicitly.** If it is `nil`, the SDK advertises
  `{"logging":{}}` plus `listChanged:true` for tools/resources. Python advertises
  `listChanged:false` and no `logging`. Setting `Capabilities` as above matches Python.
* **`Implementation.Version` is our choice.** Python's FastMCP reports the *mcp library*
  version here (`"version": "1.25.0"` today), which is an accident, not a contract. Use
  the plugin version (`0.7.2`) — no client keys off it.

Verified initialize result from the spike:

```json
{"capabilities":{"prompts":{},"resources":{},"tools":{}},
 "instructions":"Control a connected Android phone. ...",
 "protocolVersion":"2025-06-18",
 "serverInfo":{"name":"scrcpymac-phone-agent","title":"ScrcpyMac Phone Agent","version":"0.7.2"}}
```

(Python additionally emits `"experimental":{}` and spells out the `false` sub-fields.
Absent booleans are spec-equivalent to `false`; no client behaviour changes.)

---

## 4. Typed parameters, defaults, descriptions  *(capability b)*

### The default-value trap — read this twice

`jsonschema-go` v0.4.3's `jsonschema:"..."` struct tag sets **only the description**.
There is **no `default=` tag**. Confirmed in `jsonschema/infer.go`:

```go
if tag, ok := field.Tag.Lookup("jsonschema"); ok {
    ...
    fs.Description = tag    // <-- that's all it does
}
```

Two further rules compound it:

* A struct field **without** `omitempty`/`omitzero` is inferred as **required**.
* `Resolved.ApplyDefaults` **skips defaults on required properties**
  (`validate.go`: `if schemaInfo.isRequired[prop] { continue }`).

So `verify bool = True` cannot be expressed by inference at all. Because the
`inputSchema` must also carry pydantic-style `title`s to byte-match Python, the right
answer is to **write the schema by hand** and let `AddTool` use it verbatim
(`setSchema` returns a provided `*jsonschema.Schema` untouched).

Helper used by the spike (`schema.go`):

```go
type prop struct {
    name     string
    typ      string // "string" | "integer" | "number" | "boolean"
    title    string
    def      any    // ignored when required
    required bool
}

func objectSchema(title string, props ...prop) *jsonschema.Schema {
    s := &jsonschema.Schema{
        Type:       "object",
        Title:      title,
        Properties: map[string]*jsonschema.Schema{},
    }
    for _, p := range props {
        ps := &jsonschema.Schema{Type: p.typ, Title: p.title}
        if p.required {
            s.Required = append(s.Required, p.name)
        } else {
            ps.Default = raw(p.def)   // json.RawMessage
        }
        s.Properties[p.name] = ps
        s.PropertyOrder = append(s.PropertyOrder, p.name)
    }
    return s
}

func stringResultOutputSchema(toolName string) *jsonschema.Schema {
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
```

`Schema.PropertyOrder` is honoured by `Schema.MarshalJSON`, so property order matches
Python's declaration order.

The Go input struct still needs `,omitempty` on optional fields so that inference — which
still runs for validation bookkeeping in some paths — agrees with the hand-written
schema, and so the field is not accidentally treated as required if the schema is ever
dropped:

```go
type tapIn struct {
    X       int  `json:"x"`
    Y       int  `json:"y"`
    Verify  bool `json:"verify,omitempty"`
    Retries int  `json:"retries,omitempty"`
}
```

Order of operations inside `ToolHandlerFor` (verified in `mcp/server.go` `toolForErr`):
raw arguments → `ApplyDefaults` → `Validate` → `json.Unmarshal` into `In` → handler.
So `verify` arrives as `true` when omitted and as `false` when explicitly `false`.
**Verified on the wire**: `{"x":10,"y":20}` yielded `verify:true, retries:2`;
`{"x":1,"y":2,"verify":false,"retries":0}` yielded `verify:false, retries:0`.

Zero-parameter tools: `objectSchema("phone_backendArguments")` renders
`{"type":"object","properties":{},"title":"phone_backendArguments"}` — identical to
Python. Use `type noArgsIn struct{}` as `In`.
`Server.AddTool` **panics** if `InputSchema` is nil, and panics if its `type` is not
`"object"`.

---

## 5. Text + structuredContent, image blocks, isError  *(capabilities c, d, g, h)*

### The single most important rule

`ToolHandlerFor[In, Out]` has signature:

```go
func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error)
```

and the SDK post-processes your result:

* If you return a **non-nil `error`**, the SDK calls `CallToolResult.SetError(err)`:
  `IsError = true`, `Content` becomes a single text block with `err.Error()`, and
  **`StructuredContent` is discarded**. → Never return an error when you need a
  structured error payload.
* If `Out != any`, the SDK marshals `out` and **overwrites `res.StructuredContent`**.
  → You cannot hand-write `StructuredContent` while also using a typed `Out`.
* If `res.Content == nil` and `out` is non-nil, the SDK fills `Content` with the
  serialized `out` JSON. → For Shape A you must set `Content` yourself, or the text
  block becomes the `{"result": ...}` wrapper instead of the bare payload.

That gives two clean patterns.

### Pattern C — hand-written result, `Out = any`, `out = nil`

Use for **all of `open_scrcpymac` + `scrcpymac_ui_*` + `phone_screenshot`**.
With `Out = any` and `t.OutputSchema == nil`, the SDK adds no output schema, and with
`out == nil` it skips the structured-content overwrite entirely.

```go
mcp.AddTool(s, &mcp.Tool{
    Name:        "open_scrcpymac",
    Title:       "Open ScrcpyMac",
    Description: "Open the complete ScrcpyMac phone workspace in Codex.",
    Meta:        openToolMeta,
    Annotations: &mcp.ToolAnnotations{
        ReadOnlyHint:    true,
        DestructiveHint: boolPtr(false),
        IdempotentHint:  true,
        OpenWorldHint:   boolPtr(false),
    },
    InputSchema: objectSchema("open_scrcpymacArguments",
        prop{name: "display_mode", typ: "string", title: "Display Mode", def: "fullscreen"},
    ),
}, func(_ context.Context, _ *mcp.CallToolRequest, in openIn) (*mcp.CallToolResult, any, error) {
    mode := in.DisplayMode
    if mode != "inline" && mode != "fullscreen" {
        mode = "fullscreen"
    }
    return &mcp.CallToolResult{
        Content: []mcp.Content{&mcp.TextContent{Text: "Opened the ScrcpyMac widget."}},
        StructuredContent: map[string]any{
            "ok":                   true,
            "widget":               "scrcpymac-app",
            "preferredDisplayMode": mode,
            "phase":                "standalone-h264-stream",
        },
    }, nil, nil
})
```

**isError with a structured payload** — same pattern, `IsError` set by hand,
`error` return left `nil`:

```go
return &mcp.CallToolResult{
    Content:           []mcp.Content{&mcp.TextContent{Text: msg}},
    StructuredContent: map[string]any{"ok": false, "error": msg},
    IsError:           true,
}, nil, nil
```

Verified on the wire:
`{"content":[{"type":"text","text":"Android device not found: nope"}],"structuredContent":{"error":"Android device not found: nope","ok":false},"isError":true}`

**Image content block** — `ImageContent.Data` is **raw bytes**; `encoding/json`
base64-encodes `[]byte` automatically. Do **not** pre-encode, or you ship
double-base64. Content order is preserved exactly as written:

```go
return &mcp.CallToolResult{
    Content: []mcp.Content{
        &mcp.TextContent{Text: jsonText(payload)},
        &mcp.ImageContent{Data: pngBytes, MIMEType: "image/png"},
    },
}, nil, nil
```

Verified: `[{"type":"text",...},{"type":"image","mimeType":"image/png","data":"iVBORw0..."}]`,
no `structuredContent`, exactly like Python.

### Pattern A — typed `Out`, explicit `Content`

Use for all 24 `phone_*` tools except `phone_screenshot`.

```go
type stringResultOut struct {
    Result string `json:"result"`
}

mcp.AddTool(s, &mcp.Tool{
    Name:         "phone_tap",
    Description:  "Tap native device pixels. By default verifies a screen change and retries nearby.",
    InputSchema:  objectSchema("phone_tapArguments",
        prop{name: "x", typ: "integer", title: "X", required: true},
        prop{name: "y", typ: "integer", title: "Y", required: true},
        prop{name: "verify",  typ: "boolean", title: "Verify",  def: true},
        prop{name: "retries", typ: "integer", title: "Retries", def: 2},
    ),
    OutputSchema: stringResultOutputSchema("phone_tap"),
}, func(_ context.Context, _ *mcp.CallToolRequest, in tapIn) (*mcp.CallToolResult, stringResultOut, error) {
    text := jsonText(payload)                   // the bare JSON, Python-formatted
    return &mcp.CallToolResult{                 // Content MUST be set explicitly
        Content: []mcp.Content{&mcp.TextContent{Text: text}},
    }, stringResultOut{Result: text}, nil
})
```

Verified byte-for-byte against Python:

```
Go:     {"content":[{"type":"text","text":"{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"}],
         "structuredContent":{"result":"{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"}}
Python: {"content":[{"type":"text","text":"{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"}],
         "structuredContent":{"result":"{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"},
         "isError":false}
```

### Reproducing Python's `json.dumps(..., ensure_ascii=False, indent=2)`

`actions.py:815` is `json.dumps(payload, ensure_ascii=False, indent=2)`. Plain
`json.MarshalIndent` does **not** match it, for two reasons:

* Go HTML-escapes `<`, `>`, `&` into `<`, `>`, `&`. Python does not.
  UI-tree dumps and WeChat message text can contain all three.
* Go sorts `map[string]any` keys; Python preserves insertion order. E.g.
  `phone_screenshot` emits `serial, width, height, format, size_bytes, ok` — not sorted.

Working encoder (`main.go`):

```go
func jsonText(v any) string {
    var buf bytes.Buffer
    enc := json.NewEncoder(&buf)
    enc.SetEscapeHTML(false)
    enc.SetIndent("", "  ")
    if err := enc.Encode(v); err != nil { ... }
    return strings.TrimRight(buf.String(), "\n") // Encode appends a newline
}
```

plus an insertion-ordered map (or a plain struct with ordered fields) instead of
`map[string]any`. The spike ships `orderedMap` with `Set` / `SetDefault`, the latter
mirroring Python's `payload.setdefault("ok", True)` which appends `ok` **last**.

Non-ASCII needs no special handling: Go emits raw UTF-8, same as `ensure_ascii=False`.
Both facts are pinned by `TestJSONTextMatchesPython` and `TestJSONTextUnicode` in the
spike, whose expected strings were produced by running the real `json.dumps`.

---

## 6. `_meta`, annotations, and the ONE thing the SDK cannot do  *(capabilities e, f)*

### `_meta` — fully supported, no workaround needed

`mcp.Meta` is `map[string]any`, embedded in both `Tool` and `Resource`:

```go
// mcp/protocol.go
type Tool struct {
    Meta `json:"_meta,omitempty"`
    ...
}
type Resource struct {
    Meta `json:"_meta,omitempty"`
    ...
}
```

Arbitrary keys — including slash-bearing ones like `ui/resourceUri` and
`openai/outputTemplate` — pass through unvalidated:

```go
var openToolMeta = mcp.Meta{
    "ui": map[string]any{
        "resourceUri": widgetURI,
        "visibility":  []string{"model", "app"},
    },
    "ui/resourceUri":                 widgetURI,
    "openai/outputTemplate":          widgetURI,
    "openai/widgetAccessible":        true,
    "openai/toolInvocation/invoking": "Opening ScrcpyMac...",
    "openai/toolInvocation/invoked":  "ScrcpyMac ready",
}
var appOnlyMeta = mcp.Meta{"ui": map[string]any{"visibility": []string{"app"}}}
```

Verified on the wire — the emitted `_meta` for `open_scrcpymac` and for the resource is
key-for-key identical to Python's (JSON object key *order* differs, which is
meaningless in JSON).

`Meta` is also available on `CallToolResult`, `ReadResourceResult`, `TextContent`,
`ImageContent` and `ResourceContents` if ever needed.

### Annotations — **the SDK cannot emit an explicit `false`**

This is the only genuine gap found. `mcp.ToolAnnotations`:

```go
type ToolAnnotations struct {
    DestructiveHint *bool  `json:"destructiveHint,omitempty"`  // pointer -> false IS emitted
    IdempotentHint  bool   `json:"idempotentHint,omitempty"`   // plain   -> false is DROPPED
    OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`    // pointer -> false IS emitted
    ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`     // plain   -> false is DROPPED
    Title           string `json:"title,omitempty"`
}
```

`ReadOnlyHint` and `IdempotentHint` are plain `bool` with `omitempty`, so an explicit
`false` vanishes from `tools/list`. Python's `ToolAnnotations` emits it. Measured:

```
Python : {"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
Go     : {                       "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
```

Affected tools (`readOnlyHint:false` and/or `idempotentHint:false` dropped):
`scrcpymac_ui_select_device`, `scrcpymac_ui_start_stream`, `scrcpymac_ui_stop_stream`,
`scrcpymac_ui_snapshot`, `scrcpymac_ui_tap`, `scrcpymac_ui_swipe`, `scrcpymac_ui_key`,
`scrcpymac_ui_paste`, `scrcpymac_ui_connect_wifi`.

Note `DestructiveHint`/`OpenWorldHint` are `*bool` precisely because their **spec
defaults are `true`** — for those, `boolPtr(false)` is mandatory or the meaning inverts.
`ReadOnlyHint`/`IdempotentHint` default to `false`, which is why the SDK feels free to
omit them.

**Recommendation: accept the omission.** The MCP spec defines both defaults as `false`,
so an absent key and an explicit `false` are semantically identical, and annotations are
documented as advisory hints clients must not make decisions on. Do this:

```go
func boolPtr(b bool) *bool { return &b }

Annotations: &mcp.ToolAnnotations{
    ReadOnlyHint:    false,          // will be omitted; spec default is false anyway
    DestructiveHint: boolPtr(false), // MUST be a pointer; spec default is true
    IdempotentHint:  true,
    OpenWorldHint:   boolPtr(false), // MUST be a pointer; spec default is true
},
```

**If byte-identical `tools/list` is later demanded**, there is a working workaround,
and it is the *only* place raw outgoing JSON is reachable: `jsonrpc.Response.Result` is
an exported `json.RawMessage`, and `mcp.Transport`/`mcp.Connection` are exported
interfaces. Wrap the transport and rewrite the marshaled result. Full working code is in
the spike at `patchtransport.go`; the core is:

```go
type annotationPatchConn struct {
    mcp.Connection
    want fullAnnotations // tool name -> {"readOnlyHint": false, ...}
}

func (c *annotationPatchConn) Write(ctx context.Context, msg jsonrpc.Message) error {
    if resp, ok := msg.(*jsonrpc.Response); ok && resp.Error == nil && len(resp.Result) > 0 {
        if patched, ok := patchToolsListResult(resp.Result, c.want); ok {
            cp := *resp          // copy: don't mutate the SDK's message
            cp.Result = patched
            msg = &cp
        }
    }
    return c.Connection.Write(ctx, msg)
}
```

Wired in as `server.Run(ctx, &annotationPatchTransport{inner: &mcp.StdioTransport{}, want: ...})`.
Verified: with the wrapper enabled, `scrcpymac_ui_select_device` gains
`"readOnlyHint": false`. Cost is one extra unmarshal/marshal per `tools/list` — a
once-per-session call. **Do not ship this unless a concrete client incompatibility is
observed.**

### Everything else the task listed is expressible

| Requirement | Verdict |
| --- | --- |
| Arbitrary `_meta` on a tool | ✅ `Tool.Meta` |
| Arbitrary `_meta` on a resource | ✅ `Resource.Meta` |
| Exact content-block ordering | ✅ `[]mcp.Content` is emitted in slice order, verified text-then-image |
| Image content block | ✅ `mcp.ImageContent` |
| `structuredContent` + text together | ✅ (Pattern C) |
| `isError` + structured payload | ✅ (Pattern C, `IsError` set by hand) |
| Custom `ui://` URI scheme | ✅ `AddResource` only runs `url.Parse`; `ui://widget/scrcpymac/app.html` accepted |
| `mimeType` with a `;profile=` parameter | ✅ passed through verbatim |
| Explicit `false` annotation hints | ❌ omitted — see workaround above |
| `"isError": false` on success | ❌ omitted (`json:"isError,omitempty"`); spec-equivalent |
| `"experimental": {}` in capabilities | ❌ never emitted; spec-equivalent |

---

## 7. The widget resource  *(capability g)*

```go
//go:embed static/scrcpymac-app.html
var widgetHTML string

const (
    widgetURI  = "ui://widget/scrcpymac/app.html"
    widgetMIME = "text/html;profile=mcp-app"
)

s.AddResource(
    &mcp.Resource{
        Meta:        resourceMeta(runtime.StreamConnectDomains()),
        URI:         widgetURI,
        Name:        "scrcpymac-app",
        Title:       "ScrcpyMac",
        Description: "Interactive Android mirroring and control workspace.",
        MIMEType:    widgetMIME,
    },
    func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        return &mcp.ReadResourceResult{
            Contents: []*mcp.ResourceContents{{
                URI:      widgetURI,
                MIMEType: widgetMIME,
                Text:     widgetHTML,
            }},
        }, nil
    },
)
```

Verified `resources/list` and `resources/read` output match Python exactly (modulo JSON
key ordering).

**Ordering constraint carried over from Python.** `register_scrcpymac_app` computes
`_resource_meta(runtime.stream_connect_domains())` **eagerly at import time**, and
`stream_connect_domains()` calls `_ensure_loopback()` — i.e. the loopback WebSocket
listener is bound *before* the MCP handshake, and its port is baked into the resource
`_meta` CSP `connectDomains` for the process lifetime. The Go port must do the same:
**bind the loopback listener during startup, before `AddResource`.** Getting this wrong
produces a widget whose CSP forbids the very port the stream is served on.

The Python `stream_connect_domains()` list (`scrcpy_runtime.py:324`) is:
`http://127.0.0.1:{port}`, `ws://127.0.0.1:{port}`, `http://localhost:{port}`,
`ws://localhost:{port}`, `http://127.0.0.1:*`, `ws://127.0.0.1:*`, … — reproduce it
verbatim.

`ResourceContents.Blob` is `[]byte` with `json:"blob,omitzero"` if binary resources are
ever needed. Not needed here — the widget is HTML text.

---

## 8. JSON-RPC / stdio lifecycle  *(capability j)*

### initialize

Handled entirely by the SDK. `Server.Run` → `Server.Connect` → session loop. There is no
initialize hook to write; `ServerOptions.InitializedHandler` fires on
`notifications/initialized` if post-handshake work is needed. Capabilities are computed
from `ServerOptions.Capabilities` plus inference from registered tools/resources
(§3). Protocol version is negotiated per §1.

### stdout hygiene — non-negotiable

`mcp.StdioTransport` is literally `rwc{os.Stdin, os.Stdout}` with newline-delimited JSON.
**One stray byte on stdout corrupts the session.** Rules for the Go server:

* `log.SetOutput(os.Stderr)` at the top of `main`, before anything else.
* Never `fmt.Print*` (they go to stdout). Use `fmt.Fprintf(os.Stderr, ...)`.
* Every `exec.Command` for `adb` / `scrcpy-server` must have its `Stdout` captured into a
  buffer or redirected to `os.Stderr` — **never inherited**. `cmd.Stdout = os.Stdout` is
  an instant protocol corruption.
* `ServerOptions.Logger` defaults to `slog.New(slog.DiscardHandler)`
  (`mcp/logging.go:93`), so the SDK is silent unless we opt in. If we do opt in, use
  `slog.NewTextHandler(os.Stderr, ...)`.
* Consider `os.Stdout` capture-and-restore in `main` as a belt-and-braces guard if any
  third-party code might print.

### Graceful shutdown

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

defer runtime.Close() // stop scrcpy-server, kill adb forwards, close listeners

if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
    if errors.Is(err, context.Canceled) {
        return // signalled; deferred cleanup runs
    }
    log.Printf("server error: %v", err)   // stderr
    os.Exit(1)                            // NOTE: os.Exit skips defers
}
```

Two independent shutdown triggers, both measured against the spike:

| Trigger | `Server.Run` returns | Observed |
| --- | --- | --- |
| SIGTERM / SIGINT | `context.Canceled` | cleanup ran, process exit 0 |
| stdin EOF after a completed handshake | `nil` | cleanup ran, process exit 0 |
| stdin EOF **before** the handshake completes | `errors.New("server is closing: EOF")` | non-nil error; treat as a normal shutdown, not a crash |

`Server.Run` internally does `ss.Close()` on ctx cancellation and waits for the session
goroutine (`server.go:946`), so there is no race between cancellation and cleanup.

**Trap:** `os.Exit` skips deferred functions. Any error path that calls `os.Exit` must
call the cleanup explicitly first, or the scrcpy process, the `adb forward` and the
loopback listener leak. Structure `main` as a thin wrapper around
`func run() error` and call cleanup before the single `os.Exit`.

There is no SIGKILL handler possible, so `adb forward --remove` may still leak on a hard
kill. Mirror the Python behaviour of reclaiming stale forwards on startup.

### Other lifecycle notes

* `ServerOptions.KeepAlive` (a `time.Duration`) enables periodic pings and auto-closes a
  dead session. Python does not use it; leave it zero to match.
* Input validation failures never reach the handler. The SDK returns
  `{"content":[{"type":"text","text":"validating \"arguments\": ..."}],"isError":true}`.
  Wording differs from Python's pydantic message — cosmetic, not contractual.
* `Server.AddTool` **panics** on a nil or non-object `InputSchema`; `AddResource` panics
  on an unparseable URI. These are startup-time panics — a smoke test that constructs the
  server and lists tools catches all of them.

---

## 9. Verified spike

Location: `scratchpad/sdkspike` (throwaway, own `go.mod`, **not** under
`plugins/scrcpymac-phone-agent/go`).

```
sdkspike/
├── go.mod              module sdkspike, requires go-sdk v1.6.1 + jsonschema-go v0.4.3
├── main.go             server, tools, ordered/HTML-unescaped JSON encoder, shutdown
├── schema.go           objectSchema / stringResultOutputSchema helpers
├── patchtransport.go   the annotations byte-fidelity workaround
├── spike_test.go       pins jsonText against real json.dumps output
└── widget.html         //go:embed target
```

```
$ go build ./... && go vet ./... && go test ./...
ok  	sdkspike	0.625s
```

Driven over real stdio JSON-RPC, it reproduces all three tool shapes, the resource, the
error path, and clean SIGTERM/EOF shutdown. Reference captures for diffing:

* `scratchpad/py-contract.jsonl` — the live Python server's `initialize` /
  `tools/list` / `resources/list` / `resources/read`.
* `scratchpad/go-contract.jsonl`, `go-a.jsonl`, `go-b.jsonl` — the Go spike's, with and
  without the annotation-patch transport.

---

## 10. Side finding — the Python dependency floor is unpinned

> **Corrected 2026-07-22.** An earlier draft of this section claimed `mcp` 1.25.0 removed
> the `meta` keyword from `FastMCP.resource()`, and that any new Marketplace install
> therefore gets a server that cannot start. **That is wrong** — it was re-tested directly
> against the currently installed and current-latest release:
>
> ```
> mcp 1.28.1
> resource() params: ['self','uri','name','title','description','mime_type','icons','annotations','meta']
> accepts meta: True     tool() accepts meta: True
> ```
>
> `FastMCP.resource()` still accepts `meta=`. Do not act on the retracted claim, and do
> not "fix" `server/` on account of it.

The real, weaker point stands: `server/pyproject.toml` pins `mcp>=1.0.0` with no upper
bound, and `scripts/ensure-runtime.sh` builds a fresh venv with an unpinned `pip install`.
Two users installing on different days can therefore get different `mcp` majors, and the
plugin's behaviour depends on a resolution that nobody recorded. That is environment
drift, not a live outage — it is a supporting argument for the migration, not evidence of
one. The Go build removes it outright: `go.mod` + `go.sum` pin the SDK exactly.

---

## 11. Checklist for the implementer

- [ ] `go.mod`: `module github.com/zjywill/scrcpyMac/phone-agent`, `go 1.26.0`,
      `toolchain go1.26.5`, `require github.com/modelcontextprotocol/go-sdk v1.6.1`.
      Commit `go.sum`. (Go 1.26.5 is the current stable release; Go 1.25 falls out of
      upstream support as soon as 1.27 ships. The host has go1.25.7 installed, but
      `GOTOOLCHAIN=auto` — the default — downloads go1.26.5 on demand, so nothing has to
      be installed by hand. Verified working.)
- [ ] `Capabilities` set explicitly (§3) — do not leave it nil.
- [ ] Every `InputSchema` hand-written via `objectSchema` with pydantic titles,
      `PropertyOrder`, `Required` and `Default`. Never rely on tag-based defaults.
- [ ] Optional input struct fields carry `,omitempty`.
- [ ] 24 `phone_*` tools use Pattern A (`stringResultOut` + explicit `Content` +
      `stringResultOutputSchema`), except `phone_screenshot`, which uses Pattern C.
- [ ] `open_scrcpymac` + 11 `scrcpymac_ui_*` use Pattern C with `Meta` and `Annotations`.
- [ ] `DestructiveHint` / `OpenWorldHint` always via `boolPtr(...)`.
- [ ] All payload JSON rendered by the ordered, `SetEscapeHTML(false)`, two-space
      encoder — never bare `json.MarshalIndent(map[string]any{...})`.
- [ ] Loopback listener bound before `AddResource`, its port baked into the resource
      `_meta` CSP.
- [ ] `log.SetOutput(os.Stderr)` first thing in `main`; no `cmd.Stdout = os.Stdout`
      anywhere.
- [ ] `run() error` + single `os.Exit` so cleanup defers always fire.
- [ ] Startup smoke test that builds the server and calls `tools/list` — catches every
      `AddTool`/`AddResource` panic before shipping.
- [ ] `THIRD_PARTY_NOTICES.md` gains go-sdk (Apache-2.0 / MIT tail), jsonschema-go,
      segmentio/encoding, segmentio/asm, yosida95/uritemplate, golang.org/x/oauth2,
      golang.org/x/sys — alongside the existing scrcpy Apache-2.0 entry.
