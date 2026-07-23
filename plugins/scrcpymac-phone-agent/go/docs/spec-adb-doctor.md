# Spec: adb wrapper, doctor diagnostics, and launcher surface (Go port)

Scope: everything needed to reimplement, in Go, the behaviour currently in

- `server/phone_agent/adb.py` (234 lines)
- `server/phone_agent/doctor.py` (108 lines)
- `bin/phone-agent`, `scripts/ensure-runtime.sh`, `scripts/download-adb.sh`, `scripts/doctor.sh`,
  `scripts/install.sh`, `scripts/configure.sh`, `scripts/build-ui.sh`
- the parts of `server/phone_agent/scrcpy_runtime.py` that these touch
  (`resolve_scrcpy_server_path()`, line 67, and the adb calls the runtime makes)

Everything in "Current behaviour" sections was read out of the real files or probed against the
attached OnePlus 6 (`2f019965`, adb 1.0.41 / 37.0.0-14910828). Nothing is inferred.

Target Go package: `github.com/zjywill/scrcpyMac/phone-agent`, source under
`plugins/scrcpymac-phone-agent/go`.

Suggested Go layout for this slice:

```
go/
  cmd/phone-agent/main.go        // subcommand dispatch (mcp|doctor|devices|version)
  internal/adb/adb.go            // Client, Device, Error, resolution
  internal/adb/resolve.go        // adb binary discovery
  internal/doctor/doctor.go      // run_doctor equivalent + ordered JSON
  internal/paths/root.go         // PHONE_AGENT_ROOT + scrcpy-server resolution
  internal/jsonx/ordered.go      // insertion-ordered JSON object marshalling
```

---

## 0. Contract-compatibility ground rules for this slice

These bite before any individual field does.

1. **JSON encoding.** Every JSON string the Python emits comes from
   `actions.json_result()` (`actions.py:815`):
   `json.dumps(payload, ensure_ascii=False, indent=2)`.
   Go equivalent: `encoding/json` `Encoder` with `SetEscapeHTML(false)` and
   `SetIndent("", "  ")`. **`json.Marshal`/`MarshalIndent` default to escaping `<`, `>`, `&` as
   `<` etc. — that is a visible diff and must be turned off.** `ensure_ascii=False` means the
   em-dash `—` (U+2014) in two doctor messages is emitted literally as UTF-8, not `—`.
   `json.dumps` also emits no trailing newline; `print()` in the launcher adds exactly one.
2. **Key order.** Python `dict` preserves insertion order and `json.dumps` honours it. Go maps do
   not, and `encoding/json` sorts map keys alphabetically. Doctor check objects have a fixed prefix
   (`name`, `ok`, `detail`) followed by *variable* extra keys (`bundled`, `devices`, `activity`)
   appended in kwargs order. Implement an insertion-ordered object type
   (`type Obj []struct{K string; V any}` with a `MarshalJSON`) or a per-check struct with
   `omitempty`-controlled field order. Do not use `map[string]any`.
3. **Numbers.** All integers in this slice are small; `encoding/json` renders Go `int` identically
   to Python `int`. Floats do not appear in doctor/devices output.
4. **`ok` is a real bool**, never a string.
5. **Version string.** `phone_agent/__init__.py` → `__version__ = "0.7.2"`.
   `server/tests/test_packaging.py::test_versions_match_across_package_metadata` asserts this is
   equal to `.agents/plugins/marketplace.json` (`metadata.version` *and* the plugin entry's
   `version`), `.codex-plugin/plugin.json`, `.cursor-plugin/plugin.json`,
   `server/pyproject.toml` and `ui/package.json`. **The Go binary's version constant joins that
   set** — add it to the packaging test (or its Go replacement) or the versions will silently drift.

---

## 1. `adb.py` — the adb wrapper

### 1.1 `Device`

```python
@dataclass
class Device:
    serial: str
    state: str
    model: str = ""
    product: str = ""
```

`to_dict()` returns, **in this order**: `serial`, `state`, `model`, `product`. Missing fields are
`""`, never `null` and never absent.

This dict is the element type of:
- `phone_list_devices` → `{"ok": true, "devices": [ ... ]}`
- `bin/phone-agent devices` → a **bare JSON array** of these dicts (no wrapper object)
- `doctor` check `device` → the `devices` extra key
- `PhoneActions.select_device()` → `device` key

Go:

```go
type Device struct {
    Serial  string `json:"serial"`
    State   string `json:"state"`
    Model   string `json:"model"`
    Product string `json:"product"`
}
```

Struct field order gives the required JSON key order. **Do not add `device` or `transport_id`
fields** — see §1.6.

### 1.2 `AdbError` and its exact messages

`class AdbError(RuntimeError)`. In `server.py` and `mcp_ui.py` every tool catches
`(AdbError, OSError)` and returns `json_result({"ok": False, "error": str(exc)})`. So **the message
text is part of the wire contract** — it is what the model sees. Reproduce verbatim.

| Origin | Message (exact) |
|---|---|
| `resolve_adb_path()` (adb.py:94) | `adb not found. Install Android platform-tools, run scripts/install.sh, or set ADB_PATH.` |
| `AdbClient.run()` timeout (adb.py:129) | `adb timed out: <full argv joined by single spaces>` |
| `AdbClient.run()` non-zero exit (adb.py:135) | `adb <args joined by spaces> failed: <detail>` |
| `ensure_device()` none (adb.py:167) | `No Android device connected. Plug in USB or run adb connect.` |
| `ensure_device()` many (adb.py:170) | `Multiple devices connected (<s1>, <s2>). Set PHONE_AGENT_SERIAL or pass serial.` |
| `screen_size()` (adb.py:184) | `Could not parse screen size from: <python repr of output>` |
| `device_wifi_ip()` (adb.py:234) | `Could not detect device Wi-Fi IP. Is Wi-Fi connected?` |

Subtleties:

- **Timeout message uses the FULL command** — `[adb_path] + ["-s", serial]? + args`, i.e. it leaks
  the absolute adb path and the serial.
- **Failure message uses only `args`** — no adb path, no `-s <serial>`. Different from the timeout
  message. Do not unify them.
- `detail` = `stderr.strip()` if non-empty, else `stdout.strip()` if non-empty, else
  `exit <returncode>` (literal, e.g. `exit 1`). `_as_text()` decodes bytes as UTF-8 with
  `errors="replace"` (Go: `strings.ToValidUTF8(s, "�")`), and maps `None`/empty to `""`.
- **`screen_size` uses Python `!r`**, i.e. `repr()`. For a normal ASCII string with no embedded
  single quotes that is `'...'` — single quotes. Go `%q` produces `"..."`. To match, emit
  `"Could not parse screen size from: '" + out + "'"` for the common case; a faithful port of
  `repr()` (backslash-escaping, quote-flipping when the string contains `'`) is overkill — document
  the simplification. Note the output is already `.strip()`ed by `shell()`.
- Serials in the "Multiple devices" message are joined with `", "` in `list_devices()` order.
- The `resolve_adb_path` message spans two source lines; the concatenation yields exactly one space
  between `scripts/install.sh,` and `or set ADB_PATH.`

Go shape:

```go
type Error struct{ msg string }
func (e *Error) Error() string { return e.msg }
```

Use a distinct type so callers can `errors.As` where Python does `except AdbError`. Note that the
Python tool layer catches `(AdbError, OSError)` — the Go equivalent should surface *any* error from
this package plus `os`/`exec` errors through the same `{"ok": false, "error": ...}` envelope, so in
practice a catch-all at the tool boundary is correct.

### 1.3 adb binary discovery — `resolve_adb_path()` (adb.py:70)

Exact current precedence:

1. `env := os.Getenv("ADB_PATH")`; **if empty**, `env = os.Getenv("ANDROID_ADB")`
   (Python `a or b`, so an empty-string `ADB_PATH` falls through to `ANDROID_ADB`).
   Accept only if `os.path.isfile(env) and os.access(env, os.X_OK)`.
   **If `ADB_PATH` is set but not an executable file, it is silently ignored** — no error, no
   warning; resolution continues to step 2.
2. `_bundled_adb_path()` — **only if `PHONE_AGENT_ROOT` is set and non-empty**. Otherwise skipped
   entirely.
   - `platform.system().lower() == "darwin"`: try, **in order**
     1. `$ROOT/bin/darwin/adb`
     2. `$ROOT/bin/darwin/<arch>/adb` where `<arch>` is `arm64` if
        `platform.machine().lower() in ("arm64","aarch64")` else `x86_64`
        (**note: any other machine value falls into the `x86_64` bucket**, including `i386`).
     Each candidate must be `isfile` *and* `X_OK`.
   - `platform.system().lower() == "linux"` **and** `machine in ("x86_64","amd64")`:
     `$ROOT/bin/linux/x86_64/adb`, same isfile+X_OK check.
   - No Windows branch. Anything else → `None`.
