# ScrcpyMac Phone Agent — frozen MCP contract

This document freezes the **complete** MCP surface of the Python implementation
(`plugins/scrcpymac-phone-agent/server/phone_agent/`) so the Go port can be proven
byte-for-byte identical. Codex must not be able to tell the two apart.

* Plugin version: **0.7.2** (`phone_agent.__version__`)
* Python MCP SDK actually resolved by the launcher: **`mcp==1.28.1`** (FastMCP)
* Machine-readable twin: [`contract.json`](./contract.json) — designed for a Go test
  to assert the registered surface matches exactly.

Everything below was captured by **running the real server** (`tools/list`,
`tools/call`, `resources/read` through `FastMCP._mcp_server.request_handlers`) against
the attached OnePlus 6, not by reading the source alone. Source line references are
given so each claim can be re-verified.

---

## 0. Server identity

| Field | Value |
| --- | --- |
| name | `scrcpymac-phone-agent` |
| version reported in `serverInfo` | **the installed `mcp` SDK version** (FastMCP passes no explicit version, so `Server.version is None` and the low-level server falls back to `importlib.metadata.version("mcp")` → `"1.28.1"`). See the gotcha in §5. |
| prompts | none registered |
| resource templates | none |

Capabilities advertised (from `Server.get_capabilities`):

```json
{
  "experimental": {},
  "prompts":   { "listChanged": false },
  "resources": { "subscribe": false, "listChanged": false },
  "tools":     { "listChanged": false }
}
```

### `instructions` (verbatim, `server.py:20-25`)

The Python source builds it from four adjacent string literals; the resulting single
line is exactly:

```
Control a connected Android phone. The plugin owns its standalone scrcpy H.264 and control session; ScrcpyMac.app is not required. ADB remains available for screenshots and accessibility automation. For Chinese text, use phone_paste. WeChat: com.tencent.mm.
```

(No trailing newline, no leading whitespace, single spaces at the seams.)

---

## 1. Logical tool order

The Go SDK does not guarantee `tools/list` ordering. The logical order recorded
in `contract.json.toolOrder` is:

1. `open_scrcpymac`
2. `scrcpymac_ui_state`, `scrcpymac_ui_select_device`, `scrcpymac_ui_start_stream`,
   `scrcpymac_ui_stream_status`, `scrcpymac_ui_stream_pull`,
   `scrcpymac_ui_stop_stream`, `scrcpymac_ui_snapshot`,
   `scrcpymac_ui_tap`, `scrcpymac_ui_swipe`, `scrcpymac_ui_key`, `scrcpymac_ui_paste`,
   `scrcpymac_ui_connect_wifi` (**12** app-only tools)
3. the 24 model-visible `phone_*` tools.

**37 tools total.** `contract.json.toolOrder` is the authoritative list.

Clients must depend on names and metadata, not the transport order.

---

## 2. The two result shapes

FastMCP wraps a plain-string return completely differently from a hand-built
`CallToolResult`. Both shapes are live in this server and both must be reproduced.

### Shape A — "json-string" (23 of the 24 `phone_*` tools)

These are annotated `-> str` and return `json_result(...)`.

`phone_agent/actions.py:815`:

```python
def json_result(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=False, indent=2)
```

Because the return annotation is `str`, FastMCP's `func_metadata` takes the
`_create_wrapped_model` branch (`wrap_output=True`) and **declares an `outputSchema`**:

```json
{
  "properties": { "result": { "title": "Result", "type": "string" } },
  "required": ["result"],
  "title": "<tool_name>Output",
  "type": "object"
}
```

The wire result is therefore:

```jsonc
{
  "content": [ { "type": "text", "text": "<the json_result string>" } ],
  "structuredContent": { "result": "<the SAME json_result string>" },
  "isError": false
}
```

Observed live for `phone_backend`:

```jsonc
content[0].text   = "{\n  \"backend\": \"adb\",\n  \"ok\": true\n}"
structuredContent = { "result": "{\n  \"backend\": \"adb\",\n  \"ok\": true\n}" }
```

`isError` is **always `false`** for these tools, including on failure — the failure is
carried inside the JSON body (`_err`, `server.py:53`):

```json
{"ok": false, "error": "<str(exc)>"}
```

Only `AdbError` and `OSError` are caught. Anything else escapes the tool function,
the low-level server's `_make_error_result` runs, and you get
`isError: true`, `content=[TextContent(str(exc))]`, **no** `structuredContent`
(and no output-schema validation).

The success wrapper is `_ok` (`server.py:48`):

```python
def _ok(payload: dict) -> str:
    payload.setdefault("ok", True)
    return json_result(payload)
```

`setdefault` means **`"ok"` is appended LAST** when the underlying payload did not
already have it, and stays **FIRST** when it did. Confirmed live:
`{"backend": "adb", "ok": true}` vs `{"ok": true, "action": "tap", ...}`.

### Shape B — "structured" (all 12 tools in `mcp_ui.py`)

`mcp_ui.py:57` builds the result by hand:

```python
CallToolResult(
    content=[TextContent(type="text", text=text)],
    structuredContent=payload,
    isError=is_error,
)
```

FastMCP's `convert_result` short-circuits on `CallToolResult`, the low-level handler
returns it verbatim, and because the declared return type *is* `CallToolResult` with no
`Annotated` metadata, `func_metadata` returns early → **`outputSchema` is `null`** for
all 12. So there is no output validation and no `{"result": ...}` wrapping.

The text block is a short human sentence (e.g. `"Opened the ScrcpyMac widget."`), not
JSON. `ui/src/main.ts:315` reads `structuredContent` first and only falls back to
`JSON.parse` of the text block — so if the Go port ever emitted JSON only in the text
block the widget would still work, but the model-facing text would regress. Both must
be right.

Error results use `_error` (`mcp_ui.py:70`):

```jsonc
{ "content": [{"type": "text", "text": "<str(exc)>"}],
  "structuredContent": {"ok": false, "error": "<str(exc)>"},
  "isError": true }
```

**Exception:** `scrcpymac_ui_state` does *not* use `_error` on failure — see §3.13.

### Shape C — "text+image" (`phone_screenshot` only)

See §3.5.

---

## 3. Tools

