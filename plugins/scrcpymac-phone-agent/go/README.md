# ScrcpyMac Phone Agent — Go server

The Go implementation of the plugin: one static binary that serves the MCP
server, plus adb and `scrcpy-server` shipped next to it. It replaces the Python
server and its first-run bootstrap (find Python 3.10+, create a `.venv`,
`pip install`), so a Marketplace install is "unzip and it runs".

Module: `github.com/zjywill/scrcpyMac/phone-agent` · Go 1.25 · no cgo.

Status: **feature-complete, pre-cutover**. All 36 contract tools, the widget
resource and the scrcpy H.264 runtime are implemented; `go build`, `go vet` and
`go test` are green, including a stdio smoke test against the built binary.
`mcp-server.sh`, `mcp.json` and `bin/phone-agent` still point at the Python
server — switching them over is a separate, deliberate step.

---

## Build, test, cross-compile

```bash
cd plugins/scrcpymac-phone-agent/go

go build ./...          # compile everything
go vet ./...            # static checks
go test ./...           # unit tests + the stdio smoke test (no device required)
make check              # vet + test

make build              # ./phone-agent for this host
make cross              # ../bin/darwin/{arm64,x86_64}/phone-agent
../scripts/build-go.sh  # the release build: refreshes the widget, stamps
                        # version + commit, writes both arch binaries

scripts/smoke-stdio.sh [binary]   # MCP handshake over stdio against a built
                                  # artefact; defaults to building from source

make parity                       # run BOTH implementations against the
                                  # attached device and diff the JSON (needs a
                                  # device and the plugin's Python .venv)
make parity-selftest              # prove the parity harness rejects a broken
                                  # port; no device, no binary
```

Three things carry the migration:

| Check | Guards |
| --- | --- |
| `internal/mcpserver/contract_test.go` | every tool in `docs/contract.json` is registered, with the same description, parameters, defaults, parameter order, annotations and `_meta`; no unexpected extra tool; the widget resource and server identity |
| `cmd/phone-agent/stdio_smoke_test.go` | the built binary completes a real handshake over stdio and **stdout carries JSON-RPC and nothing else** |
| `scripts/parity/` | the two implementations produce the *same bytes* for the same call on a real device — see below |

The contract test proves the tool *surface* matches. It says nothing about what
the tools return. `scripts/parity/run-parity.sh` closes that gap: it imports the
Python `phone_agent.server` in-process, drives the Go binary over a real MCP
stdio session, calls both with identical arguments against the attached device,
and diffs the results including **key order** — which is contract, because
Python dicts are insertion-ordered and Go maps are not. Only fields declared
volatile by path are excused, and `phone_doctor`'s three sanctioned deviations
(`python`→`binary`, `mcp_package`→`plugin_root`, `uv_available` dropped) are
asserted to be present *and* to be the only ones.

`scripts/parity/selftest.py` is what makes a green parity run mean something: it
perturbs real captured payloads 35 ways — keys alphabetised, `ok` moved,
`change_score` rendered as an int, Chinese escaped, `<` HTML-escaped, a node
dropped — and fails if the harness misses any of them.

Device-backed tests are opt-in and skipped by default:
`PHONE_AGENT_LIVE_DEVICE=1` (device tools), `PHONE_AGENT_DEVICE_TESTS=1`
(input tools — types into whatever is focused), `PHONE_AGENT_LIVE_SERIAL=<serial>`
(`internal/scrcpy` end-to-end stream).

Cross-compilation is `GOOS=darwin GOARCH=arm64|amd64` with `CGO_ENABLED=0`.
The `x86_64` output directory name is deliberate — it matches `uname -m` and the
existing `bin/<platform>/<arch>/` layout that `bin/phone-agent` dispatches on.

macOS distribution needs the binaries codesigned (ad-hoc at minimum) or
Gatekeeper refuses them on first run. That belongs on the cutover checklist.

### Run it

```bash
PHONE_AGENT_ROOT=.. ./phone-agent doctor    # diagnostics JSON on stdout
PHONE_AGENT_ROOT=.. ./phone-agent devices   # bare JSON array of devices
./phone-agent version                       # scrcpymac-phone-agent 0.7.2
./phone-agent mcp                           # MCP over stdio

# Adds the read-only phone_stream_status relay diagnostic (37th tool, model-
# visible). Off by default so the shipped surface matches the Python server's.
PHONE_AGENT_STREAM_DIAGNOSTICS=1 ./phone-agent mcp
```