3. `shutil.which("adb")` — plain `PATH` lookup, executable-bit checked by `which`.
4. Hard-coded candidates, in order, each `isfile` + `X_OK`:
   1. `~/Library/Android/sdk/platform-tools/adb` (tilde expanded via `os.path.expanduser`, i.e.
      `$HOME`)
   2. `/opt/homebrew/bin/adb`
   3. `/usr/local/bin/adb`
   4. `/usr/bin/adb`
5. Raise `AdbError` with the "adb not found." message.

**Resolution is not cached** in Python: `resolve_adb_path()` runs on *every* `AdbClient()`
construction (adb.py:102), and `AdbClient()` is constructed lazily per `PhoneActions` and freshly in
`ScrcpyRuntime.start()` (scrcpy_runtime.py:418) and three times in `doctor.py`. It is cheap
(a handful of `stat`s), so caching in Go is fine **provided the cache is invalidated if
`ADB_PATH`/`PHONE_AGENT_ROOT` change**; since the Go binary is a single long-lived process and
nothing mutates those, resolve once at start-up and store it. Doctor must still report the resolved
path.

**Interaction with the launcher (important).** `bin/phone-agent` (lines 10–39, called at line 77
before dispatch) pre-exports `ADB_PATH=$ROOT/bin/<platform>/<arch>/adb` when that file is
executable — and **only the arch-specific path; it never considers `$ROOT/bin/darwin/adb`**. So in
the real, launcher-driven process the effective order is:

```
user ADB_PATH  >  bin/<os>/<arch>/adb  >  bin/darwin/adb  >  PATH  >  hard-coded candidates
```

whereas the library alone prefers `bin/darwin/adb` over the arch mirror. `scripts/download-adb.sh`
makes the two byte-identical copies of the same universal macOS binary, so today the difference is
unobservable.

**Go decision (flagged):** the target package layout ships a single universal
`bin/darwin/adb` and *no* arch mirrors, so the Go binary should use the **library order**
(universal first), which is also the order that finds the shipped file:

```
1. $ADB_PATH (if isfile+executable), else $ANDROID_ADB (same test)
2. $ROOT/bin/<goos>/adb                     // target layout, universal on darwin
3. $ROOT/bin/<goos>/<arch>/adb              // legacy mirrors from download-adb.sh
4. exec.LookPath("adb")
5. ~/Library/Android/sdk/platform-tools/adb, /opt/homebrew/bin/adb, /usr/local/bin/adb, /usr/bin/adb
6. Error("adb not found. Install Android platform-tools, run scripts/install.sh, or set ADB_PATH.")
```

`<goos>` = `runtime.GOOS` (`darwin`/`linux`); `<arch>` = `arm64` for `runtime.GOARCH=="arm64"`,
`x86_64` for `amd64`. Keep the linux branch restricted to `x86_64`/`amd64` as today. Executability
test: `unix.Access(p, unix.X_OK)` or `fi.Mode()&0111 != 0 && fi.Mode().IsRegular()`.

Steps 2–3 must be skipped when `PHONE_AGENT_ROOT` resolves to empty. In the Go binary
`PHONE_AGENT_ROOT` is *derived* if unset (§3.2), so unlike Python it will essentially always be
available — this is a behaviour improvement, flagged.

### 1.4 `AdbClient` construction

```python
AdbClient(serial: Optional[str] = None, adb_path: Optional[str] = None)
    self.adb_path = adb_path or resolve_adb_path()      # raises here if adb missing
    self.serial   = serial or os.environ.get("PHONE_AGENT_SERIAL")   # may be None
```

- `serial` may be `None`; `PHONE_AGENT_SERIAL=""` is falsy and therefore treated as unset.
- `self.serial` is **mutable** — `select_device()` (actions.py:72) and `ensure_device()` write it.
  The Go `Client` must have the same mutable serial and must be safe for the concurrent access the
  MCP server does (`PhoneActions` is a process singleton behind `_get_actions()`); guard with a
  mutex.

`_base_cmd()` = `[adb_path]` + `["-s", serial]` when `serial` is truthy.

### 1.5 `run` / `run_bytes` — exact invocation semantics

```python
run(args, *, check=True, text=True, timeout=60) -> CompletedProcess
```

- Command: `_base_cmd() + args`. **`args` is an argv list, never a shell string.** No shell is
  involved anywhere in `adb.py` (`shlex` appears in `actions.py` only).