Formatting note for every table below: `default` is the JSON Schema default emitted by
FastMCP; `required` is membership in `inputSchema.required`. The `inputSchema` always has
`"type": "object"` and `"title": "<funcname>Arguments"`, each property carries a
title-cased `"title"` (`max_fps` → `"Max Fps"`), and `additionalProperties` is absent.

---

### 3.1 `open_scrcpymac`

* **title**: `Open ScrcpyMac`
* **description** (verbatim): `Open the complete ScrcpyMac phone workspace in Codex.`
* **shape**: B (structured) · **outputSchema**: `null`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `display_mode` | string | `string` | no | `"fullscreen"` |

* **annotations**: `readOnlyHint=true, destructiveHint=false, idempotentHint=true, openWorldHint=false`
* **_meta** (`OPEN_TOOL_META`, `mcp_ui.py:38`):

```json
{
  "ui": { "resourceUri": "ui://widget/scrcpymac/app.html", "visibility": ["model", "app"] },
  "ui/resourceUri": "ui://widget/scrcpymac/app.html",
  "openai/outputTemplate": "ui://widget/scrcpymac/app.html",
  "openai/widgetAccessible": true,
  "openai/toolInvocation/invoking": "Opening ScrcpyMac...",
  "openai/toolInvocation/invoked": "ScrcpyMac ready"
}
```

* text block: `Opened the ScrcpyMac widget.`
* success keys: `ok, widget, preferredDisplayMode, phase`
  * `widget` is always `"scrcpymac-app"`, `phase` always `"standalone-h264-stream"`.
  * `display_mode` is normalised: anything other than `"inline"`/`"fullscreen"` → `"fullscreen"`.
* cannot fail (no try/except, no device access).

---

### 3.2 The twelve `scrcpymac_ui_*` tools — shared `_meta`

All eleven carry exactly (`APP_ONLY_META`, `mcp_ui.py:50`):

```json
{ "ui": { "visibility": ["app"] } }
```

`visibility: ["app"]` is what keeps them invisible to the model. `open_scrcpymac` is the
only one visible to both (`["model", "app"]`). The existing pytest
(`tests/test_mcp_ui.py::test_internal_tools_are_app_only`) asserts both this and that
`resourceUri` is absent from their `ui` dict — the Go port must satisfy the same.

None of them declare an `outputSchema`.

---

### 3.3 `scrcpymac_ui_state`

* **title**: `Read ScrcpyMac UI state`
* **description**: `Return device discovery and backend state for the ScrcpyMac widget.`
* **params**: none
* **annotations**: `readOnly=true, destructive=false, idempotent=true, openWorld=false`
* text block (success): `Read ScrcpyMac device state.`
* success keys: `ok, backend, selectedSerial, devices, stream`
  * `devices[]` = `{serial, state, model, product}`
  * `stream` = the full `ScrcpyRuntime.status()` payload (§3.6)
* **failure keys**: `ok, backend, selectedSerial, devices, stream, error` with
  `devices: []` — and **`isError` stays `false`**, text block is `str(exc)`.
  This is the one `mcp_ui` tool that does not use `_error()` (`mcp_ui.py:190-201`).

---

### 3.4 `scrcpymac_ui_select_device`

* **title**: `Select ScrcpyMac device`
* **description**: `Select the Android serial used by subsequent widget actions.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `serial` | string | `string` | **yes** | — |

* **annotations**: `readOnly=false, destructive=false, idempotent=true, openWorld=false`
* text block: `Selected Android device {serial}.` (f-string, uses the raw argument)
* success keys: `ok, serial, device` (`device` is the matching `adb devices -l` entry)
* failure: `_error` → `isError=true`, `{ok:false, error}`
* error strings: `device serial must not be empty` ·
  `Android device not found: {serial}` · `Android device is {state}: {serial}`
* side effect: sets `AdbClient.serial` and invalidates the UI-tree cache.

---

### 3.5 `scrcpymac_ui_start_stream`

* **title**: `Start standalone ScrcpyMac stream`
* **description**: `Start the plugin-owned scrcpy H.264 and control session.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `serial` | string | `string` | **yes** | — |
| `max_fps` | integer | `int` | no | `60` |
| `resolution_percent` | integer | `int` | no | `50` |

* **annotations**: `readOnly=false, destructive=false, idempotent=**false**, openWorld=false`
* text block: `Started the standalone ScrcpyMac H.264 stream.`
* success keys: the full streaming `status()` — `ok, state, backend, encoding, error,
  fps, frames, serial, deviceName, deviceWidth, deviceHeight, frameWidth, frameHeight,
  maxFps, resolutionPercent, codec, streamUrl`
* failure: `_error` (catches `AdbError`, `OSError`, `ScrcpyRuntimeError`)
* clamping happens inside `ScrcpyRuntime.start` (`scrcpy_runtime.py:405`):
  `max_fps = 60 if max_fps >= 45 else 30`; `resolution_percent = min(100, max(25, v))`.
  The *returned* `maxFps`/`resolutionPercent` are the clamped values.
* calls `actions.select_device(serial)` first, so an unknown serial fails before any
  scrcpy work.

---

### 3.6 `scrcpymac_ui_stream_status`

* **title**: `Read standalone ScrcpyMac stream status`
* **description**: `Read the plugin-owned H.264 stream state and measured FPS.`
* **params**: none
* **annotations**: `readOnly=true, destructive=false, idempotent=true, openWorld=false`
* text block: `Read the standalone ScrcpyMac stream status.`
* **no try/except — this tool can never return `isError`.**

`ScrcpyRuntime.status()` (`scrcpy_runtime.py:357`) emits, in this key order:

| key | always? | value |
| --- | --- | --- |
| `ok` | yes | `state != "error"` |
| `state` | yes | `"idle"` \| `"starting"` \| `"streaming"` \| `"error"` |
| `backend` | yes | `"plugin-h264"` while streaming else `"adb"` |
| `encoding` | yes | `"H.264"` while streaming else `"JPEG"` |
| `error` | yes | string, `""` when clean |
| `fps` | yes | `round(fps, 1)` — a **float**, serialised `0.0` not `0` |
| `frames` | yes | int |
| `serial` | only while `_meta` set | |
| `deviceName` | only while `_meta` set | scrcpy device name, falls back to serial |
| `deviceWidth` / `deviceHeight` | only while `_meta` set | **native** device pixels |
| `frameWidth` / `frameHeight` | only while `_meta` set | encoded frame size |
| `maxFps` / `resolutionPercent` | only while `_meta` set | clamped request values |
| `codec` | only while `_meta` set | `avc1.XXXXXX` parsed from the SPS, default `avc1.42E01E` |
| `streamUrl` | only when `state=="streaming"` **and** a loopback port **and** a token | `ws://127.0.0.1:{port}/stream?token={token}` |