`PHONE_AGENT_ROOT` is optional: when unset it is derived from the executable
path (`<root>/bin/<os>/<arch>/phone-agent` → three levels up, falling back to two
and one), verified by looking for `mcp.json`, and re-exported for child
processes. It is deliberately **not** symlink-resolved — `scripts/configure.sh`
symlinks the plugin into `~/.cursor/plugins/local`, and the doctor's `bundled`
flags are prefix comparisons against this exact string.

---

## The widget is embedded — refresh it before you build

`//go:embed` cannot follow symlinks or reach outside the module, so the widget
built by `scripts/build-ui.sh` into
`server/phone_agent/static/scrcpymac-app.html` is **copied** to
`internal/widget/assets/scrcpymac-app.html` and committed. It is a copy, not a
move: the Python server keeps reading the original until the cutover, and a
Marketplace build must never depend on npm being reachable.

Order of operations:

```bash
scripts/build-ui.sh    # npm ci && npm run build -> server/phone_agent/static/
cd go && make widget   # copy into internal/widget/assets/
go build ./...         # embeds it
```

`scripts/build-go.sh` performs the copy itself, so the release path is one
command. `make widget-check` fails when the embedded copy is stale — worth
wiring into CI, because a stale embed is invisible at runtime: the binary works,
it just serves the previous UI.

---

## Package layout

| Package | Owns |
| --- | --- |
| `cmd/phone-agent` | CLI dispatch (`mcp`/`doctor`/`devices`/`version`), signal handling, the single `os.Exit` |
| `internal/mcpserver` | server identity, capabilities, widget resource, **the tool registration interface**, shutdown plumbing |
| `internal/tools` | the MCP tools themselves plus the helpers they share |
| `internal/adb` | adb discovery and invocation, `devices -l` parsing, the device probes |
| `internal/paths` | plugin root, bundled adb, `scrcpy-server` resolution |
| `internal/doctor` | the diagnostics payload behind `phone-agent doctor` and `phone_doctor` |
| `internal/jsonresult` | Python-identical JSON serialisation, ordered objects, float/rounding compatibility |
| `internal/widget` | the embedded widget and its resource metadata |
| `internal/version` | version constant, build stamp, uname-style platform strings |

---

## Adding tools (the fan-out contract)

Several agents add tools in parallel. The rule that makes that conflict-free:
**create your own file in `internal/tools`; never edit someone else's.**

```go
// internal/tools/phone_input.go
package tools

func init() {
    mcpserver.Register(mcpserver.Registration{
        Name:  "phone-input",                  // group name, shows up in errors
        Order: mcpserver.OrderPhoneTools + 20, // see the Order* bands
        Apply: registerPhoneInput,
    })
}

func registerPhoneInput(s *mcp.Server, env *mcpserver.Env) error {
    mcp.AddTool(s, &mcp.Tool{
        Name:         "phone_tap",
        Description:  "Tap native device pixels. ...",
        InputSchema:  ObjectSchema("phone_tapArguments",
            Prop{Name: "x", Type: "integer", Title: "X", Required: true},
            Prop{Name: "y", Type: "integer", Title: "Y", Required: true},
            Prop{Name: "verify",  Type: "boolean", Title: "Verify",  Default: true},
            Prop{Name: "retries", Type: "integer", Title: "Retries", Default: 2},
        ),
        OutputSchema: StringOutputSchema("phone_tap"),
    }, func(ctx context.Context, _ *mcp.CallToolRequest, in tapIn) (*mcp.CallToolResult, StringOut, error) {
        return JSON(doTap(ctx, env, in))
    })
    return nil
}
```

`cmd/phone-agent` blank-imports `internal/tools`, so every `init` runs before
`mcpserver.New`; `New` then applies the registrations sorted by `Order` (ties
broken by insertion, i.e. file-name order). `Apply` runs at construction time,
not at init time, so `Env` is fully available.

Order bands, with gaps left for splitting groups later:

| Constant | Value | Group |
| --- | --- | --- |
| `OrderWidgetTool` | 100 | `open_scrcpymac` |
| `OrderAppTools` | 200 | the 11 app-only `scrcpymac_ui_*` |
| `OrderPhoneTools` | 300 | the 24 model-visible `phone_*` |

Prefix every symbol you declare with your group (`tapIn`, `doTap`) so two files
in this package can never collide.

### Shared helpers (`internal/tools/tools.go`)

| Helper | Use |
| --- | --- |
| `OK(payload)` | appends `ok: true` last — Python's `payload.setdefault("ok", True)` |
| `Fail(err)` | `{"ok": false, "error": ...}` with `ok` first |
| `JSON(payload, err)` / `JSONPayload` / `JSONFail` | Shape A: bare JSON in the text block, `{"result": "<same text>"}` as structuredContent |
| `Structured(payload, text)` | Shape C: hand-built structuredContent plus a human sentence |
| `StructuredError(err)` | Shape C failure with `isError: true` |
| `TextAndImage(payload, png, mime)` | the `phone_screenshot` text-then-image shape |
| `ObjectSchema` / `Prop` / `StringOutputSchema` | hand-written input/output schemas |
| `Annotations` / `BoolPtr` | annotation hints with the pointer/value trap handled |
| `OpenToolMeta` / `AppOnlyMeta` | the `_meta` blocks; `AppOnlyMeta` is the only thing hiding the 11 app tools from the model |
| `Clamp` / `StringOr` | parameter defaulting |

Env gives you `env.ADB()` (lazily resolved shared client), `env.Root`,
`env.Log`, `env.Context()` and `env.OnShutdown(name, fn)` — register cleanup
there for anything that must be released (scrcpy process, `adb forward --remove`,
the loopback listener). Cleanups run LIFO, exactly once, however the session
ends.

---

## Rules the whole binary lives by

**stdout is the transport.** `mcp.StdioTransport` is literally
`{os.Stdin, os.Stdout}`. One stray byte corrupts the session. So: `log` is
pointed at stderr in `main` before anything else, never `fmt.Print*`, and every
`exec.Command` captures its stdout into a buffer — `cmd.Stdout = os.Stdout` is
instant protocol corruption. The SDK's own logger stays nil (it defaults to
`slog.DiscardHandler`).

**`os.Exit` skips defers.** `main` is a thin wrapper around `run() int`; every
path returns instead of exiting, so `Env.Shutdown` always runs.

