# ScrcpyMac Phone Agent Go Runtime

The plugin ships one native Go MCP server plus bundled `adb`,
`scrcpy-server`, and a single-file Codex MCP App widget. There is no Python
runtime, virtual environment, package bootstrap, or backend fallback.

Module: `github.com/zjywill/scrcpyMac/phone-agent`

Release toolchain: Go 1.26.5 with `CGO_ENABLED=0`.

## Build And Test

```bash
cd plugins/scrcpymac-phone-agent/go

go test ./...
go vet ./...
make check

# Host binary for local stdio testing.
make build

# Release binaries and embedded widget.
../scripts/build-go.sh
```

`scripts/build-go.sh` runs the UI typecheck/build first, writes the single-file
widget directly to `internal/widget/assets/scrcpymac-app.html`, then builds:

```text
bin/darwin/arm64/phone-agent
bin/darwin/x86_64/phone-agent
```

The release build stamps the plugin version and current short Git commit into
both binaries.

## Runtime Commands

```bash
bin/phone-agent doctor
bin/phone-agent devices
bin/phone-agent version
bin/phone-agent mcp
```

`bin/phone-agent` is an architecture dispatcher only. It resolves the bundled
Go binary and exits with an actionable error if no compatible binary exists.
`PHONE_AGENT_BINARY=/absolute/path/to/phone-agent` can override it for local
smoke testing.

## MCP Contract

The default server publishes exactly 37 tools:

- 24 model-visible `phone_*` tools
- `open_scrcpymac`
- 12 app-only `scrcpymac_ui_*` tools

`docs/contract.json` is the machine-readable contract. The tests enforce tool
names, descriptions, schemas, defaults, annotations, `_meta`, visibility,
resource metadata, server identity, and exact tool count.

The native widget is served as:

```text
ui://widget/scrcpymac/app.html
text/html;profile=mcp-app
```

`open_scrcpymac` binds that resource with `_meta.ui.resourceUri`. Internal
widget tools declare `_meta.ui.visibility = ["app"]`, keeping them available
to the component without exposing them to the model.

## H.264 Transports

The Go runtime owns the scrcpy process, adb forward, video socket, control
socket, and teardown.

1. The widget first attempts the token-protected loopback WebSocket.
2. If the Codex sandbox cannot connect to loopback, the app-only
   `scrcpymac_ui_stream_pull` tool carries the same H.264 application packets
   in `CallToolResult._meta`.
3. JPEG screenshot polling is the final compatibility fallback only.

Both H.264 transports use the same GOP-aware relay queue and WebCodecs decoder.
Packet bytes never enter model-visible content.

## Package Layout

| Package | Responsibility |
| --- | --- |
| `cmd/phone-agent` | CLI dispatch, stdio MCP server, signals and shutdown |
| `internal/mcpserver` | server identity, MCP resource, registrations and middleware |
| `internal/tools` | model-visible and app-only MCP tools |
| `internal/adb` | adb discovery, commands and device probes |
| `internal/scrcpy` | H.264 session, relay, control socket and cleanup |
| `internal/widget` | embedded MCP App and resource metadata |
| `internal/doctor` | Go runtime and bundled-asset diagnostics |
| `internal/packaging` | marketplace, version and Go-only layout tests |
| `internal/version` | plugin version and build stamp |

## Verification

```bash
# Source and contract tests.
go test ./...
go vet ./...

# Stdio handshake against a release artifact.
go/scripts/smoke-stdio.sh \
  bin/darwin/arm64/phone-agent

# Verify the installed plugin copy.
plugins/scrcpymac-phone-agent/scripts/install-local.sh
```

Device-backed stream acceptance must record the first decoded frame, UI FPS,
source packet rate, dropped GOPs/packets, adb forward cleanup, and device-side
`scrcpymac-plugin-server` process cleanup.

## Reference Documents

- `docs/contract.json`: canonical MCP surface
- `docs/mcp-contract.md`: detailed MCP wire contract
- `docs/spec-actions.md`: Android action behavior
- `docs/spec-adb-doctor.md`: adb and doctor behavior
- `docs/spec-go-sdk.md`: Go SDK implementation notes