FPS is computed from a `deque(maxlen=180)` of `time.monotonic()` stamps as
`(len-1) / (last - first)`, and only when at least 2 samples exist.

---

### 3.6a `scrcpymac_ui_stream_pull`

* **title**: `Pull standalone ScrcpyMac H.264 packets`
* **description**: `Long-poll a bounded H.264 packet batch through the MCP Apps bridge.`
* **params**: `max_bytes` integer default `524288`; `timeout_ms` integer default `250`
* **annotations**: `readOnly=true, destructive=false, idempotent=false, openWorld=false`
* text block: `Pulled a standalone ScrcpyMac H.264 packet batch.`
* structured keys: `ok, state, backend, encoding, error, fps, frames, frameWidth,
  frameHeight, codec, transport, packetCount, sizeBytes, droppedGops, droppedPackets`
* H.264 bytes live only in result `_meta["scrcpymac/h264"].dataBase64`
* `_meta.ui.visibility=["app"]` keeps the tool component-only

The payload concatenates complete ScrcpyMac application packets. Each packet
starts with the frozen 14-byte version/flags/PTS/length header, so the widget
can split the batch without another envelope.

---

### 3.7 `scrcpymac_ui_stop_stream`

* **title**: `Stop standalone ScrcpyMac stream`
* **description**: `Stop the plugin-owned scrcpy session and release its adb forward.`
* **params**: none
* **annotations**: `readOnly=false, destructive=false, idempotent=true, openWorld=false`
* text block: `Stopped the standalone ScrcpyMac stream.`
* result: `runtime.stop()` = `_stop_resources()` then `status()`. On a clean stop `_meta`
  is cleared, so only the 7 base keys remain. After an *error* stop, `_meta` is retained
  and the device keys are still present with `state:"error"`.
* no try/except — cannot return `isError`.

---

### 3.8 `scrcpymac_ui_snapshot`

* **title**: `Capture ScrcpyMac preview frame`
* **description**: `Capture a compressed frame for the ScrcpyMac widget.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `max_width` | integer | `int` | no | `540` |
| `quality` | integer | `int` | no | `60` |

* **annotations**: `readOnly=true, destructive=false, idempotent=**false**, openWorld=false`
* text block: `Captured a ScrcpyMac preview frame.`
* success keys: `ok, serial, backend, deviceWidth, deviceHeight, frameWidth, frameHeight,
  mimeType, dataBase64, sizeBytes`
* failure: `_error` (catches `AdbError`, `OSError`, `ValueError`)
* clamping happens in the tool **before** use: `max_width → [320, 1200]`,
  `quality → [45, 90]`.
* `PhoneActions.preview_frame` currently **ignores both arguments** and returns a full
  `adb exec-out screencap -p` PNG (`actions.py:129`). `_preview_frame` then:
  downscale to `max_width` with BILINEAR **only if wider**, re-encode JPEG at the clamped
  quality with `optimize=False`, and report `mimeType: "image/jpeg"`.
* If the shot dict ever carries `image_bytes` (a not-yet-wired H.264 path) the bytes pass
  through untouched and `mimeType` comes from `shot["mime_type"]` defaulting to `"image/jpeg"`.
* **The base64 appears only in `structuredContent.dataBase64`, never in the text block** —
  `tests/test_mcp_ui.py` asserts this explicitly.

---

### 3.9 `scrcpymac_ui_tap`

* **title**: `Tap ScrcpyMac preview`
* **description**: `Tap normalized preview coordinates.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `x` | number | `float64` | **yes** | — |
| `y` | number | `float64` | **yes** | — |

* **annotations**: `readOnly=false, destructive=false, idempotent=false, openWorld=false`
* text block: `Tapped the Android screen.`
* **two payload shapes:**
  * runtime active → `runtime.tap_relative` →
    `ok, action, serial, point, coordinateSpace, backend` with `action:"tap"`,
    `backend:"plugin-control"`, `point` in **frame** coordinates, `coordinateSpace`
    `[frameWidth, frameHeight]`. Coordinates are clamped to `[0,1]`.
  * runtime idle → `actions.tap_relative(x, y, verify=False, retries=0)` →
    `ok, coordinate_space, source, device_point, tap`. Out-of-range **raises**
    `relative x and y must be between 0 and 1`.
* failure: `_error`

---

### 3.10 `scrcpymac_ui_swipe`

* **title**: `Swipe ScrcpyMac preview`
* **description**: `Swipe between normalized preview coordinates.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `x1` | number | `float64` | **yes** | — |
| `y1` | number | `float64` | **yes** | — |
| `x2` | number | `float64` | **yes** | — |
| `y2` | number | `float64` | **yes** | — |
| `duration_ms` | integer | `int` | no | `300` |

* **annotations**: `readOnly=false, destructive=false, idempotent=false, openWorld=false`
* text block: `Swiped the Android screen.`
* all four coordinates validated `0.0 <= v <= 1.0` up front →
  `swipe coordinates must be between 0 and 1`
* `duration_ms` clamped to `[0, 10000]`
* **two payload shapes:**
  * runtime active → `ok, action, serial, from, to, durationMs, backend` (**camelCase**
    `durationMs`, `backend:"plugin-control"`, coordinates in frame space)
  * runtime idle → `ok, action, from, to, duration_ms, serial` (**snake_case**, device
    pixels via `round(v * (size - 1))`, no `backend` key)
* the `except` clause additionally catches `KeyError`, `TypeError`, `ValueError`

---

### 3.11 `scrcpymac_ui_key`

* **title**: `Press ScrcpyMac navigation key`
* **description**: `Press an Android navigation or hardware key.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `name` | string | `string` | **yes** | — |