**Python-identical JSON.** Never `json.Marshal` a payload. `jsonresult.Text` is
`json.dumps(payload, ensure_ascii=False, indent=2)`: HTML escaping off (UI-tree
XML and WeChat text contain `<`, `>`, `&`), insertion-ordered keys
(`map[string]any` sorts alphabetically and would reorder every payload), no
trailing newline. Use `jsonresult.Obj`, `jsonresult.Float` for anything that is a
Python float (`0.0` must not become `0`), `jsonresult.PyRound`/`PyRoundInt`
(banker's rounding — `math.Round` is ties-away and shifts coordinates), and
`jsonresult.RuneLen` for `len()` on text. Initialise empty slices as `[]T{}`;
a nil slice marshals to `null`, not `[]`.

**Error strings are model-visible.** `adb.Error` messages are byte-identical to
the Python `AdbError` templates, down to which of them quotes the full argv
(timeout) and which quotes only the args (non-zero exit).

---

## Known deviations from the Python

Recorded here so nobody rediscovers them as bugs.

1. **`tools/list` order.** The Go SDK stores features in a set sorted by name,
   so tools come back alphabetically (`open_scrcpymac`, `phone_*`,
   `scrcpymac_ui_*`) rather than in the Python's registration order
   (`open_scrcpymac`, `scrcpymac_ui_*`, `phone_*`). The MCP spec does not define
   an ordering and no client should depend on one, so `contract_test.go`
   deliberately asserts the tool *set* and not `contract.json.toolOrder`. A
   transport wrapper can rewrite the marshaled `tools/list` result if byte
   fidelity is ever demanded (`jsonrpc.Response.Result` is an exported
   `json.RawMessage`); do not ship that unless a concrete client incompatibility
   shows up. Parameter order *is* asserted, because it survives to the wire.
2. **`readOnlyHint: false` / `idempotentHint: false` are omitted.** The SDK
   declares them as plain `bool` with `omitempty`. Both spec defaults are false,
   so absent and explicit-false are semantically identical. Affects nine
   `scrcpymac_ui_*` tools.
3. **`isError: false` and `experimental: {}` are omitted** on success — both
   spec-equivalent to the Python's explicit values.
4. **`serverInfo.version` is the plugin version (`0.7.2`)**, not the MCP SDK
   version. Python reported the SDK version by accident (FastMCP passes no
   explicit version); nothing keys off it.
5. **Doctor checks changed**: `python` → `binary` (which architecture, and
   whether it is running under Rosetta 2), `mcp_package` → `plugin_root` (root
   path + whether it was derived), and the top-level `uv_available` is gone.
   Everything else — the `plugin-h264-ready` backend, the em-dashed device
   messages, `ok`/`summary` semantics — is preserved verbatim.
6. **Two Python doctor bugs are not ported**: the `"\0"` sentinel that reported
   `bundled: true` for everything when `PHONE_AGENT_ROOT` was empty, and the
   duplicate `screen_size` check emitted when `current_app` raised after
   `screen_size` had already succeeded (the Go reports a failing
   `foreground_app` instead).
7. **`internal/tools.JSON` converts every error** into
   `{"ok": false, "error": ...}`. The Python caught only `AdbError` and
   `OSError`, letting anything else escape to FastMCP as `isError: true` with no
   structured payload. Flag it if a tool genuinely needs the escaping behaviour.
8. **`phone_enable_wifi_adb` runs the host command `adb tcpip <port>`**, not the
   Python's `adb shell tcpip <port>`, which is a device binary that does not
   exist (verified: exit 127). The tool has never succeeded, so nothing can
   depend on the old output; only `output` changes.
9. **One extra tool, off by default: `phone_stream_status`.** A read-only H.264
   relay diagnostic with no Python counterpart, added for the 1 FPS
   investigation: per-client queue depth, dropped GOPs, pump rates, and a hint
   naming which side is behind. It is model-visible, so registering it would give
   Codex a 25th `phone_*` tool the Python server never had — which the migration's
   drop-in requirement forbids. It is therefore gated:

   ```sh
   PHONE_AGENT_STREAM_DIAGNOSTICS=1 bin/phone-agent mcp
   ```

   A default build publishes exactly the contract's 36 tools, asserted by
   `TestDefaultSurfaceIsExactlyTheContract`. Flip the default only after deciding
   that Codex seeing the tool is wanted; the allowance is recorded in
   `contract_test.go`'s `deliberateAdditions`.
10. **A null `arguments` member is treated as absent** (`internal/mcpserver/
    middleware.go`). It has to be: go-sdk v1.6.1 + jsonschema-go v0.4.3 panic on
    it, which killed the whole process. The same middleware contains panics from
    tool handlers so one bad call cannot leak the scrcpy session and the adb
    forwards.

---

## Not implemented here

Owned by later phases, listed so the gaps are explicit rather than surprising:

- in-process adb download (`PHONE_AGENT_AUTO_DOWNLOAD_ADB`). The bash launcher
  still handles it; the Go binary logs a warning and continues, never fatally;
- a scrcpy `RESET_VIDEO` control message. After a dropped GOP a client waits for
  the device's next IDR, up to scrcpy's 10 s interval. The constant could not be
  extracted from the bundled dex offline; the drop path is unreachable in
  measurements so far (0 drops, 0-byte peak client queue);
- the `bin/phone-agent` cutover to a pure arch-dispatch shim, and
  `THIRD_PARTY_NOTICES.md` entries for the Go SDK (Apache-2.0 with a residual
  MIT tail), jsonschema-go, segmentio/encoding, segmentio/asm,
  yosida95/uritemplate, golang.org/x/oauth2 and golang.org/x/sys;
- `server/tests/test_packaging.py` does not yet include `internal/version.
  Version` in the set of files whose versions must match. All seven agree on
  `0.7.2` today; nothing stops them drifting.

## Reference documents

`docs/` holds the specs this implementation is checked against:
`mcp-contract.md` and the machine-readable `contract.json` (the frozen tool
surface), `spec-actions.md`, `spec-adb-doctor.md` and `spec-go-sdk.md`.