- `subprocess.run(cmd, capture_output=True, text=text, timeout=timeout, check=False)`:
  - stdout **and** stderr are both captured to pipes and fully drained; neither is inherited.
  - `text=True` decodes with the locale-preferred encoding (UTF-8 on macOS) **and enables universal
    newlines** — `\r\n` is translated to `\n`. Android `adb shell` output frequently contains
    `\r\n`. **Go's `exec.Cmd.Output()` does not do this.** Anywhere the Python compares or regexes
    text output, Go must strip `\r` first. Concretely: `strings.ReplaceAll(s, "\r\n", "\n")` then
    also drop lone `\r` (Python's universal newlines maps lone `\r` → `\n` too). Apply it in
    `run()` for the text path only, never for `run_bytes` (screenshots are binary).
  - `text=False` (used by `run_bytes`) returns raw bytes with **no** newline translation. This is
    critical for `screenshot_png()` — a PNG mangled by CRLF translation is corrupt.
- Default timeout **60 s**. Callers that override: `screenshot_png()` 30 s, `ui_tree_xml()` 30 s,
  `scrcpy_runtime` `push` 60 s (explicit), `forward --remove` on teardown 10 s
  (`scrcpy_runtime.py:907`), everything else 60 s. `timeout=None` is accepted by the signature but
  no caller passes it.
- On `subprocess.TimeoutExpired`: Python's `subprocess.run` **kills the child and reaps it** before
  re-raising. Go must do the same — `exec.CommandContext` with `context.WithTimeout` sends
  `SIGKILL` (set `Cmd.Cancel`/`WaitDelay` if a graceful `SIGTERM` first is wanted; Python sends
  `SIGKILL` via `Popen.kill()`, so plain `CommandContext` matches). **Never leave an orphaned adb
  child** — the concurrent stream investigation on this device makes leaked `adb shell` processes
  actively harmful.
- `check=True` (the default) raises on non-zero. `check=False` is **never used by any caller** in
  the current tree (grep confirms); keep the parameter for parity but the Go API can expose
  `Run(args...) (Result, error)` and `RunNoCheck(args...) (Result, error)`.
- No environment scrubbing: the child inherits the parent env verbatim, including
  `ANDROID_SERIAL` if the user has it set. Note `ANDROID_SERIAL` is *not* read by this code but adb
  itself honours it — when `self.serial` is empty and `ANDROID_SERIAL` is exported, adb will target
  that device and `ensure_device()`'s "multiple devices" guard is bypassed. Preserve this
  (inherit the env) rather than "fixing" it; document it.
- No `cwd` is set — the child inherits the server's working directory.

`run_bytes(args, timeout=60)` = `run(args, text=False, timeout=timeout).stdout`. Note `check`
defaults to `True` here too, so a failed `exec-out screencap` raises with `detail` taken from the
**bytes** stderr via `_as_text`.

Proposed Go surface:

```go
type Result struct {
    Stdout   string // CRLF-normalised
    Stderr   string // CRLF-normalised
    Raw      []byte // undecoded stdout, for run_bytes callers
    ExitCode int
}
func (c *Client) Run(ctx context.Context, args ...string) (Result, error)      // check=True
func (c *Client) RunBytes(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error)
func (c *Client) Shell(ctx context.Context, command string) (string, error)
```

### 1.6 `list_devices()` — `adb devices -l` parsing

Current implementation (adb.py:141):

```python
result = self.run(["devices", "-l"])
for line in (result.stdout or "").splitlines()[1:]:     # blindly drop line 0
    line = line.strip()
    if not line: continue
    parts = line.split()                                # split on any whitespace runs
    if len(parts) < 2: continue
    serial, state = parts[0], parts[1]
    for token in parts[2:]:
        if token.startswith("model:"):   model   = token.split(":",1)[1]
        elif token.startswith("product:"): product = token.split(":",1)[1]
```

Note it does **not** pass `-s <serial>`… actually it does: `_base_cmd()` prepends `-s <serial>` when
a serial is set. `adb -s X devices -l` is harmless (adb ignores `-s` for `devices`), but be aware
the argv is not always just `adb devices -l`.

Real output from the attached device:

```
List of devices attached
2f019965               device usb:1-1 product:OnePlus6 model:ONEPLUS_A6000 device:OnePlus6 transport_id:22
```

Long-form (`-l`) token vocabulary, in the order adb emits them:

| token | meaning | consumed by Python? |
|---|---|---|
| `<serial>` | positional 0. USB serial, or `host:port` for TCP/IP devices (e.g. `192.168.1.7:5555`) | yes → `serial` |
| `<state>` | positional 1. See state vocabulary below | yes → `state` |
| `usb:1-1` | USB bus/port path. **Absent for TCP/IP devices.** | **no — discarded** |
| `product:OnePlus6` | `ro.product.name` | yes → `product` |
| `model:ONEPLUS_A6000` | `ro.product.model`, spaces replaced by `_` by adb | yes → `model` |
| `device:OnePlus6` | `ro.product.device` (board/codename) | **no — discarded** |
| `transport_id:22` | adb-server-local transport handle, changes on every reconnect | **no — discarded** |

For non-`device` states adb prints only `<serial> <state>` plus, for USB, `usb:...` — the
`product:`/`model:`/`device:` triplet is only present once the device is authorised, so
`model`/`product` are `""` for `unauthorized`/`offline` entries. That is exactly why the dataclass
defaults them to `""`.

**State vocabulary** (adb `transport.cpp`): `device`, `offline`, `unauthorized`, `authorizing`,
`connecting`, `bootloader`, `host`, `recovery`, `rescue`, `sideload`, `no permissions`, `unknown`.
Two of these are hazardous for this parser: **`no permissions`** contains a space, so
`parts[1] == "no"` and the rest is treated as long-form tokens; and the long
`no permissions; see [http://...]` suffix likewise. Python has this bug today. Recommend the Go
parser special-case a `state` field of `no` followed by `permissions` and join them into
`no permissions`, and document the deviation — the resulting `state` string then matches adb's own
`adb devices` (non-`-l`) rendering. Everything downstream only ever tests `state == "device"`
(`actions.py:70`, `doctor.py:65`, `adb.py:165`), so the change is safe.

**Header handling.** The Python drops line 0 unconditionally. When the adb server is not yet
running, adb 1.0.41 prints

```
* daemon not running; starting now at tcp:5037 *
* daemon started successfully *
```

before `List of devices attached`. Those lines split into ≥2 tokens, so the current parser would
synthesise a bogus `Device(serial="*", state="daemon")`. On adb 1.0.41 these messages go to
**stderr**, so the bug is latent, not live — but it is a real robustness gap and the destination has
varied across adb versions.

**Go deviation (flagged, safe):** instead of `[1:]`, scan for the line equal to
`List of devices attached` (after trimming) and parse everything after it; if that header is absent,
fall back to skipping line 0. Additionally skip any line whose first token is `*`. No JSON key
changes.

Ordering: adb lists devices in transport order; the Python preserves it and so must Go (it decides
which serial `ensure_device()` auto-selects when exactly one is ready, and the join order of the
"Multiple devices" message).

### 1.7 `ensure_device()`

```python
if self.serial: return self.serial          # NO validation — a stale/bogus serial is accepted
devices = [d for d in self.list_devices() if d.state == "device"]
0  -> AdbError("No Android device connected. Plug in USB or run adb connect.")
>1 -> AdbError(f"Multiple devices connected ({', '.join(serials)}). Set PHONE_AGENT_SERIAL or pass serial.")
1  -> self.serial = devices[0].serial; return it
```

The "already have a serial → return immediately" short-circuit is important: once
`PHONE_AGENT_SERIAL` is set or `select_device()` has run, `ensure_device()` performs **no adb call
at all**. Preserve that — it is on the hot path of every action.

Contrast `PhoneActions.select_device()` (actions.py:62), which *does* validate: it lists devices,
`AdbError(f"Android device not found: {serial}")` if absent, and
`AdbError(f"Android device is {state}: {serial}")` if the state is not `device`.

### 1.8 `shell()`

```python
def shell(self, command: str, *, timeout=60) -> str:
    return self.run(["shell", command], timeout=timeout).stdout.strip()
```

**`command` is passed as a single argv element.** adb concatenates the post-`shell` argv with spaces
and hands the result to the device's `/system/bin/sh`, so the *device* shell performs word
splitting, globbing, pipes and redirection. This is load-bearing:
`ui_tree_xml()` relies on `&&`, `;`, `>` and `2>&1`, and `current_app()` relies on `|`.
Go must emit exactly `[]string{"shell", command}` — **never** split `command` on the host side.

Return value is `stdout` only (stderr is captured and dropped unless the exit code is non-zero),
`.strip()`ed of leading/trailing whitespace including the trailing `\n`.

Exit-code propagation: adb 1.0.41 propagates the device command's exit status, so a failing device
command raises `AdbError`. Verified live: `adb -s 2f019965 shell "tcpip 5555"` →
stderr `/system/bin/sh: tcpip: inaccessible or not found`, exit 127.

### 1.9 `screen_size()`

The original Python searched the first `WxH` in `wm size`. When a display
override is active, that output is:

```
Physical size: 1080x2280
Override size: 720x1520
```

That old behavior reported the physical size and shifted every screenshot-based
tap. The Go implementation intentionally queries WindowManager's `cur=WxH`
coordinate space first, then falls back to Override size, Physical size, and a
generic legacy `WxH` match. This also handles landscape rotation.

### 1.10 Remaining `AdbClient` methods (needed by other specs, listed for completeness)

| method | argv / device command | timeout | returns |
|---|---|---|---|
| `current_app()` | `shell "dumpsys window \| grep -E 'mCurrentFocus\|mFocusedApp' \| head -1"` | 60 | `{"package","activity","raw"}` — regex `([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)`, first match; **never raises on no-match**, returns `""`/`""` with `raw` = full output |
| `screenshot_png()` | `exec-out screencap -p` (binary) | **30** | raw PNG bytes, no newline translation |
| `ui_tree_xml()` | `shell "uiautomator dump /sdcard/window_dump.xml >/dev/null 2>&1 && cat /sdcard/window_dump.xml; rm -f /sdcard/window_dump.xml"` | **30** | XML text, stripped. One round trip by design; the `; rm -f` always runs, so a failed dump still returns exit 0 with empty stdout |
| `connect_wifi(host, port=5555)` | `connect <target>` where `target = host if ":" in host else f"{host}:{port}"` | 60 | stdout stripped |
| `disconnect_wifi(host="")` | `disconnect` or `disconnect <host or host:5555>` | 60 | stdout stripped |
| `enable_tcpip(port=5555)` | `shell "tcpip <port>"` | 60 | stdout stripped |
| `device_wifi_ip()` | see below | 60 each | dotted-quad string |

**`enable_tcpip()` is broken and must not be ported as-is.** `adb tcpip <port>` is a *host*-side adb
command; the current code runs `adb shell tcpip 5555`, which invokes a non-existent device binary.
Verified live on `2f019965`: `which tcpip` → rc=1, and `adb shell "tcpip 5555"` →
`/system/bin/sh: tcpip: inaccessible or not found`, exit 127, which `run(check=True)` turns into
`AdbError("adb shell tcpip 5555 failed: /system/bin/sh: tcpip: inaccessible or not found")`.
So `phone_enable_wifi_adb` / `scrcpymac_ui_enable_wifi_adb` currently always fail on this device.

Two options for Go, both acceptable, **pick one and flag it in the migration notes**:
- (a) faithful port — reproduce the failure bit-for-bit, keep the migration a pure no-op, and fix it
  in a separate follow-up commit;
- (b) fix it — emit `[]string{"tcpip", strconv.Itoa(port)}` (a host command; `-s <serial>` is still
  correct and required when several devices are attached). The JSON result shape
  (`{"ok":true,"action":"enable_tcpip","port":<int>,"output":<string>,"serial":<string>}`,
  actions.py:649) is unchanged; only `output` changes from an exception to
  `restarting in TCP mode port: 5555`.

**Recommendation: (b)**, with the fix called out in `CHANGELOG.md`, because the tool is
non-functional today and the JSON contract does not move. Note `port` is coerced with `int(port)` in
the device command but the *result* dict echoes the raw `port` argument.

`device_wifi_ip()` — two attempts, first dotted-quad match wins:
1. `shell "ip route | awk '/wlan/ {print $9; exit}'"`, accept if it matches `^\d+\.\d+\.\d+\.\d+$`
2. `shell "ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1"`, same test
3. else `AdbError("Could not detect device Wi-Fi IP. Is Wi-Fi connected?")`

The regex is `re.match` (anchored at start) with `$`, so a trailing newline would still match in
Python (`$` matches before a final newline) — but `shell()` has already stripped. Go: use
`^\d+\.\d+\.\d+\.\d+$` with `regexp.MatchString` on the stripped value. The regex does **not**
validate octet ranges; `999.999.999.999` passes. Preserve.

### 1.11 adb calls made from `scrcpy_runtime.py` (constraints on the Go `adb.Client` API)

Listed because they constrain the API surface, not because they belong to this spec:

- `AdbClient(serial=serial)` constructed per stream start (scrcpy_runtime.py:418)
- `adb.screen_size()` (line 426)
- `adb.run(["push", server_path, "/data/local/tmp/scrcpymac-plugin-server.jar"], timeout=60)` (431)
- `adb.run(["forward", "tcp:0", "localabstract:scrcpy_%08x"])`; the allocated port is parsed from
  `forward.stdout.strip()` with `int()`; a non-integer raises
  `ScrcpyRuntimeError(f"adb did not allocate a scrcpy forward port: {forward.stdout!r}")` (442–448)
- **`adb.adb_path` is read directly** (line 450) to build a long-lived
  `subprocess.Popen([adb_path, "-s", serial, "shell", "CLASSPATH=...", "app_process", ...])`.
  The Go `adb.Client` must therefore expose `Path() string` and `Serial() string`, and the runtime
  must be able to build its own `exec.Cmd` rather than going through `Run` (that process is
  streamed, not captured).
- teardown: `adb.run(["forward", "--remove", f"tcp:{port}"])` (521) and the same with `timeout=10`
  (907). Failures are swallowed (`except AdbError: pass`). **Keep swallowing** — teardown must never
  raise. Given another investigation may be starting/stopping streams on this device concurrently,
  a `forward --remove` for a port someone else already removed is expected and benign.

---

## 2. `doctor.py` — diagnostics

### 2.1 Current output shape

`run_doctor()` returns a dict. Two possible shapes.

**Shape A — early return when adb cannot be resolved** (doctor.py:48). Keys, in order:

```json
{
  "ok": false,
  "version": "0.7.2",
  "checks": [ ... ],
  "summary": "adb is required before controlling a phone"
}
```

**There is no `backend` key and no `uv_available` key in this shape.** Any Go implementation that
always emits the full struct changes the contract. Use two distinct result types, or
`omitempty`-style suppression driven by an explicit flag.

**Shape B — normal return** (doctor.py:101). Keys, in order:

```json
{
  "ok": <bool>,
  "version": "0.7.2",
  "backend": "plugin-h264-ready" | "adb",
  "checks": [ ... ],
  "summary": "ready" | "fix failed checks above",
  "uv_available": <bool>
}
```

- `backend` = `"plugin-h264-ready"` **iff `resolve_scrcpy_server_path()` succeeded**, else `"adb"`.
  This is the string `skills/scrcpymac-link/SKILL.md:24` promises the user
  ("`phone_doctor` should report `plugin-h264-ready`; the active Widget reports `plugin-h264`").
  It is *not* affected by whether a stream is actually running.
- `ok` = `all(c["ok"] for c in checks if c["name"] != "foreground_app")`. The `foreground_app`
  exclusion is a no-op today because that check is only ever appended with `ok=True`; keep the
  exclusion anyway so behaviour matches if the check ever fails.
- `summary` = `"ready"` if `ok` else `"fix failed checks above"`.
- `uv_available` = `shutil.which("uv") is not None`.
- `version` = `phone_agent.__version__`.

### 2.2 The `checks` array — every entry, in emission order

Each entry is an object with `name`, `ok`, `detail` **in that order**, then zero or more extras.

| # | `name` | `ok` | `detail` | extras | notes |
|---|---|---|---|---|---|
| 1 | `platform` | always `true` | `f"{platform.system()} {platform.machine()}"` → `Darwin arm64` | — | note the capitalised `Darwin`, from `uname -s`, not `sys.platform` |
| 2 | `python` | `sys.version_info >= (3,10)` | `f"{major}.{minor}"` → `3.13` | — | **vanishes in Go, see §2.4** |
| 3 | `mcp_package` | import succeeded | `"installed"` / `"pip install mcp"` | — | **vanishes in Go, see §2.4** |
| 4 | `scrcpy_server` | resolution succeeded | resolved absolute path, or `str(ScrcpyRuntimeError)` | `bundled: bool` **only on success** | `bundled = bool(root) and path.startswith(root)` |
| 5 | `adb` | resolution succeeded | resolved absolute path, or `str(AdbError)` | `bundled: bool` **only on success** | on failure → **Shape A early return** |
| 6 | `adb_version` | `adb version` succeeded | first line of stdout, stripped; `"unknown"` if stdout empty | — | live: `Android Debug Bridge version 1.0.41` |
| 7 | `device` | ≥1 ready device | see below | `devices: []Device` — **omitted entirely on `AdbError`** | |
| 8 | `screen_size` | | `f"{w}x{h}"` e.g. `1080x2280`, or `str(AdbError)` | — | **only emitted when exactly one ready device** |
| 9 | `foreground_app` | always `true` | `app["package"]` or `"unknown"` | `activity: str` (may be `""`) | same condition as #8; **not emitted if `screen_size` raised** |
| 10 | `runtime_architecture` | always `true` | `standalone plugin H.264 runtime; ScrcpyMac.app is not used` | `backend: "plugin-h264"` | constant |

Check #7 `device` detail vocabulary — three mutually exclusive strings plus the error path:

| condition | `ok` | `detail` | `devices` |
|---|---|---|---|
| ≥1 device with `state == "device"` | `true` | `f"{n} ready device(s)"` (e.g. `1 ready device(s)`) | full list |
| devices present, none ready | `false` | `device(s) found but not authorized — accept USB debugging prompt` | full list |
| empty list | `false` | `no devices — connect USB or adb connect` | `[]` |
| `AdbError` from `list_devices()` | `false` | `str(exc)` | **key absent** |

Both non-empty failure strings contain **U+2014 EM DASH**, emitted raw because `ensure_ascii=False`.

Extras key order within a check: `name`, `ok`, `detail`, then `bundled` / `devices` / `activity`.

### 2.3 Two real bugs in `doctor.py` to *not* port

1. **`bundled` for adb uses a NUL sentinel** (doctor.py:44):
   `adb_path.startswith(os.environ.get("PHONE_AGENT_ROOT", "\0"))`.
   The `"\0"` default makes the result `False` when `PHONE_AGENT_ROOT` is unset — clever but
   fragile: if `PHONE_AGENT_ROOT` is set to the **empty string**, `startswith("")` is `True` and
   every adb path is reported as bundled. The `scrcpy_server` check five lines earlier does it
   correctly (`bool(root) and ...`). Go: use one helper,
   `func isBundled(p, root string) bool { return root != "" && strings.HasPrefix(p, root) }`.
   Behaviour is identical except in the empty-string edge case. Flagged; no key change.
2. **Duplicate `screen_size` entry.** Lines 83–90 append `screen_size(ok=true)` and then
   `foreground_app` inside the same `try`. If `screen_size()` succeeds but `current_app()` raises
   `AdbError`, the `except` appends a *second* `screen_size` entry with `ok=false` — the array then
   contains two checks with the same `name` and the overall `ok` goes false. Go should wrap each of
   the two probes in its own error handling: `screen_size` gets exactly one entry (ok or not), and
   `foreground_app` gets an entry with `ok=false, detail=<error>` when `current_app()` fails.
   This is a strict improvement and cannot regress: no consumer indexes `checks` positionally
   (grep across `ui/`, `skills/`, `server/` finds no reader of `checks` at all — the array is
   presented to the model as text).

Also note the whole ready-device block is entered only when `len(ready_devices) == 1`; with two
authorised devices doctor reports neither screen size nor foreground app. Preserve.

### 2.4 Go doctor: which keys change

The Python-runtime checks describe a runtime that no longer exists. Per the task, changing those is
acceptable; every change is flagged here.

**Removed:**

| key | where | why |
|---|---|---|
| `checks[].name == "python"` | check #2 | there is no Python interpreter. Nothing to report. |
| `checks[].name == "mcp_package"` | check #3 | the MCP Go SDK is statically linked; "installed" is vacuous and "pip install mcp" is actively misleading guidance. |
| top-level `uv_available` | Shape B | `uv` is a Python package installer; irrelevant once `.venv` is gone. |

**Added:**

| key | value | rationale |
|---|---|---|
| `checks[].name == "binary"` (slot #2, replacing `python`) | `ok: true`, `detail: "phone-agent <version> <GOOS>/<GOARCH> (go<toolchain>)"` e.g. `phone-agent 0.7.2 darwin/arm64 (go1.25.7)` | the task's "Go binary arch" check. Answers "did the user get the arm64 or the x86_64 build, and is it running under Rosetta?" — compare `runtime.GOARCH` against the host arch (`sysctl hw.optional.arm64` / `unix.Sysctl("kern.version")`); if `GOARCH == "amd64"` on an arm64 host, set `ok: true` but append ` (running under Rosetta 2)` to `detail`, since it works but is slower. |
| `checks[].name == "plugin_root"` (slot #3, replacing `mcp_package`) | `ok` = the resolved root is an existing directory containing `mcp.json`; `detail` = the resolved absolute path; extra `derived: bool` (`true` when derived from `os.Executable()`, `false` when taken from the `PHONE_AGENT_ROOT` env) | without this, an adb/scrcpy-server "not found" is unattributable. |

**Kept, unchanged in name and value vocabulary:** `platform`, `scrcpy_server` (+`bundled`), `adb`
(+`bundled`), `adb_version`, `device` (+`devices`, all four detail strings verbatim including the
em-dashes), `screen_size`, `foreground_app` (+`activity`), `runtime_architecture` (+
`backend: "plugin-h264"`), and top-level `ok`, `version`, `backend`
(`"plugin-h264-ready"` / `"adb"`), `checks`, `summary` (`"ready"` /
`"fix failed checks above"` / `"adb is required before controlling a phone"`).

The "bundled adb present + executable" and "share/scrcpy-server present" checks the task asks for
are **already** exactly what the existing `adb` and `scrcpy_server` checks report (both resolution
paths require the file to exist, and the adb path additionally requires `X_OK`); the `bundled`
extra already distinguishes "shipped with the plugin" from "found on the system". No new key needed
— just make sure `$ROOT/share/scrcpy-server` is in the candidate list (§3.3). Likewise "device
reachable" is the existing `device` check.

`platform` detail must stay in `uname` vocabulary: `Darwin arm64`, **not** `darwin arm64` and not
`darwin/arm64`. Derive it from `runtime.GOOS` → `"Darwin"`/`"Linux"` (title-cased) and
`runtime.GOARCH` → `"arm64"`/`"x86_64"` (**note: `amd64` must be rendered `x86_64` to match
`platform.machine()` on macOS**).

### 2.5 Doctor's adb invocations

Doctor constructs `AdbClient` **three separate times** (lines 56, 63, 84), each re-resolving the adb
path. In Go, resolve once and reuse a single client; the observable output is identical. Doctor
never passes a serial except for the single-ready-device probe (line 84), and it deliberately does
**not** honour `PHONE_AGENT_SERIAL` for the `device` check (it lists everything).

Careful: `AdbClient(adb_path=adb_path)` at line 63 still picks up `PHONE_AGENT_SERIAL` for
`self.serial`, so `list_devices()` runs as `adb -s <serial> devices -l` when that env var is set.
Harmless, but reproduce it or the argv logs differ.

`phone_doctor` (server.py:70) is the only tool with **no** try/except — it returns
`json_result(run_doctor())` directly, so an unexpected exception propagates to FastMCP. `run_doctor`
itself never raises today (every adb call is guarded). Go: the doctor function must return a result,
not an error; any internal failure becomes a failing check.

---

## 3. The launcher surface

### 3.1 `bin/phone-agent` today (bash, 105 lines)

```
PLUGIN_ROOT = $(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export PHONE_AGENT_ROOT="$PLUGIN_ROOT"
SERVER_DIR = $PLUGIN_ROOT/server
PYTHON_BIN = ${PHONE_AGENT_PYTHON:-python3}
select_bundled_adb                        # unconditionally, line 77
export PYTHONPATH="$SERVER_DIR${PYTHONPATH:+:$PYTHONPATH}"
```

`set -euo pipefail` is in force.

`PLUGIN_ROOT` is computed with bash's **logical** `cd` (no `-P`), so when the plugin is reached
through the symlink `configure.sh` creates at `~/.cursor/plugins/local/scrcpymac-phone-agent`,
`PHONE_AGENT_ROOT` is the **symlink path**, not the real path. Every `bundled` prefix test in
`doctor.py` and every bundled-file candidate is built from that same string, so the prefix
comparisons stay consistent. **Go must not blindly `filepath.EvalSymlinks`** or a symlinked install
will report `bundled: false` for files that are in fact bundled. See §3.2.

`select_bundled_adb()` (lines 10–39):
- `[[ -z "${ADB_PATH:-}" ]] || return 0` — if `ADB_PATH` is already non-empty, do nothing. Note it
  does **not** verify that the user's `ADB_PATH` exists.
- `uname -s`: `Darwin` → `platform=darwin`; `Linux` → `platform=linux`; anything else → empty.
- `uname -m`: darwin `arm64|aarch64` → `arm64`, `x86_64|amd64` → `x86_64`; linux only
  `x86_64|amd64` → `x86_64`. Any other value leaves `arch` empty.
- If both set: `candidate=$ROOT/bin/$platform/$arch/adb`; `[[ -x $candidate ]] && export ADB_PATH`.
  **`$ROOT/bin/darwin/adb` is never considered here.**

Subcommand dispatch (lines 81–105):

| `$1` | actions | process |
|---|---|---|
| `mcp` | `ensure_plugin_runtime`; `ensure_adb_for_mcp`; `exec "$PYTHON_BIN" -m phone_agent.server` | replaced by exec |
| `doctor` | `ensure_plugin_runtime`; `exec "$PYTHON_BIN" -c "from phone_agent.doctor import run_doctor; import json; print(json.dumps(run_doctor(), indent=2, ensure_ascii=False))"` | replaced by exec |
| `devices` | `ensure_plugin_runtime`; `exec "$PYTHON_BIN" -c "from phone_agent.actions import PhoneActions; import json; print(json.dumps(PhoneActions().devices(), indent=2, ensure_ascii=False))"` | replaced by exec |
| `version` | `ensure_plugin_runtime`; `"$PYTHON_BIN" -c "from phone_agent import __version__; print(f'scrcpymac-phone-agent {__version__}')"` — **no `exec`**, so the script exits with the child's status after it returns | child |
| anything else, **including no argument** (`cmd="${1:-}"`) | prints two lines to **stderr**: `ScrcpyMac Phone Agent` then `Usage: phone-agent {mcp\|doctor\|devices\|version}`; `exit 1` | — |

Exact stdout contracts:
- `doctor` → the doctor JSON, 2-space indent, non-ASCII literal, **plus a trailing `\n`** from
  `print`.
- `devices` → a **bare JSON array** `[ {...}, ... ]`, 2-space indent, trailing `\n`. Empty device
  list → `[]`. **Not** wrapped in `{"devices": ...}` — that wrapper only exists on the
  `phone_list_devices` MCP tool.
  Note `PhoneActions()` is constructed with no client and `devices()` calls
  `self.client.list_devices()` **without** `ensure_device()`, so it never errors on "no device"; it
  errors only if adb cannot be resolved (uncaught `AdbError` traceback → non-zero exit, stderr).
- `version` → exactly `scrcpymac-phone-agent 0.7.2\n`.
- `mcp` → stdio JSON-RPC; **nothing else may ever be written to stdout.**
  `server/tests/test_runtime_bootstrap.py:188` asserts `result.stdout == ""` for a first-launch
  `bin/phone-agent mcp` and that all bootstrap chatter lands on stderr. Any Go log line on stdout
  corrupts the MCP framing. Route every diagnostic to stderr.

`ensure_adb_for_mcp()` (lines 41–68) — **only invoked for the `mcp` subcommand**:
1. `select_bundled_adb`
2. if `ADB_PATH` non-empty **or** `command -v adb` succeeds → return
3. if `PHONE_AGENT_AUTO_DOWNLOAD_ADB == "0"` (exact string compare against `0`; default `1`) →
   return
4. platform: `Darwin` → `darwin`; `Linux` **and** `uname -m` in `x86_64|amd64` → `linux`; else empty
5. if platform non-empty: `echo "==> adb not found; downloading bundled platform-tools" >&2`, then
   `"$ROOT/scripts/download-adb.sh" "$platform" >&2`; on failure
   `echo "WARN: bundled adb download failed; phone tools will report setup guidance" >&2`
   (**never fatal** — the server still starts and the tools return setup guidance)
6. `select_bundled_adb` again — this succeeds because `download-adb.sh darwin` mirrors the universal
   binary into `bin/darwin/arm64/adb` and `bin/darwin/x86_64/adb`.

`ensure_plugin_runtime()` (lines 70–75): if `PHONE_AGENT_PYTHON` is unset/empty, run
`scripts/ensure-runtime.sh` with stdout redirected to stderr, then set
`PYTHON_BIN=$ROOT/.venv/bin/python`. **This entire function, and `ensure-runtime.sh`, disappear in
the Go port** — that is the point of the migration.

### 3.2 `bin/phone-agent` after the migration

Constraint: `mcp-server.sh` (`exec "$ROOT/bin/phone-agent" mcp`) and `scripts/doctor.sh`
(`exec "$ROOT/bin/phone-agent" doctor`) must keep working unchanged, and `mcp.json`/`.mcp.json`
point at `./mcp-server.sh` with `cwd: "."`.
`server/tests/test_packaging.py:61` asserts that mapping.

**Recommended cutover shape** (a later, deliberate step — not part of writing the Go code):
`bin/phone-agent` becomes a ~15-line arch-dispatch shim that keeps its current filename and
`exec`s the Go binary:

```bash
#!/usr/bin/env bash
set -euo pipefail
PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"   # logical cd, keep -L
export PHONE_AGENT_ROOT="$PLUGIN_ROOT"
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64|Darwin/aarch64) BIN="$PLUGIN_ROOT/bin/darwin/arm64/phone-agent" ;;
  Darwin/x86_64|Darwin/amd64)  BIN="$PLUGIN_ROOT/bin/darwin/x86_64/phone-agent" ;;
  Linux/x86_64|Linux/amd64)    BIN="$PLUGIN_ROOT/bin/linux/x86_64/phone-agent" ;;
  *) echo "Unsupported platform: $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac
[[ -x "$BIN" ]] || { echo "phone-agent binary missing: $BIN" >&2; exit 1; }
exec "$BIN" "$@"
```

Keeping the shim (rather than making `bin/phone-agent` itself the arm64 binary) preserves the
symlink-derived `PHONE_AGENT_ROOT` semantics and keeps a single fat-free artefact per arch.

**The Go binary must nonetheless work when invoked directly**, so it derives `PHONE_AGENT_ROOT`
itself:

```
if v := os.Getenv("PHONE_AGENT_ROOT"); v != "" { root = v }        // shim / user wins, verbatim
else {
    exe, _ := os.Executable()                                       // do NOT EvalSymlinks
    root = filepath.Clean(filepath.Join(filepath.Dir(exe), "..", "..", ".."))
}
```

Three levels up because the binary lives at `<root>/bin/<os>/<arch>/phone-agent`. Sanity-check that
`<root>/mcp.json` exists; if not, fall back to two levels up (`<root>/bin/<os>/phone-agent`) and
then to the executable's own directory's parent, and surface the outcome through the new
`plugin_root` doctor check rather than aborting.

Whether or not it was derived, the Go binary should `os.Setenv("PHONE_AGENT_ROOT", root)` so that
child processes (`scripts/download-adb.sh`, the long-lived `adb shell app_process` for scrcpy) see
the same value the bash launcher exported.

Requirements the Go `main` must satisfy:

- Same four subcommands, same argument (`os.Args[1]`), same "no argument" fallthrough.
- Usage text byte-identical, on **stderr**, exit code **1**.
- `doctor`: marshal `run_doctor()` with `SetEscapeHTML(false)` + 2-space indent, write to stdout,
  then one `\n`. Exit 0 even when `ok:false` (the bash version exits with `print`'s status, i.e. 0).
- `devices`: bare array, same encoder settings, trailing `\n`. On adb-resolution failure, write the
  error to stderr and exit non-zero (matching the current uncaught-traceback behaviour, minus the
  traceback).
- `version`: `fmt.Printf("scrcpymac-phone-agent %s\n", Version)`.
- `mcp`: run `ensureADBForMCP()` (below), then serve MCP over stdio. **Nothing on stdout but
  JSON-RPC.**
- Replicate `select_bundled_adb` *effects* by simply doing resolution in-process (§1.3); there is no
  separate export step. Still `os.Setenv("ADB_PATH", resolved)` after resolution so the
  `adb shell app_process` child and `download-adb.sh` agree — and so the env var round-trips the way
  `test_runtime_bootstrap.py:230` asserts today.
- `ensureADBForMCP()`: if adb already resolves, return. Else if
  `os.Getenv("PHONE_AGENT_AUTO_DOWNLOAD_ADB") == "0"`, return. Else, on
  `darwin` (any arch) or `linux/amd64`, log `==> adb not found; downloading bundled platform-tools`
  to stderr and download; on failure log
  `WARN: bundled adb download failed; phone tools will report setup guidance` to stderr and
  **continue**. Never fatal.
  Implement the download natively with `net/http` + `archive/zip` (both stdlib) rather than shelling
  out to `scripts/download-adb.sh`, so the shipped plugin has no bash/curl/unzip dependency —
  §3.5.

Cross-compilation: `GOOS=darwin GOARCH=arm64` and `GOOS=darwin GOARCH=amd64`, `CGO_ENABLED=0`.
No cgo means `os/user` and DNS use the pure-Go paths, which is fine here. For macOS distribution the
two binaries must be codesigned (ad-hoc at minimum) or Gatekeeper will refuse them on first run;
that is a packaging concern but it belongs on the cutover checklist.

### 3.3 `resolve_scrcpy_server_path()` (scrcpy_runtime.py:67)

Current candidate order, first `is_file()` wins (**executability is not checked** — it is a `.jar`,
`adb push`ed to the device):

1. `$SCRCPY_SERVER_PATH`, `expanduser()`-ed. Empty string is falsy → skipped. **Only this candidate
   gets tilde expansion.**
2. if `$PHONE_AGENT_ROOT` non-empty:
   1. `$ROOT/bin/scrcpy-server`
   2. `$ROOT/bin/darwin/share/scrcpy-server` ← **where the file actually lives today**
      (`bin/darwin/share/scrcpy-server`, 92 KB; asserted `> 80_000` bytes by
      `test_packaging.py:85`)
3. `/opt/homebrew/share/scrcpy/scrcpy-server`
4. `/usr/local/share/scrcpy/scrcpy-server`

Failure: `ScrcpyRuntimeError("scrcpy-server is missing from the plugin. Reinstall the plugin or set SCRCPY_SERVER_PATH.")`
(one space between the sentences; the source splits across two lines).

**Go must add the new `share/` location from the target layout.** Recommended order:

```
1. $SCRCPY_SERVER_PATH (leading "~/" expanded to $HOME)
2. $ROOT/share/scrcpy-server                 // NEW — target layout
3. $ROOT/bin/scrcpy-server                   // legacy
4. $ROOT/bin/darwin/share/scrcpy-server      // legacy, current on-disk location
5. /opt/homebrew/share/scrcpy/scrcpy-server
6. /usr/local/share/scrcpy/scrcpy-server
```

Keeping 3 and 4 means a Go binary dropped into today's tree still finds the jar, so the migration
can be validated before the files move. The doctor `scrcpy_server.bundled` extra stays
`root != "" && strings.HasPrefix(path, root)`, which is `true` for candidates 2–4.

`SCRCPY_VERSION = "3.3.4"` is a compile-time constant passed as the first `app_process` argument and
**must match the bundled jar exactly** or scrcpy-server aborts with a version-mismatch error.
`THIRD_PARTY_NOTICES.md` pins SHA-256
`8588238c9a5a00aa542906b6ec7e6d5541d9ffb9b5d0f6e1bc0e365e2303079e` — the Go build/packaging step
should verify it, and `licenses/scrcpy-APACHE-2.0.txt` must ship alongside.

**Do not `//go:embed` the jar.** `adb push` needs a real filesystem path; embedding would force a
temp-file extraction on every stream start. Keep it adjacent, as the target layout specifies. Same
reasoning for `adb` itself.

### 3.4 `scripts/ensure-runtime.sh` — deleted

172 lines. Summarised only so nothing is lost:

- Resolves a base Python: `$PHONE_AGENT_BOOTSTRAP_PYTHON` → `$PHONE_AGENT_PYTHON` → a 14-entry
  candidate list (`python3`, `python3.13`…`python3.10`, `~/miniconda3/bin/python{3,}`,
  `/opt/homebrew/bin/python3.1{3,2,1,0}`, `/usr/local/bin/python3.1{3,2,1,0}`), each validated by
  importing `bz2`, `ssl`, `struct`, `sys`, `venv` and asserting `>= (3,10)`.
- Freshness marker `$ROOT/.venv/.phone-agent-runtime` = `"1:<sha256 of server/pyproject.toml>"`;
  a changed pyproject forces a full rebuild.
- `mkdir`-based lock at `$ROOT/.runtime-bootstrap.lock` with a pid file, stale-lock reaping via
  `kill -0`, 20-attempt grace for a pid-less lock and a 240-attempt (≈60 s) hard timeout.
- Install via `uv venv` + `uv pip install -e server` when `uv` is on `PATH`, else
  `python -m venv` + `pip install -e server`.
- Verification: `python -c 'import mcp; import phone_agent'`.
- All logging on stderr; `==> Phone Agent runtime ready` is asserted by
  `test_runtime_bootstrap.py:192`.

**All of this is deleted.** The Go binary has no bootstrap step — that is the entire justification
for the migration. Consequences to flag on the cutover checklist:

- `server/tests/test_runtime_bootstrap.py` (235 lines, 6 tests) becomes obsolete. Its only
  still-relevant assertion is `test_linux_launcher_selects_linux_bundled_adb` (line 196), which
  verifies `ADB_PATH=$ROOT/bin/linux/x86_64/adb` reaches the child — port that one to a Go test over
  the resolution function.
- `test_packaging.py:72` asserts `scripts/ensure-runtime.sh` exists; that assertion must be removed
  or retargeted at the Go binaries when the script is deleted.
- `.gitignore` entries `.venv/` and `.runtime-bootstrap.lock/` become dead; the Go binaries
  (`bin/darwin/*/phone-agent`) must be **committed**, not ignored — note the existing
  `bin/darwin/*/adb` ignore pattern is broad enough that a `bin/darwin/arm64/phone-agent` is *not*
  matched, but double-check when the layout lands.

### 3.5 `scripts/download-adb.sh`

Current behaviour (52 lines):

- `PLATFORM = ${1:-$(uname -s | tr '[:upper:]' '[:lower:]')}` → `darwin` / `linux`.
- `darwin|macos|mac` → `https://dl.google.com/android/repository/platform-tools-latest-darwin.zip`,
  `DEST=$ROOT/bin/darwin/adb`.
- `linux` → `…-latest-linux.zip`, `DEST=$ROOT/bin/linux/x86_64/adb`.
- anything else → `Unsupported platform: <p> (use darwin or linux)` on stderr, exit 1.
- If `DEST` is already executable: `adb already present at $DEST`, print `$DEST version | head -1`,
  exit 0. **Idempotent, no version check, never re-downloads.**
- Otherwise `mktemp -d` (+ `trap rm -rf` on EXIT), `curl -fsSL`, `unzip -q`,
  `cp platform-tools/adb "$DEST"`, `chmod +x`.
- **macOS only:** mirrors `DEST` into `$ROOT/bin/darwin/arm64/adb` and `$ROOT/bin/darwin/x86_64/adb`
  and chmods both. This is why the launcher's arch-only lookup works.
- Prints `==> Installed: $DEST` and `$DEST version | head -1`.
- **No checksum verification, no signature verification, no pinned version** — it always fetches
  "latest". `curl` does not set `com.apple.quarantine`, so the copied binary runs without a
  Gatekeeper prompt.

Go replacement (`internal/adbdl`), invoked only from `ensureADBForMCP`:

- stdlib only: `net/http` (with a `context` timeout, say 120 s), `archive/zip`, `io`, `os`.
- Same URLs, same destinations, same "already present → no-op" short-circuit, same stderr messages.
- Write to `DEST + ".tmp"` then `os.Rename` (atomic), `chmod 0755`. Mirror to the two arch paths on
  darwin exactly as the script does, so a mixed old/new tree stays consistent.
- `archive/zip` requires a `ReaderAt`, so buffer the ~15 MB zip to a temp file rather than memory,
  and extract only the `platform-tools/adb` entry. **Reject entries whose cleaned path escapes the
  destination** (zip-slip) even though only one entry is extracted.
- **Recommendation (new):** record and verify a SHA-256 for the extracted `adb`, or at minimum log
  the resulting `adb version`. Fetching an unverified executable over HTTPS from
  `dl.google.com` is the current behaviour, but for a Marketplace artefact the far better answer is
  that **`bin/darwin/adb` ships pre-bundled** and this path is a dev-only fallback that essentially
  never fires. Keep `PHONE_AGENT_AUTO_DOWNLOAD_ADB` and its default of enabled for parity.
- Keep `scripts/download-adb.sh` on disk for developers/CI (`install.sh` calls it); the Go path is
  the runtime one.

### 3.6 `scripts/doctor.sh`

Five lines; `exec "$PLUGIN_ROOT/bin/phone-agent" doctor`. **No change required** — it keeps working
through the shim. Its comment ("bin/phone-agent already sets PHONE_AGENT_ROOT/PYTHONPATH/ADB_PATH
itself") should drop the `PYTHONPATH` mention at cutover.

### 3.7 `scripts/install.sh`

Current: requires `python3` on `PATH` and asserts `>= 3.10` (**hard `exit 1` otherwise**), chmods
`bin/phone-agent`, `mcp-server.sh` and `scripts/*.sh`, runs `ensure-runtime.sh`, runs
`download-adb.sh darwin|linux` (`|| true`), runs `configure.sh`, then `bin/phone-agent doctor`
(`|| true`).

After migration:
- **Delete the two Python gates** (lines 9–17) — a Python check that rejects the install of a plugin
  that no longer uses Python is the single worst regression available here.
- **Delete the `ensure-runtime.sh` call** (line 23).
- Add `chmod +x "$PLUGIN_ROOT/bin/darwin/arm64/phone-agent" "$PLUGIN_ROOT/bin/darwin/x86_64/phone-agent"`
  (git preserves the exec bit, but zip-based Marketplace delivery may not).
- Keep the `download-adb.sh` call as a no-op safety net (`bin/darwin/adb` should already be present).
- Keep `configure.sh` and the final `doctor` run.
- Optionally verify `share/scrcpy-server`'s SHA-256 against `THIRD_PARTY_NOTICES.md`.

### 3.8 `scripts/configure.sh`

Current: chmods `mcp-server.sh` and `bin/phone-agent`; prints an `mcpServers` JSON snippet with the
absolute launcher path for Cursor (`~/.cursor/mcp.json`) and Claude Desktop
(`~/Library/Application Support/Claude/claude_desktop_config.json`); symlinks
`~/.cursor/plugins/local/scrcpymac-phone-agent → $PLUGIN_ROOT` unless a non-symlink already occupies
that path (`WARN: … exists and is not a symlink — skipping`); greps `~/.cursor/mcp.json` for
`scrcpymac-phone-agent` and prints guidance; prints
`codex plugin marketplace add $(cd "$PLUGIN_ROOT/../.." && pwd)`.

After migration: **only the chmod list changes** — add the two Go binaries. Everything else is
runtime-agnostic. Note that the symlink it creates is precisely the case that makes
`PHONE_AGENT_ROOT` a symlink path (§3.1).

### 3.9 `scripts/build-ui.sh`

Current: `cd ui`, `npm ci`, `npm run check`, `npm run build`, then
`cp ui/dist/index.html → server/phone_agent/static/scrcpymac-app.html`.
A `trap cleanup EXIT` deletes `ui/node_modules` **and** `ui/dist` unconditionally (via
`find … -depth -delete`), including on success — so every build is a cold `npm ci` and the built
`dist` is not left behind.

`test_packaging.py:74` asserts `server/phone_agent/static/scrcpymac-app.html` exists, is
**> 100 000 bytes**, that `build-ui.sh` is executable, and that `ui/package-lock.json` exists.

Migration: the widget HTML is `//go:embed`-ed. It must therefore live inside the Go module tree —
`go:embed` cannot reach outside the package directory, so
`go/internal/mcpui/static/scrcpymac-app.html` (or similar) is required; a symlink into `server/` will
**not** work (`go:embed` refuses to follow symlinks).

Because this spec's rules forbid touching `server/`, during the migration `build-ui.sh` should copy
the built HTML to **both** destinations:

```bash
mkdir -p "$STATIC_ROOT" "$GO_STATIC_ROOT"
cp "$UI_ROOT/dist/index.html" "$STATIC_ROOT/scrcpymac-app.html"
cp "$UI_ROOT/dist/index.html" "$GO_STATIC_ROOT/scrcpymac-app.html"
```

and the `server/` copy is dropped at cutover along with the Python tree. The widget itself is not
being rewritten. (Separately: `ui/src/main.ts:573`'s arbitrary delta-frame drop is a known bug owned
by the streaming spec, not this one.)

---

## 4. Every environment variable the plugin reads

Complete list, from an exhaustive grep of `os.environ` / `getenv` / `process.env` / `${VAR}` across
`server/`, `bin/`, `scripts/`, `ui/`, `skills/` (excluding `node_modules`, `.venv`, `.git`, `dist`).

| Variable | Read by | Default | Effect | After Go migration |
|---|---|---|---|---|
| `ADB_PATH` | `adb.py:72` (1st choice); `bin/phone-agent:15,36,45` (suppressor + export target) | unset | Absolute path to adb. Honoured by the library **only if** it is an existing executable file — otherwise silently ignored and resolution continues. The launcher exports it to the bundled arch path when unset. | **unchanged**; Go re-exports the resolved value |
| `ANDROID_ADB` | `adb.py:72` (fallback when `ADB_PATH` is unset **or empty**) | unset | Same semantics as `ADB_PATH`. **The bash launcher never reads it**, so it only matters when the Go/Python code resolves on its own. | **unchanged** |
| `PHONE_AGENT_ROOT` | exported by `bin/phone-agent:6`; read by `adb.py:41`, `doctor.py:35,44`, `scrcpy_runtime.py:73` | unset outside the launcher | Plugin root. Gates *all* bundled-file lookup (adb and scrcpy-server) and both `bundled:` doctor flags. Unset ⇒ bundled adb and bundled scrcpy-server are invisible. May legitimately be a symlink path. | **unchanged**, but now *derived* from `os.Executable()` when unset instead of being mandatory |
| `PHONE_AGENT_SERIAL` | `adb.py:103` (default `AdbClient.serial`), `actions.py:60` (`selected_serial` fallback); referenced in `adb.py:171` error text and `skills/phone-setup/SKILL.md` | unset | Pins `adb -s <serial>` on every call and makes `ensure_device()` skip device enumeration entirely. Empty string = unset. | **unchanged** |
| `SCRCPY_SERVER_PATH` | `scrcpy_runtime.py:68` | unset | Overrides the bundled scrcpy-server jar; first candidate, tilde-expanded. Empty string = unset. Must match `SCRCPY_VERSION = "3.3.4"`. | **unchanged** |
| `PHONE_AGENT_AUTO_DOWNLOAD_ADB` | `bin/phone-agent:48` (only on the `mcp` path) | `1` | Exactly `"0"` disables the first-launch adb download. Any other value (including unset, `false`, `no`) enables it. | **unchanged** — keep the exact-`"0"` comparison |
| `PHONE_AGENT_PYTHON` | `bin/phone-agent:8,71`; `ensure-runtime.sh:31` (2nd choice) | `python3` | Uses a caller-managed interpreter and **skips the `.venv` bootstrap entirely**. | **becomes a no-op**; flag in `CHANGELOG.md` and drop from `README.md` |
| `PHONE_AGENT_BOOTSTRAP_PYTHON` | `ensure-runtime.sh:31` (1st choice) | unset | Base interpreter used to *create* the `.venv`. | **becomes a no-op**; drop from `README.md` |
| `PYTHONPATH` | exported by `bin/phone-agent:79` as `$ROOT/server:$PYTHONPATH` | inherited | Makes `phone_agent` importable. Prepends, preserving any existing value. | **disappears**; the Go shim must stop exporting it |
| `PATH` | `shutil.which("adb")` (`adb.py:80`), `shutil.which("uv")` (`doctor.py:107`), `command -v` in every script, `resolve_python` in `ensure-runtime.sh` | inherited | adb discovery step 3, `uv_available` doctor key. **Note MCP clients often launch the server with a minimal `PATH`**, which is exactly why the bundled-adb path exists. | `PATH` still used for `exec.LookPath("adb")`; the `uv` lookup disappears with `uv_available` |
| `HOME` | `os.path.expanduser` in `adb.py:85` and `scrcpy_runtime.py:71`; `$HOME` in `configure.sh:33,43` and `ensure-runtime.sh:55` | inherited | Tilde expansion for the Android SDK adb candidate, `SCRCPY_SERVER_PATH`, the Cursor plugin symlink and mcp.json probe. | **unchanged**; in Go use `os.UserHomeDir()` (which reads `$HOME` first) |
| `ANDROID_SERIAL` | **not read by this code**, but honoured by `adb` itself in every child process (env is inherited unmodified) | unset | Silently selects a device when `self.serial` is empty, bypassing `ensure_device()`'s multi-device guard. | **unchanged** (inherit the environment; do not scrub) |
| `FAKE_UV_LOG`, `FAKE_VENV_PYTHON_LOG`, `FAKE_UV_SLEEP`, `MANAGED_PYTHON_LOG` | `server/tests/test_runtime_bootstrap.py` fixtures only | — | Test scaffolding, not product surface. | deleted with the test |

`README.md:147-156` documents seven of these; after cutover that table must drop
`PHONE_AGENT_PYTHON` and `PHONE_AGENT_BOOTSTRAP_PYTHON` and gain `ANDROID_SERIAL` (as a caveat).

---

## 5. Migration checklist for this slice

Behaviour-identical, no discussion needed:

- [ ] `Device` JSON: 4 keys, that order, `""` never `null`
- [ ] All seven `AdbError` message templates, verbatim (mind full-argv vs args-only)
- [ ] adb resolution order incl. the silent-ignore of a bad `ADB_PATH`
- [ ] `run()` CRLF normalisation on text, **none** on bytes
- [ ] 60 s default timeout, 30 s for screencap/uiautomator, 10 s for teardown `forward --remove`
- [ ] child killed and reaped on timeout
- [ ] `shell()` passes one argv element; device-side shell does the parsing
- [ ] `screen_size()` takes the **first** `\d+x\d+` match
- [ ] `ensure_device()` short-circuits on a preset serial without any adb call
- [ ] doctor Shape A (adb missing) has **no** `backend` and **no** `uv_available`
- [ ] doctor check key order `name, ok, detail, <extras>`; `devices` omitted on `AdbError`
- [ ] `backend: "plugin-h264-ready"` iff scrcpy-server resolves; `runtime_architecture.backend`
      is the constant `"plugin-h264"`
- [ ] em-dashes emitted raw; `SetEscapeHTML(false)`; 2-space indent
- [ ] four subcommands; usage on stderr, exit 1; `devices` prints a bare array; `version` prints
      `scrcpymac-phone-agent <v>`; `mcp` writes **nothing** to stdout but JSON-RPC
- [ ] `PHONE_AGENT_AUTO_DOWNLOAD_ADB` compared to the exact string `"0"`

Deliberate deviations — each must be listed in `CHANGELOG.md`:

- [ ] doctor: `python` and `mcp_package` checks removed; `binary` and `plugin_root` added
- [ ] doctor: top-level `uv_available` removed
- [ ] doctor: `bundled` for adb no longer uses the `"\0"` sentinel (empty-`PHONE_AGENT_ROOT` edge case)
- [ ] doctor: no duplicate `screen_size` entry when `current_app()` fails; `foreground_app` gets its
      own failing entry
- [ ] `adb devices -l`: header located rather than assumed at index 0; `*`-prefixed daemon lines
      skipped; `no permissions` state re-joined
- [ ] `enable_tcpip()` fixed to the host command `adb tcpip <port>` (recommended) — result keys
      unchanged
- [ ] `$ROOT/share/scrcpy-server` added as the preferred bundled candidate
- [ ] `PHONE_AGENT_PYTHON`, `PHONE_AGENT_BOOTSTRAP_PYTHON`, `PYTHONPATH` become no-ops
- [ ] adb download implemented in-process (stdlib `net/http` + `archive/zip`), atomic rename,
      zip-slip guard, optional SHA-256

Cutover-only (not part of writing the Go code):

- [ ] `bin/phone-agent` → arch-dispatch shim; `mcp-server.sh`, `mcp.json`, `.mcp.json` untouched
- [ ] `install.sh` loses its Python gates and the `ensure-runtime.sh` call; gains chmod of the Go
      binaries
- [ ] `configure.sh` chmod list gains the Go binaries
- [ ] `build-ui.sh` copies the widget into the Go tree (both destinations during migration)
- [ ] `ensure-runtime.sh` deleted; `test_runtime_bootstrap.py` retired, its Linux-bundled-adb
      assertion ported to Go
- [ ] `test_packaging.py` version-parity set gains the Go version constant; the
      `scripts/ensure-runtime.sh` existence assertion removed
- [ ] `README.md` env table updated; `bin/darwin/README.md` updated for the new layout
- [ ] `THIRD_PARTY_NOTICES.md` / `licenses/` re-verified (scrcpy Apache-2.0; platform-tools adb has
      its own terms if it is now *shipped* rather than downloaded — check before bundling)