* **annotations**: `readOnly=false, destructive=false, idempotent=false, openWorld=false`
* text block: `Pressed Android key {name}.` (raw argument, not normalised)
* runtime active → `ok, action, key, serial, backend`; runtime idle →
  `ok, action, key, code, serial`
* **the two key tables differ**: `scrcpy_runtime.KEYCODES` has 10 entries (no `paste`),
  `actions.KEYCODES` has 11 (adds `paste` = 279). So `scrcpymac_ui_key("paste")` succeeds
  over adb and fails while streaming.
* failure: `_error`

---

### 3.12 `scrcpymac_ui_paste`

* **title**: `Paste text through ScrcpyMac`
* **description**: `Paste text into the focused Android field.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `text` | string | `string` | **yes** | — |

* **annotations**: `readOnly=false, destructive=false, idempotent=false, openWorld=false`
* text block: `Pasted text into Android.`
* runtime active → `ok, action, length, serial, backend`; idle → `ok, action, length, serial`
* `length` is `len(text)` in **Unicode code points**, not bytes — Go must use
  `utf8.RuneCountInString`.
* empty text → `text must not be empty`

---

### 3.13 `scrcpymac_ui_connect_wifi`

* **title**: `Connect ScrcpyMac over Wi-Fi`
* **description**: `Connect adb to an Android device over Wi-Fi.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `host` | string | `string` | **yes** | — |
| `port` | integer | `int` | no | `5555` |

* **annotations**: `readOnly=false, destructive=false, idempotent=false, **openWorldHint=true**`
  (the only tool in the whole server with `openWorldHint: true`)
* text block: `Connected to Android over Wi-Fi.`
* keys: `ok, action, target, output`; `action:"connect_wifi"`
* `host` is `.strip()`ed here (unlike `phone_connect_wifi`);
  `target = host if ":" in host else f"{host}:{port}"`

---

### 3.14 `phone_backend`

* no title · **description**: `Report whether the standalone H.264 runtime or adb fallback is active.`
* params: none · annotations: **none** · `_meta`: **none** · shape A
* keys: `backend, ok` (note `ok` **last**)
* errors: `{ok:false, error}`

Every `phone_*` tool has `title: null`, `annotations: null` and `meta: null` — none of
them declare any of the three. Only the mcp_ui tools do.

---

### 3.15 `phone_doctor`

* **description**: `Run environment diagnostics for adb, the connected device, and bundled runtime assets.`
* params: none · shape A
* **Not wrapped by `_ok`/`_err`** — it is `json_result(run_doctor())` with **no try/except**,
  so `"ok"` is *first* and an unexpected exception really does produce `isError: true`.
* full keys: `ok, version, backend, checks, summary, uv_available`
* **early-return keys** (adb unresolvable, `doctor.py:48`): `ok, version, checks, summary`
  with `summary: "adb is required before controlling a phone"` — no `backend`, no `uv_available`.
* `checks[]` entries are `{name, ok, detail}` plus optional extras:
  `scrcpy_server`→`bundled` (bool), `adb`→`bundled` (bool), `device`→`devices` (list),
  `foreground_app`→`activity` (string).
* check names, in emission order: `platform`, `python`, `mcp_package`, `scrcpy_server`,
  `adb`, `adb_version`, `device`, then `screen_size` **and** `foreground_app` only when
  exactly one device is in state `device`, then `runtime_architecture`
  (which also carries `backend: "plugin-h264"`).
* `backend` = `"plugin-h264-ready"` when scrcpy-server resolved, else `"adb"`.
* `ok` = all checks ok **excluding** the `foreground_app` check.
* `version` = `"0.7.2"`.
* Live sample (device attached): `ok:true, backend:"plugin-h264-ready", summary:"ready"`.

The Go port has to decide what the `python` and `mcp_package` checks become. They are
part of the model-visible payload; changing their meaning changes the contract. Keeping
the same check *names* with Go-appropriate details is the least surprising option, but it
is an explicit product decision, not something the Go code can infer.

---

### 3.16 `phone_list_devices`

* **description**: `List Android devices visible to adb.`
* params: none · shape A
* keys: `devices, ok`; `devices[]` = `{serial, state, model, product}` (model/product `""` when absent)

---

### 3.17 `phone_device_info`

* **description**: `Get connected device serial, screen size, and foreground app.`
* params: none · shape A
* streaming keys: `serial, screen, video, foreground, backend, ok` with
  `backend:"plugin-h264"`, `screen={width,height}` (native), `video={width,height,fps,codec}`
* adb keys: `serial, screen, foreground, backend, ok` with `backend:"adb"`, no `video`
* `foreground` = `{package, activity, raw}` from
  `dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -1`, parsed with
  `([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)`; all three are `""` when nothing matches.

---

### 3.18 `phone_screenshot` — the tuple return (shape C)

* **description**: `Capture the device screen. Returns metadata JSON and optionally a PNG image.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `include_image` | boolean | `bool` | no | `true` |

* **The only `server.py` tool with no return annotation.** `sig.return_annotation` is
  `inspect.Parameter.empty`, `_try_create_model_and_schema` falls into the
  "other class types" branch, `get_type_hints(inspect._empty)` is empty, so
  **`outputSchema` is `null` and `structuredContent` is `null` on every path.**

`include_image=true` returns the Python tuple `(str, Image)`. FastMCP's `_convert_to_content`
flattens list/tuple results, so the wire result is **two content blocks**:

```jsonc
{
  "content": [
    { "type": "text",  "text": "{\n  \"serial\": \"2f019965\",\n  \"width\": 1080,\n  \"height\": 2280,\n  \"format\": \"png\",\n  \"size_bytes\": 145820,\n  \"ok\": true\n}" },
    { "type": "image", "data": "<base64 PNG, 194428 chars>", "mimeType": "image/png" }
  ],
  "structuredContent": null,
  "isError": false
}
```

Text keys: `serial, width, height, format, size_bytes, ok` (**no `base64`** — the comment
at `server.py:106` says the base64 copy is deliberately skipped).

`include_image=false` returns a single text block whose JSON has the extra key:
`serial, width, height, format, size_bytes, base64, ok`.

On `AdbError`/`OSError`: one text block `{"ok": false, "error": "..."}`, no image block,
`isError` still `false`.

`format` is always the literal `"png"`; `size_bytes` is `len(png)`; the PNG comes from
`adb exec-out screencap -p` with a 30 s timeout.
`width` and `height` are decoded from that PNG's IHDR, so they describe the exact
image coordinate space even when Android has a display-size override.

---

### 3.19 `phone_tap`

* **description**: `Tap native device pixels. By default verifies a screen change and retries nearby.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `x` | integer | `int` | **yes** | — |
| `y` | integer | `int` | **yes** | — |
| `verify` | boolean | `bool` | no | `true` |
| `retries` | integer | `int` | no | `2` |

Shape A. Base payload:
* adb path → `ok, action, x, y, serial` (`action:"tap"`)
* streaming path → `ok, action, serial, point, coordinateSpace, backend`
  (`runtime.tap_relative(x/(w-1), y/(h-1))`, `backend:"plugin-control"`)

`x`/`y` are clamped to the screen first: `max(0, min(size-1, v))`.

With `verify=true` a `verification` object is **added to whichever base payload came
back** (three variants):

| situation | `verification` keys |
| --- | --- |
| baseline screenshot failed | `requested:true, available:false, attempts:1` (a plain int, and exactly one tap is issued) |
| a change was detected | `requested:true, available:true, verified:true, attempts:[...], after_size_bytes:int` |
| no change after every offset | `requested:true, available:true, verified:false, attempts:[...], hint:"No screen change detected after tapping the target and nearby points."` |

`attempts[]` entries are `{point:[x,y], screen_changed:bool, change_score:float}` with
`change_score` rounded to 4 decimals.

Verification algorithm (`actions.py:163-243`), all constants are contract:
* `retries` clamped to `[0, 4]`; offsets are the first `retries+1` of
  `[(0,0), (0,-32), (0,+32), (-32,0), (+32,0)]` (`retry_radius_px=32`, clamped `[1,96]`)
* settle `0.45 s` between tap and re-screenshot (clamped `[0.1, 2.0]`)
* change score: both PNGs resampled to **72x128 RGB**, per-pixel
  `max(r,g,b) >= 20` counted, divided by `72*128`; **changed when `>= 0.035`**

`verify=false` → no `verification` key at all.

---

### 3.20 `phone_tap_relative`

* **description**: `Tap normalized screenshot coordinates where x and y are between 0 and 1.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `x` | number | `float64` | **yes** | — |
| `y` | number | `float64` | **yes** | — |
| `verify` | boolean | `bool` | no | `true` |
| `retries` | integer | `int` | no | `2` |

Shape A. Keys: `ok, coordinate_space, source, device_point, tap`
* `coordinate_space` is always `"relative"`
* `source` is `[x, y]` exactly as supplied
* `device_point` is `[round(x*(w-1)), round(y*(h-1))]`
* `tap` is the nested `phone_tap` payload (including its `verification`)
* out of range → `relative x and y must be between 0 and 1`

---

### 3.21 `phone_tap_image`

* **description** (verbatim, multi-line — FastMCP keeps the full docstring including the
  blank line and the trailing newline produced by `inspect.cleandoc`):

```
Tap a point measured on a displayed/resized screenshot.

Pass the exact width and height of the image coordinate space used to
choose x/y. The tool maps it into current native device pixels.
```

(The wire value ends with `\n`.)

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `x` | integer | `int` | **yes** | — |
| `y` | integer | `int` | **yes** | — |
| `image_width` | integer | `int` | **yes** | — |
| `image_height` | integer | `int` | **yes** | — |
| `verify` | boolean | `bool` | no | `true` |
| `retries` | integer | `int` | no | `2` |

Shape A. Keys: `ok, coordinate_space, source, device_point, device_size, tap`
* `coordinate_space` is always `"image"`
* `source` = `{point:[x,y], size:[image_width,image_height]}`
* `device_point` = `[round(x/max(iw-1,1)*(w-1)), round(y/max(ih-1,1)*(h-1))]`
* `device_size` = `[w, h]`
* errors: `image_width and image_height must be positive` ·
  `image point must be inside image_width x image_height`

---

### 3.22 `phone_swipe`

* **description**: `Swipe from (x1,y1) to (x2,y2).`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `x1`,`y1`,`x2`,`y2` | integer | `int` | **yes** | — |
| `duration_ms` | integer | `int` | no | `300` |

Shape A.
* adb path → `ok, action, from, to, duration_ms, serial` (**snake_case**, no `backend`)
* streaming path → `ok, action, serial, from, to, durationMs, backend`
  (**camelCase**, `backend:"plugin-control"`, `from`/`to` in **frame** coordinates)

The streaming path interpolates `steps = max(1, min(120, round(duration_ms/16)))`
intermediate MOTION events and paces them against `time.monotonic()`.

---

### 3.23 `phone_long_press`

* **description**: `Long press at coordinates.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `x` | integer | `int` | **yes** | — |
| `y` | integer | `int` | **yes** | — |
| `duration_ms` | integer | `int` | no | `1000` |

Shape A. Implemented as `swipe(x, y, x, y, duration_ms)`, so the payload is identical to
`phone_swipe` and **`action` is `"swipe"`, not `"long_press"`**.

---

### 3.24 `phone_key`

* **description**: `Press a key: back, home, recents, enter, delete, tab, menu, power, paste.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `name` | string | `string` | **yes** | — |

Shape A.
* adb path → `ok, action, key, code, serial`; streaming path → `ok, action, key, serial, backend`
  (**no `code`**)
* `key` is `name.lower().strip()`
* accepted (adb): `back`=4, `home`=3, `recents`=187, `enter`=66, `delete`=67, `tab`=61,
  `menu`=82, `power`=26, `volume_up`=24, `volume_down`=25, `paste`=279.
  **The docstring advertises only 9 — `volume_up`/`volume_down` are accepted but undocumented.**
* unknown key, live-verified text:
  `Unknown key 'not_a_key'. Supported: back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down, paste`
  (Python `!r` produces single quotes; the list is `', '.join(KEYCODES)` i.e. dict
  insertion order.)

---

### 3.25 `phone_type`

* **description**: `Type short ASCII text. Do not use for Chinese — use phone_paste instead.`
  (note the U+2014 em dash — `ensure_ascii=False` / raw UTF-8 on the wire)

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `text` | string | `string` | **yes** | — |

Shape A. Keys: `ok, action, length, serial` (`action:"type"`).
* **always the adb path**, even while streaming
* escaping: `text.replace("%", "%25").replace(" ", "%s")` then `shlex.quote` for the
  device shell, sent as `input text <quoted>`
* `length` is the rune count of the **original** text
* empty text → `text must not be empty`

---

### 3.26 `phone_paste`

* **description**: `Paste text via device clipboard (supports Chinese and emoji).`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `text` | string | `string` | **yes** | — |

Shape A.
* adb path → `ok, action, length, serial`: `cmd clipboard set-text <quoted>`, sleep 150 ms,
  `input keyevent 279`
* streaming path → `ok, action, length, serial, backend`: scrcpy control message type 9
  (`>BQBI` = type, sequence 0, paste flag 1, length) followed by the UTF-8 bytes
* `ScrcpyRuntimeError` is converted to `AdbError` inside `actions.paste`, so it is caught
* empty text → `text must not be empty`

---

### 3.27 `phone_launch_app`

* **description**: `Launch an Android app by package name. Example WeChat: com.tencent.mm.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `package` | string | `string` | **yes** | — |
| `activity` | string | `string` | no | `""` |

Shape A. Keys: `ok, action, package, activity, foreground, serial` (`action:"launch"`).
* `server.py` passes `activity or None`, so with the default the emitted `activity` is
  **JSON `null`**, not `""`.
* with an activity: `am start -n {package}/{activity}`; without:
  `monkey -p {package} -c android.intent.category.LAUNCHER 1`
* sleeps **1.0 s** before reading the foreground app

---

### 3.28 `phone_current_app`

* **description**: `Get the foreground app package and activity.`
* params: none · shape A
* keys: `foreground, serial, ok` (`ok` **last**)
* `serial` = `selected_serial()` or the ensured client serial

---

### 3.29 `phone_ui_tree`

* **description** (verbatim, 5 lines, `inspect.cleandoc`-normalised, **no** trailing newline
  because the closing `"""` is on the last text line):

```
Dump the UI accessibility tree as JSON nodes or raw XML.
Nodes carry state flags (scrollable, enabled=false, focused, checked...)
only when noteworthy. If the result has "degraded": true, the tree is
incomplete (WebView/Compose/game) — call phone_screenshot and use vision
instead.
```

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `compact` | boolean | `bool` | no | `true` |

Shape A.
* `compact=true` → `ok, nodes, count, serial` (+ `degraded`, `hint` when degraded)
* `compact=false` → `ok, xml, serial`
* XML parse failure → `ok, xml, serial, parse_error, degraded, hint` with
  `hint: "UI dump was empty or unparseable. Retry phone_ui_tree or fall back to phone_screenshot for vision."`
  and the cache is invalidated (never cache a broken dump)
* degraded heuristic: any raw XML node class contains `WebView` (including roots
  removed by compact filtering), **or** fewer than 3 compact nodes are
  clickable-or-texted →
  `hint: "UI tree looks incomplete (WebView/Compose/custom-drawn). Fall back to phone_screenshot for vision."`

Node key order (`actions.py:461`): `index, text, content_desc, resource_id, class,
clickable, bounds, center`, then only when noteworthy: `scrollable` (true),
`enabled` (false), `password` (true), `focused` (true), `selected` (true),
`checkable` (true) **immediately followed by** `checked` (bool).

* `index` is the position in the *filtered* list, not the XML order.
* `center` is `[(x1+x2)//2, (y1+y2)//2]` parsed from `bounds` `"[x1,y1][x2,y2]"`, or `null`.
* a node is kept only if it has non-empty `text`, non-empty `content-desc`,
  `clickable=true`, `scrollable=true`, `"EditText" in class`, or `checkable=true`.
* dump command: `uiautomator dump /sdcard/window_dump.xml >/dev/null 2>&1 && cat /sdcard/window_dump.xml; rm -f /sdcard/window_dump.xml` (30 s timeout).
  An empty result is retried once after 300 ms.
* the result is cached on the `PhoneActions` instance and invalidated by
  tap / swipe / key / type / paste / launch_app / select_device.

---

### 3.30 `phone_find_and_tap`

* **description** (verbatim, 5 lines, no trailing newline):

```
Find a UI element by visible text, content-desc, resource-id, or class,
then tap it. Provide at least one selector. require_all=True demands every
given selector to hit (use text + resource_id to disambiguate); exact=True
matches whole strings instead of substrings; index=N picks the Nth match;
scroll_to_find=N scrolls down up to N times when the element is off-screen.
```

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `text` | string | `string` | no | `""` |
| `content_desc` | string | `string` | no | `""` |
| `resource_id` | string | `string` | no | `""` |
| `class_name` | string | `string` | no | `""` |
| `require_all` | boolean | `bool` | no | `false` |
| `exact` | boolean | `bool` | no | `false` |
| `index` | integer | `int` | no | `0` |
| `scroll_to_find` | integer | `int` | no | `0` |
| `timeout_s` | number | `float64` | no | `10` |
| `verify` | boolean | `bool` | no | `true` |

Note the parameter order in the schema: `scroll_to_find` comes **before** `timeout_s`.

Shape A. Keys: `ok, matched, tap` — `matched` is the compact tree node, `tap` the nested
`phone_tap` payload.

* no selector supplied → `Provide text, content_desc, resource_id, or class_name`
* matched node without `center` → `Matched node has no tappable bounds ({criteria})`
* timeout →
  `Element not found within {timeout_s}s ({criteria}). Last tree had {n} nodes[, after {k} scroll(s)].`
  If the last tree is degraded, its screenshot-fallback hint is appended.
  The trailing scroll clause is present only when `scroll_to_find` is truthy.
  `{criteria}` is `NodeCriteria.describe()`: comma-joined `name=<python repr of a list of
  str>` for each non-empty selector in the order text, content_desc, resource_id, class,
  then optionally `require_all=True`, `exact=True`. Go must reproduce the Python list-repr
  (e.g. `text=['Search']`).
* matching: substring by default, whole-string when `exact`; a node matching **any**
  specified attribute wins unless `require_all`; disabled nodes (`enabled: false`) are
  always skipped; empty needles never match.
* polling: base interval `0.4 s`, backoff `0.4 * 1.5^(attempt-1)` capped at `2.0 s`;
  the first probe uses the cache, subsequent probes force a refresh; a scroll consumes an
  attempt but re-dumps immediately with **no** sleep.
* a scroll is a mid-screen swipe from 70 % to 30 % height over 350 ms plus a 0.4 s settle.

---

### 3.31 `phone_wait_for_text`

* **description**: `Wait until the given text appears in the UI tree.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `text` | string | `string` | **yes** | — |
| `timeout_s` | number | `float64` | no | `10` |

Shape A. Keys: `ok, found, serial`. Matches by substring on the `text` attribute only.
`serial` falls back to the client serial when the tree carries none. Same poll/backoff as
`phone_find_and_tap` but with `scroll_to_find = 0`.

---

### 3.32 `phone_shell`

* **description**: `Execute an adb shell command on the device.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `command` | string | `string` | **yes** | — |

Shape A. Keys: `ok, output, serial`. `output` is the **trimmed** stdout; 60 s timeout;
non-zero exit becomes `AdbError` → `{ok:false, error:"adb shell <cmd> failed: <detail>"}`.

---

### 3.33 `phone_send_wechat`

* **description**: `High-level recipe: open WeChat, find a contact, and send a message.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `contact` | string | `string` | **yes** | — |
| `message` | string | `string` | **yes** | — |

Shape A. Keys: `ok, recipe, contact, message, steps, verification`.
* `recipe` is always `"wechat_send_message"`
* `verification` = `{width, height, size_bytes}` from a final screenshot
* `steps[]` is heterogeneous:
  `{step, result}` for `launch_wechat`, `wait_wechat_ready`, `open_search`,
  `wait_search_input`, `open_chat`, `tap_send`;
  `{step, contact}` for `type_contact`; `{step, length}` for `type_message`;
  `{step, key}` for `open_chat_fallback` / `send_fallback`;
  `{step, skipped}` for skipped waits.
* constants: package `com.tencent.mm`; search labels `["搜索", "Search"]`; send labels
  `["发送", "Send"]`; home markers `["微信","通讯录","发现","我","WeChat","Chats","Contacts"]`;
  default `timeout_s = 15`.
* on failure the message is rewritten before it reaches `_err`:
  `{exc}. Steps completed: {n}. Last screenshot: {w}x{h}`

---

### 3.34 `phone_enable_wifi_adb`

* **description**: `Enable TCP/IP adb on a USB-connected device (required before Wi-Fi connect).`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `port` | integer | `int` | no | `5555` |

Shape A. Keys: `ok, action, port, output, serial`; `action:"enable_tcpip"`;
`output` is the trimmed stdout of `adb shell tcpip <port>`.

---

### 3.35 `phone_get_device_ip`

* **description**: `Get the device's Wi-Fi IP address (for wireless adb).`
* params: none · shape A
* keys: `ok, ip, serial`
* probes `ip route | awk '/wlan/ {print $9; exit}'` then
  `ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1`;
  both must match `^\d+\.\d+\.\d+\.\d+$`
* failure → `Could not detect device Wi-Fi IP. Is Wi-Fi connected?`

---

### 3.36 `phone_connect_wifi`

* **description**: `Connect to a device over Wi-Fi adb. Example host: 192.168.1.100.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `host` | string | `string` | **yes** | — |
| `port` | integer | `int` | no | `5555` |

Shape A. Keys: `ok, action, target, output`; `action:"connect_wifi"`;
`target = host if ":" in host else f"{host}:{port}"`.
**`host` is NOT stripped here** (unlike `scrcpymac_ui_connect_wifi`).
Uses `self.client` without `ensure_device()`, so it works with no device attached.

---

### 3.37 `phone_disconnect_wifi`

* **description**: `Disconnect a Wi-Fi adb session. Leave host empty to disconnect all.`

| param | type | Go type | required | default |
| --- | --- | --- | --- | --- |
| `host` | string | `string` | no | `""` |

Shape A. Keys: `ok, action, output`; `action:"disconnect_wifi"`.
Empty host → `adb disconnect` (all). A non-empty host without `:` gets **`:5555`**
appended unconditionally — this tool has no `port` parameter.

---

## 4. The widget resource

| field | value |
| --- | --- |
| `uri` | `ui://widget/scrcpymac/app.html` |
| `name` | `scrcpymac-app` |
| `title` | `ScrcpyMac` |
| `description` | `Interactive Android mirroring and control workspace.` |
| `mimeType` | `text/html;profile=mcp-app` |

`resources/read` returns a single `TextResourceContents` with that `uri`, that `mimeType`,
the **same `_meta`** as the `resources/list` entry, and the full text of
`go/internal/widget/assets/scrcpymac-app.html` (self-contained: no
`<script src=>`, no `<link href=>`). A missing file raises
`FileNotFoundError("ScrcpyMac widget build is missing. Run scripts/build-ui.sh.")`.

`_meta` (from `_resource_meta`, `mcp_ui.py:21`):

```jsonc
{
  "ui": {
    "prefersBorder": false,
    "csp": {
      "connectDomains": [
        "http://127.0.0.1:<PORT>", "ws://127.0.0.1:<PORT>",
        "http://localhost:<PORT>", "ws://localhost:<PORT>",
        "http://127.0.0.1:*",      "ws://127.0.0.1:*",
        "http://localhost:*",      "ws://localhost:*"
      ],
      "resourceDomains": ["data:", "blob:"]
    }
  },
  "openai/widgetDescription": "Full ScrcpyMac phone control workspace.",
  "openai/widgetPrefersBorder": false,
  "openai/widgetCSP": {
    "connect_domains": [ /* the identical 8-entry list */ ],
    "resource_domains": ["data:", "blob:"]
  }
}
```

Note the deliberate key-style split: the `ui.csp` block uses **camelCase**
(`connectDomains`/`resourceDomains`) while `openai/widgetCSP` uses **snake_case**
(`connect_domains`/`resource_domains`). Both carry the same values.

### The registration-time port dependency (live suspect in the 1 FPS bug)

`register_scrcpymac_app` computes `resource_meta = _resource_meta(runtime.stream_connect_domains())`
on its **first line** (`mcp_ui.py:123`). `stream_connect_domains` calls `_ensure_loopback()`,
which binds `127.0.0.1:0` **immediately at import of `phone_agent.server`** — before the
first tool is registered, long before any device is selected or any stream started.
So:

1. The literal port baked into the CSP is whatever the OS handed out at process start.
2. `ScrcpyRuntime.stop()` does **not** tear the loopback down, so during normal
   start/stop cycles the port stays valid.
3. `ScrcpyRuntime.close()` (atexit + SIGTERM) sets `_loopback = None`. Any later
   `_ensure_loopback()` binds a **different** port while the already-published resource
   `_meta` still advertises the old one — the CSP would then be stale and the widget's
   WebSocket blocked.
4. What actually saves the current build are the four wildcard entries
   (`http://127.0.0.1:*`, `ws://127.0.0.1:*`, and the `localhost` variants). If the host
   ever tightens wildcard handling, the stream dies silently.

**The Go port must reproduce all 8 entries, and should compute them from the loopback
listener at registration time exactly as the Python does — or, better, bind the loopback
once at startup and keep it for the process lifetime so the literal port can never go
stale.** Recording the ordering here is the point: it is contract-visible behaviour, not
an implementation detail.

---

## 5. Gotchas for the Go reimplementation

1. **`structuredContent` differs by tool family.** `phone_*` → `{"result": "<json string>"}`
   plus a declared `outputSchema`. `open_scrcpymac` / `scrcpymac_ui_*` → the payload object
   itself with **no** `outputSchema`. `phone_screenshot` → `null`. Getting this backwards
   breaks either the widget or Codex's output validation.

2. **`isError` is almost never set.** All 24 `phone_*` tools return `isError: false` even
   when `ok` is `false`. Among the `mcp_ui` tools, only the `_error()` paths set
   `isError: true`; `scrcpymac_ui_state`'s failure branch does **not**, and
   `open_scrcpymac`, `scrcpymac_ui_stream_status` and `scrcpymac_ui_stop_stream` have no
   error path at all.

3. **JSON key order is contract.** `json_result` uses insertion order, never sorted. Go's
   `map[string]any` marshals **alphabetically**, which would reorder every payload. Use
   ordered structs (or a custom ordered marshaller) and mirror the `_ok` `setdefault`
   quirk: `ok` last for `phone_backend` / `phone_list_devices` / `phone_device_info` /
   `phone_screenshot` / `phone_current_app`, `ok` first everywhere else.

4. **`json_result` formatting.** `json.dumps(..., ensure_ascii=False, indent=2)`:
   2-space indent, `": "` between key and value, `,\n` between items, **no trailing
   newline**, raw UTF-8 for non-ASCII. Go's `json.MarshalIndent` matches the layout but
   escapes `<`, `>` and `&` — you must use an `Encoder` with `SetEscapeHTML(false)` and
   strip the newline it appends. Also: Python `[]`/`{}` stay `[]`/`{}`; a Go `nil` slice
   marshals to `null`, so initialise empty slices.

5. **Floats vs ints in `structuredContent`.** `status()["fps"]` is `round(x, 1)` — a Python
   float, serialised `0.0`. Go marshals `float64(0)` as `0`. If a Go test byte-compares,
   normalise; if the widget compares, it does not care. Decide once and document it.

6. **`phone_screenshot` emits two content blocks**, in order text-then-image, with the
   image as `{"type":"image","data":<std base64>,"mimeType":"image/png"}`. Never inline
   the base64 into the text block on that path; do inline it (key `base64`) when
   `include_image=false`.

7. **snake_case vs camelCase leaks through the backend split.** The same tool returns
   `duration_ms` over adb and `durationMs` over the plugin control socket
   (`phone_swipe`, `scrcpymac_ui_swipe`); `phone_key` returns `code` only on the adb path;
   the plugin paths add `backend: "plugin-control"`. This is ugly but it is the contract.

8. **Two different keycode tables.** `actions.KEYCODES` has 11 entries including
   `paste`=279; `scrcpy_runtime.KEYCODES` has 10 and omits `paste`. The "Supported:" list
   in the error message is the joined dict order of whichever table raised.

9. **`phone_launch_app` emits `activity: null`** for the default `""`, because `server.py`
   passes `activity or None`.

10. **`phone_long_press` reports `action: "swipe"`.** Do not "fix" it.

11. **Python `round()` is banker's rounding** (half-to-even). Every `round(v*(n-1))`
    coordinate mapping in `actions.py` and `scrcpy_runtime.py` uses it. Go's `math.Round`
    rounds half away from zero, so `round(0.5)` is `0` in Python and `1` in Go. Use
    `math.RoundToEven` for these mappings.

12. **String lengths are rune counts.** `len(text)` in `phone_type` / `phone_paste` /
    `scrcpymac_ui_paste` counts Unicode code points.

13. **Error strings are part of the model-visible payload.** They are quoted verbatim in
    §3; reproduce them exactly, including Python `!r` single quotes
    (`Unknown key 'foo'.`) and the list-repr in `NodeCriteria.describe()`
    (`text=['Search']`).

14. **`serverInfo.version`.** FastMCP reports the *SDK* version (`1.28.1`), which is an
    accident of the Python implementation rather than an intentional contract. Reporting
    the plugin version (`0.7.2`) from Go is the sane choice, but it is a visible change —
    flag it in the cutover notes rather than letting it slip through.

15. **`phone_doctor`'s `python` and `mcp_package` checks** have no Go equivalent. They are
    model-visible. Keep the check *names* and give them Go-appropriate `detail` values, or
    deliberately rename them — but make it a decision, not a silent drop. Everything else
    in that payload (`version`, `backend`, `summary`, `uv_available`, the `bundled` flags,
    the exact check order, and the "exclude `foreground_app` from `ok`" rule) is
    reproducible as-is.

16. **Tool visibility.** `visibility: ["app"]` on the eleven `scrcpymac_ui_*` tools is the
    only thing keeping them out of the model's tool list. A Go SDK that drops unknown
    `_meta` keys would expose all eleven to Codex. Assert the `_meta` round-trips.

17. **Registration order** (mcp_ui first, then `phone_*` in source order) is what
    `contract.json.toolOrder` records; keep it so diffs stay readable and so the loopback
    bind still happens before the first tool is registered.
