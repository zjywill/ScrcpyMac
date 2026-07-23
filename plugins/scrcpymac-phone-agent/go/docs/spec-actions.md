# spec-actions.md — behavioural spec for `phone_agent/actions.py`

Source of truth: `server/phone_agent/actions.py` (816 lines), read in full on 2026-07-22.
Supporting reads: `server/phone_agent/adb.py`, `server/phone_agent/server.py`,
`server/phone_agent/mcp_ui.py`, `server/phone_agent/scrcpy_runtime.py`,
`server/phone_agent/recipes/wechat.py`, `server/tests/*`.

This document is written so the Go port can be implemented **without opening the Python again**.
Everything below is observed behaviour, not intent. Where the Python does something wrong,
it is called out in a `BUG` block with a replicate/fix recommendation.

Conventions in this document:

* `→ {a, b, c}` means a JSON object whose keys appear **in that insertion order**
  (`json_result` preserves Python dict insertion order — see §13).
* "streaming" always means `runtime.status()["state"] == "streaming"`.
* All sleeps are wall-clock sleeps on the calling goroutine; there is no async anywhere.

---

## 1. Dependency surface

`PhoneActions` sits on top of exactly two collaborators. The Go port must expose the same
seams so the same branches exist.

### 1.1 `AdbClient` (from `adb.py`) — the methods `actions.py` actually calls

| Method | Exact adb invocation | Return / error |
|---|---|---|
| `list_devices()` | `adb [-s S] devices -l` | skips line 0 (the `List of devices attached` header); for each non-empty line, splits on whitespace; needs ≥2 fields; `serial=f[0]`, `state=f[1]`; scans remaining tokens for `model:`/`product:` prefixes (value = text after the first `:`); other tokens ignored |
| `ensure_device()` | if `serial` already set → return it. Else `list_devices()`, filter `state == "device"`. 0 → `AdbError("No Android device connected. Plug in USB or run adb connect.")`. >1 → `AdbError("Multiple devices connected (<s1>, <s2>). Set PHONE_AGENT_SERIAL or pass serial.")`. Exactly 1 → assigns `self.serial` and returns it | |
| `shell(cmd, timeout=60)` | `adb [-s S] shell <cmd>` — **`cmd` is passed as ONE argv element**, the device-side `sh` does the word splitting | returns `stdout.strip()` |
| `screen_size()` | `shell("wm size")`, then first regex match of `(\d+)x(\d+)` | `(w, h)` ints; no match → `AdbError("Could not parse screen size from: <repr(output)>")` |
| `current_app()` | `shell("dumpsys window \| grep -E 'mCurrentFocus\|mFocusedApp' \| head -1")` | first match of `([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)` → `{package, activity, raw}`; no match → `{"package":"", "activity":"", "raw": output}` |
| `screenshot_png()` | `adb [-s S] exec-out screencap -p`, **timeout 30 s**, raw bytes (no text decoding) | `[]byte` |
| `ui_tree_xml()` | `shell("uiautomator dump /sdcard/window_dump.xml >/dev/null 2>&1 && cat /sdcard/window_dump.xml; rm -f /sdcard/window_dump.xml", timeout=30)` | stripped stdout |
| `enable_tcpip(port)` | `shell("tcpip <int(port)>")` — see **BUG-1** | stripped stdout |
| `device_wifi_ip()` | see §12.2 | dotted-quad string or `AdbError` |
| `connect_wifi(host, port)` | `adb [-s S] connect <target>` where `target = host` if host contains `:` else `host:port` | stripped stdout |
| `disconnect_wifi(host)` | `adb [-s S] disconnect` (+ `<host>` if host non-empty, appending `:5555` when host has no `:`) | stripped stdout |

`run()` semantics that leak into `actions.py`:

* default subprocess timeout **60 s**; timeout → `AdbError("adb timed out: <full cmd joined by spaces>")`.
* non-zero exit with `check=True` → `AdbError("adb <args joined by spaces> failed: <detail>")`
  where `detail = stderr.strip()` else `stdout.strip()` else `"exit <rc>"`.
  Note the message contains the args **without** the adb path and **without** `-s <serial>`.
* stdout/stderr decoded as UTF-8 with `errors="replace"` when text mode.

`AdbError` is `RuntimeError`. `actions.py` raises `AdbError` for every user-facing failure;
`server.py` catches `(AdbError, OSError)` and emits `{"ok": false, "error": str(exc)}`.
**In Go, `AdbError` must map to a single error type whose `Error()` string is what the
Python `str(exc)` produced**, because that string is the tool's `error` field.

### 1.2 `ScrcpyRuntime` — the methods `actions.py` actually calls

| Method | Used by | Notes |
|---|---|---|
| `backend_name()` | `backend()` | `"plugin-h264"` while streaming else `"adb"` |
| `status()` | `selected_serial`, `device_info`, `_tap_once`, `swipe`, `_screen_size` | always returns `{ok, state, backend, encoding, error, fps, frames}` plus, **only when stream metadata exists**, `{serial, deviceName, deviceWidth, deviceHeight, frameWidth, frameHeight, maxFps, resolutionPercent, codec}` plus `streamUrl` when streaming |
| `is_active()` | `key()`, `paste()` | `state == "streaming" && meta != nil` |
| `tap_relative(x, y)` | `_tap_once` | → `{ok, action:"tap", serial, point:[px,py], coordinateSpace:[w,h], backend:"plugin-control"}` (coords are **frame** pixels) |
| `swipe_relative(x1,y1,x2,y2, duration_ms)` | `swipe()` | → `{ok, action:"swipe", serial, from:[..], to:[..], durationMs, backend:"plugin-control"}` |
| `key(name)` | `key()` | → `{ok, action:"key", key, serial, backend:"plugin-control"}`; raises `ScrcpyRuntimeError` on unknown key |
| `paste(text)` | `paste()` | → `{ok, action:"paste", length, serial, backend:"plugin-control"}` |

**`ScrcpyRuntimeError` raised by `key()`/`paste()` is caught in `actions.py` and re-raised as
`AdbError(str(exc))`.** Note the runtime's `tap_relative`/`swipe_relative` errors are **not**
wrapped — they propagate as `ScrcpyRuntimeError`, which `server.py` does **not** catch
(`except (AdbError, OSError)`), so a `phone_tap` while the stream dies mid-call raises out of
the tool handler. See **BUG-8**.

---

## 2. `PhoneActions` construction and state

```
PhoneActions(client: AdbClient|nil = nil, runtime: ScrcpyRuntime|nil = nil)
  self._client        = client            # may stay nil until first use
  self.runtime        = runtime or NewScrcpyRuntime()
  self._ui_tree_cache = nil               # dict|nil
```

* `client` property (lazy): if `_client == nil` → `_client = NewAdbClient()` and cache it.
  `NewAdbClient()` resolves the adb path eagerly (may raise `AdbError("adb not found. …")`)
  and sets `serial = os.Getenv("PHONE_AGENT_SERIAL")` (empty string ⇒ unset).
* `_ready()` = `client.ensure_device(); return client`. Every device-touching method calls it.
* `_invalidate_ui_tree_cache()` sets `_ui_tree_cache = nil`.
* `backend()` → `runtime.backend_name()` (string). `server.py` wraps as `{"backend": …, "ok": true}`.
* The server constructs exactly one `PhoneActions` lazily and shares it across all tools
  (`_get_actions()` singleton, `runtime` injected). **There is no mutex** — the Python MCP
  server is effectively single-threaded per request. In Go the handlers may run concurrently;
  guard `_client.serial` and `_ui_tree_cache` with a mutex. This is a safe, invisible change.

### 2.1 `selected_serial() -> string`

Exact order, no adb calls, **never instantiates the client**:

1. `st := runtime.status()`; if `st["serial"]` is present and truthy → return it as a string.
2. else if `_client != nil` and `_client.serial` is truthy → return `_client.serial`.
3. else return `os.Getenv("PHONE_AGENT_SERIAL")` (defaults to `""`).

Step 2 deliberately reads the *raw* field, so calling `selected_serial()` on a fresh
`PhoneActions` with no device does **not** raise "adb not found". Preserve that: in Go,
`selectedSerial()` must not trigger adb resolution. `scrcpymac_ui_state` relies on this —
it calls `selected_serial()` in its *error* path too.

### 2.2 `select_device(serial) -> dict`

```
serial = strings.TrimSpace(serial)
if serial == "" -> AdbError("device serial must not be empty")
devices = self.devices()                          # adb devices -l
device  = first d in devices where d["serial"] == serial
if device == nil -> AdbError("Android device not found: <serial>")
if device["state"] != "device" -> AdbError("Android device is <state>: <serial>")
self.client.serial = serial                        # instantiates the client if needed
self._invalidate_ui_tree_cache()
→ {"ok": true, "serial": serial, "device": device}
```

`device` is the full device dict `{serial, state, model, product}` in that key order.

### 2.3 `devices() -> []dict`

`self.client.list_devices()` mapped through `to_dict()`. Note it uses `self.client`, **not**
`_ready()` — no `ensure_device()`, so it works with zero devices connected and returns `[]`.
Each entry: `{"serial", "state", "model", "product"}` (order fixed, `model`/`product` default `""`).

---

## 3. `screenshot()` / `preview_frame()`

Both are thin wrappers over `_adb_screenshot()`. **`preview_frame(max_width=540, quality=60)`
ignores both arguments entirely** and returns the same full-resolution PNG dict.

```
_adb_screenshot():
    client = self._ready()
    png    = client.screenshot_png()          # adb exec-out screencap -p, 30s timeout
    w, h   = client.screen_size()             # adb shell wm size  (a SECOND round trip)
    → {
        "serial":     client.serial,
        "width":      w,
        "height":     h,
        "format":     "png",
        "base64":     base64.StdEncoding(png),   # ASCII, standard alphabet, padded, no newlines
        "png_bytes":  png,                       # raw bytes, in-process only
        "size_bytes": len(png),
        "backend":    "adb",
      }
```

Key facts:

* **Go correction:** width/height come from the PNG header, so they always match
  the image coordinate space. Coordinate mapping uses WindowManager's current
  `cur=WxH` size, which reflects display overrides and rotation; older Android
  versions fall back to `Override size`, then `Physical size`.
* `png_bytes` is an in-process-only key. `server.py` uses it to build the MCP image content
  block and then does **not** include `base64` in the JSON. When `include_image=false`, the
  JSON does include `base64`. `json_result` is never called on a dict containing `png_bytes`.
  In Go, model this as a struct with a `PNG []byte` field marked `json:"-"` plus an explicit
  serialiser for the tool payload.
* `phone_screenshot`'s JSON payload is exactly `{serial, width, height, format, size_bytes[, base64], ok}`
  — `ok` is appended last by `_ok()`'s `setdefault`, since it is absent from the screenshot dict.
* **There is no scrcpy-stream path in `screenshot()`.** Even while the H.264 stream is running,
  screenshots go through `adb exec-out screencap`, and `backend` is `"adb"`. Replicate.
* Failures: `screenshot_png()` raising (`AdbError` on non-zero exit / 30 s timeout) propagates.
  `tap()` catches `(AdbError, OSError)` around it; nothing else does.

### 3.1 Handoff to `scrcpymac_ui_snapshot`

`mcp_ui.scrcpymac_ui_snapshot` clamps `max_width` to `[320, 1200]` and `quality` to `[45, 90]`,
calls `preview_frame(max_width=…, quality=…)`, then compresses in `mcp_ui._preview_frame`:

* it first checks `shot["image_bytes"]`; `_adb_screenshot()` never sets that key, nor
  `frame_width`/`frame_height`/`mime_type`, so **that branch is dead code today**
  (it exists for a future runtime-fed JPEG path).
* live path: decode `png_bytes` → RGB; if `srcW > max_width`, resize to
  `(max_width, max(1, round(srcH * max_width/srcW)))` with **BILINEAR**; encode JPEG at
  `quality`, `optimize=False`. Result keys:
  `{ok, serial, backend, deviceWidth, deviceHeight, frameWidth, frameHeight, mimeType:"image/jpeg", dataBase64, sizeBytes}`.
* `tests/test_mcp_ui.py::test_snapshot_returns_compressed_structured_frame_once` pins:
  a 1080x2400 source at `max_width=540` → `frameWidth=540`, `frameHeight=1200`,
  `deviceWidth=1080`, `deviceHeight=2400`, `mimeType == "image/jpeg"`,
  `len(base64decode(dataBase64)) > 1000`, and the base64 **must not** appear in the text content block.

That compression lives in the mcp-ui layer, not in actions — but the Go `PreviewFrame` must
keep returning the *uncompressed PNG dict* so the split stays where it is.

---

## 4. Tap family

### 4.1 `_clamp_device_point(x, y) -> (int, int)`

```
size := self._screen_size()
if size == nil { return int(x), int(y) }          // NO clamping when size is unknown
return clamp(int(x), 0, w-1), clamp(int(y), 0, h-1)
```

`int(x)` on an already-int input is identity; the MCP layer types x/y as `int`.

### 4.2 `_screen_size() -> (int,int)|nil`

```
st := runtime.status()
if st["state"] == "streaming":
    w := int(st["deviceWidth"] or 0); h := int(st["deviceHeight"] or 0)
    if w != 0 && h != 0 { return w, h }          // native device pixels, not frame pixels
try: return self._ready().screen_size()
except (AdbError, OSError): return nil
```

`_required_screen_size()` = same, but `nil` → `AdbError("Could not determine the device screen size")`.

> **Performance note (not a behaviour change):** `_screen_size()` is called on *every* tap and
> *every* screenshot, i.e. a verified tap issues `wm size` up to 1 + 2·(attempts+1) times plus a
> `screencap` per attempt. Go may memoize `wm size` for a short TTL (≤2 s) keyed on serial;
> this is invisible to the contract except across a rotation that happens inside one call.
> Flagged as an allowed, deliberate deviation.

### 4.3 `_tap_once(x, y) -> dict`

```
st := runtime.status()
if st["state"] == "streaming":
    w := max(1, int(st["deviceWidth"])); h := max(1, int(st["deviceHeight"]))
    result = runtime.tap_relative(float(int(x))/float(max(w-1,1)),
                                  float(int(y))/float(max(h-1,1)))
else:
    client = self._ready()
    client.shell(fmt.Sprintf("input tap %d %d", int(x), int(y)))
    result = {"ok": true, "action": "tap", "x": x, "y": y, "serial": client.serial}
self._invalidate_ui_tree_cache()
return result
```

Note the two result shapes differ by design (streaming returns the runtime's shape with
`point`/`coordinateSpace`/`backend`, adb returns `x`/`y`). Replicate both verbatim.

`input tap` args are plain `%d` — no quoting needed, and no injection risk.

### 4.4 `tap(x, y, *, verify=False, retries=2, retry_radius_px=32, settle_s=0.45) -> dict`

MCP defaults differ: `phone_tap` exposes `verify=True, retries=2` and does **not** expose
`retry_radius_px`/`settle_s`. Internal callers (`find_and_tap`) use `verify=False` by default.

```
point := self._clamp_device_point(x, y)
if !verify { return self._tap_once(point.x, point.y) }        // FAST PATH, no verification key

retries         = clamp(int(retries), 0, 4)
retry_radius_px = clamp(int(retry_radius_px), 1, 96)
settle_s        = clamp(float(settle_s), 0.1, 2.0)

baseline, err := self.screenshot()
if err is (AdbError | OSError):
    result := self._tap_once(point.x, point.y)
    result["verification"] = {"requested": true, "available": false, "attempts": 1}   // int!
    return result
```

**Offsets** — a 5-point cross, truncated to `retries+1` entries, in this exact order:

```
(0, 0), (0, -R), (0, +R), (-R, 0), (+R, 0)          R = retry_radius_px
```

So `retries=0` → 1 attempt (the target only); `retries=2` (the default) → target, above, below;
`retries=4` → all five. `retries` above 4 is clamped to 4 (never more than 5 attempts).
The order is **up, down, left, right** — i.e. vertical neighbours are tried before horizontal.

**Loop** (for each `(dx, dy)` in order):

```
candidate := self._clamp_device_point(point.x+dx, point.y+dy)
last_result = self._tap_once(candidate.x, candidate.y)
sleep(settle_s)                                        // 0.45 s default
try:
    after        := self.screenshot()
    change_score := _screenshot_change_score(baseline, after)
    changed      := change_score >= 0.035
except (AdbError, OSError):
    changed = false; change_score = 0.0; after = {}
attempts = append(attempts, {
    "point":          [candidate.x, candidate.y],
    "screen_changed": changed,
    "change_score":   round(change_score, 4),
})
if changed:
    last_result["verification"] = {
        "requested": true, "available": true, "verified": true,
        "attempts": attempts,
        "after_size_bytes": after.get("size_bytes", 0),
    }
    return last_result
```

**Give-up** (loop exhausted):

```
last_result["verification"] = {
    "requested": true, "available": true, "verified": false,
    "attempts": attempts,
    "hint": "No screen change detected after tapping the target and nearby points.",
}
return last_result
```

Critical details:

* `baseline` is captured **once, before the first tap**, and every attempt is compared against
  that same baseline — not against the previous attempt. So a change caused by attempt #1 that
  persists will make attempt #1 succeed; the comparison is cumulative-from-start, which is what
  we want.
* Duplicate points are **not** de-duplicated: near an edge, clamping can make two offsets land
  on the same pixel and the same tap is repeated. Replicate (harmless, and de-duping would
  change the `attempts` array the model sees).
* The `verification` key is **merged into** the `_tap_once` result dict, so its position in the
  key order is last (after `ok/action/x/y/serial` or after the runtime's keys).
* On the "baseline capture failed" path, `attempts` is the **integer 1**, not a list.
  See **BUG-2**.
* `settle_s` is a wall-clock sleep between the tap and the after-screenshot; the after-screenshot
  itself takes ~150–400 ms on a real device, so the effective settle is longer.

### 4.5 `_screenshot_change_score(before, after) -> float`

```
b := before["png_bytes"]; a := after["png_bytes"]
if b or a is not []byte -> return 0.0                 // e.g. after == {}
decode both PNGs; convert to RGB; resize BOTH to exactly (width=72, height=128)
   using PIL's DEFAULT resample for RGB images == BICUBIC
diff := per-pixel per-channel absolute difference
changed := count of pixels where max(dR, dG, dB) >= 20
return float(changed) / (72*128)                       // /9216
on any decode/resize error (OSError|ValueError) -> return 0.0
```

Threshold in `tap()`: `change_score >= 0.035`, i.e. **≥ 323 changed pixels of 9216**
(`0.035 * 9216 = 322.56`).

> **Go implementation guidance.** Nothing here needs to be bit-identical — the only consumer
> is the `>= 0.035` predicate and the `change_score` shown to the model. But the *sensitivity*
> must match, and that depends on the resampler:
> * 72x128 is a **fixed** target, so aspect ratio is not preserved (1080x2280 → 72x128 squashes
>   horizontally). Keep the fixed 72x128; do not "fix" it to be aspect-correct.
> * BICUBIC downsampling in Pillow is a *proper* area-weighted resample (Pillow's `resize`
>   uses a support-scaled filter, not point sampling), so every source pixel contributes.
>   A naive nearest-neighbour 72x128 sample in Go would be far noisier and would fire on
>   cursor blinks. Use `golang.org/x/image/draw` — **not allowed** (stdlib + MCP SDK only) —
>   so implement a **box/area average** downscale by hand: for each of the 72x128 output cells,
>   average the source pixels in its rectangle. Area-average is slightly *less* sensitive to
>   thin high-contrast edges than bicubic (no negative lobes/overshoot). Recommendation:
>   keep the `20` per-channel threshold and the `0.035` fraction as-is; area-average plus
>   those constants matches the Python within a few tenths of a percent on real screens.
> * `image/png` in the stdlib decodes `screencap -p` output fine (it is 8-bit RGBA).
>   "Convert to RGB" = drop alpha, no un-premultiplication (Android's PNG is not premultiplied).
> * `round(change_score, 4)` → Python's round-half-to-even on the double. Go: use
>   `math.Round(v*1e4)/1e4` and accept the half-way tie difference (it can only differ in the
>   4th decimal of a display-only field).

### 4.6 `tap_relative(x, y, *, verify=True, retries=2) -> dict`

```
if !(0.0 <= x <= 1.0 && 0.0 <= y <= 1.0) -> AdbError("relative x and y must be between 0 and 1")
w, h := self._required_screen_size()
device_x := round(x * float(w-1))
device_y := round(y * float(h-1))
→ {
    "ok": true,
    "coordinate_space": "relative",
    "source": [x, y],                       // the ORIGINAL floats, echoed back
    "device_point": [device_x, device_y],
    "tap": self.tap(device_x, device_y, verify=verify, retries=retries),
  }
```

Bounds are **inclusive** on both ends. Mapping is to `w-1`/`h-1`, so `x=1.0` → the last column.

`scrcpymac_ui_tap` calls `tap_relative(x, y, verify=False, retries=0)` when the stream is not
active (and bypasses actions entirely when it is).

### 4.7 `tap_image(x, y, image_width, image_height, *, verify=True, retries=2) -> dict`

```
if image_width <= 0 || image_height <= 0 -> AdbError("image_width and image_height must be positive")
if !(0 <= x < image_width && 0 <= y < image_height) -> AdbError("image point must be inside image_width x image_height")
w, h := self._required_screen_size()
device_x := round( (float(x) / float(max(image_width-1, 1)))  * float(w-1) )
device_y := round( (float(y) / float(max(image_height-1, 1))) * float(h-1) )
→ {
    "ok": true,
    "coordinate_space": "image",
    "source": {"point": [x, y], "size": [image_width, image_height]},
    "device_point": [device_x, device_y],
    "device_size": [w, h],
    "tap": self.tap(device_x, device_y, verify=verify, retries=retries),
  }
```

Note `tap_relative` has **no** `device_size` key but `tap_image` does. Do not "harmonise" them.

The `max(image_width-1, 1)` guard means a 1-pixel-wide image maps `x=0` → `0` (the only legal x).

> **Rounding gotcha.** Python's builtin `round()` is **banker's rounding** (ties to even) and Go's
> `math.Round` is ties-away-from-zero. Ties happen whenever the product lands exactly on `.5`,
> which is common: e.g. `x=0.5`, `w=1080` → `0.5*1079 = 539.5` → Python `540` (even) vs Go `540`
> (away) — same here, but `0.5*1077 = 538.5` → Python `538`, Go `539`. Implement
> `pyRound(v float64) int` as round-half-to-even (`math.RoundToEven`) and use it for every
> `round()` in this file: `tap_relative`, `tap_image`, and `_scroll_once`'s implicit `int()`
> (which is truncation, not rounding — see §5.7).

---

## 5. Input primitives

### 5.1 `swipe(x1, y1, x2, y2, duration_ms=300) -> dict`

```
st := runtime.status()
if st["state"] == "streaming":
    w := max(1, int(st["deviceWidth"])); h := max(1, int(st["deviceHeight"]))
    result = runtime.swipe_relative(
        float(int(x1))/float(max(w-1,1)), float(int(y1))/float(max(h-1,1)),
        float(int(x2))/float(max(w-1,1)), float(int(y2))/float(max(h-1,1)),
        duration_ms = duration_ms)                      // passed through UNCLAMPED here
else:
    client := self._ready()
    client.shell(fmt.Sprintf("input swipe %d %d %d %d %d", int(x1), int(y1), int(x2), int(y2), int(duration_ms)))
    result = {"ok": true, "action": "swipe", "from": [x1,y1], "to": [x2,y2],
              "duration_ms": duration_ms, "serial": client.serial}
self._invalidate_ui_tree_cache()
return result
```

* **No clamping of coordinates to the screen** in `swipe` (unlike `tap`). Out-of-range values
  are passed straight to `input swipe`, or converted to out-of-range relatives which the runtime
  then clamps to `[0,1]`. Replicate.
* The adb result uses snake_case `duration_ms`; the runtime result uses camelCase `durationMs`.
  Replicate both.
* `runtime.swipe_relative` itself clamps `duration_ms` to `[0, 10000]` and interpolates
  `steps = clamp(round(duration_ms/16), 1, 120)` move events, sleeping to hit the wall-clock
  schedule — so a streaming swipe **blocks for ~duration_ms**. `mcp_ui.scrcpymac_ui_swipe`
  pre-clamps `duration_ms` to `[0, 10000]`; `phone_swipe` does not.

### 5.2 `long_press(x, y, duration_ms=1000) -> dict`

Literally `return self.swipe(x, y, x, y, duration_ms=duration_ms)`. The result therefore says
`"action": "swipe"` with `from == to`, **not** `"long_press"`. Replicate — the model reads it.

### 5.3 `key(name) -> dict`

```
KEYCODES = {back:4, home:3, recents:187, enter:66, delete:67,
            tab:61, menu:82, power:26, volume_up:24, volume_down:25, paste:279}

key := strings.ToLower(strings.TrimSpace(name))
if key not in KEYCODES:
    -> AdbError("Unknown key '<name>'. Supported: back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down, paste")
       // NOTE: <name> is the ORIGINAL argument rendered with Python repr() -> single quotes,
       // e.g.  Unknown key 'Foo'. …   The key list is joined with ", " in the dict's
       // insertion order shown above.
if runtime.is_active():
    result = runtime.key(key)          // ScrcpyRuntimeError -> re-raised as AdbError(str(exc))
else:
    client := self._ready()
    code   := KEYCODES[key]
    client.shell(fmt.Sprintf("input keyevent %d", code))
    result = {"ok": true, "action": "key", "key": key, "code": code, "serial": client.serial}
self._invalidate_ui_tree_cache()
return result
```

Note the adb result has a `code` key; the runtime result does not. Replicate.
In Go, `KEYCODES` must be an **ordered** structure (slice of pairs or a map plus an explicit
order slice) so the error message's key list matches byte-for-byte.

> **BUG-3 (real, hit in practice).** `scrcpy_runtime.KEYCODES` is a *different* map that
> **omits `paste`**. So `key("paste")` while the stream is active passes the actions-level
> validation, reaches `runtime.key("paste")`, and fails with
> `AdbError("Unknown key 'paste'. Supported: back, home, recents, enter, delete, tab, menu, power, volume_up, volume_down")`
> (note: no `paste` in that list). Off-stream it works.
> **Recommendation: FIX in Go** by adding `paste: 279` to the runtime's keycode map. The tool
> contract (`phone_key` accepts `paste`, documented in its docstring) says it should work; the
> current behaviour is a plain inconsistency, not something a caller can depend on. Document the
> fix in the migration notes.

### 5.4 `type_text(text) -> dict`

```
client := self._ready()
if text == "" -> AdbError("text must not be empty")
escaped := strings.ReplaceAll(text, "%", "%25")
escaped  = strings.ReplaceAll(escaped, " ", "%s")     // ORDER MATTERS: % first, then space
client.shell("input text " + shellQuote(escaped))
self._invalidate_ui_tree_cache()
→ {"ok": true, "action": "type", "length": len(text), "serial": client.serial}
```

* `len(text)` is the **Python `str` length = number of Unicode code points**, not bytes and not
  UTF-16 units. In Go use `utf8.RuneCountInString(text)`, **not** `len(text)`.
  (Astral-plane emoji count as 1 in Python 3 and 1 rune in Go — consistent.)
* The `%` → `%25` then ` ` → `%s` order is what makes it reversible: a literal `%s` typed by the
  user becomes `%25s`, and a literal space becomes `%s`. `input text` on Android decodes `%s`
  as space; other `%XX` sequences are **not** decoded by `input text` on most Android versions,
  so a literal `%` really does get typed as `%25` on many devices. That is a pre-existing
  fidelity bug in the Python; it is also the only escape that keeps spaces working.
  **Recommendation: replicate exactly.** `phone_type`'s docstring already says
  "Type short ASCII text. Do not use for Chinese — use phone_paste instead."

#### 5.4.1 `shellQuote` — Python `shlex.quote`, exactly

This is the **crucial** escaping rule and it must be reimplemented precisely.

```
func shellQuote(s string) string {
    if s == "" { return "''" }
    if !containsUnsafe(s) { return s }
    return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
```

`containsUnsafe(s)` is Python's `re.compile(r'[^\w@%+=:,./-]', re.ASCII).search(s) is not None`.
With `re.ASCII`, `\w` is exactly `[A-Za-z0-9_]`. So the **safe set is exactly**:

```
A-Z  a-z  0-9  _  @  %  +  =  :  ,  .  /  -
```

Every other byte/rune — including space, `$`, backtick, `"`, `'`, `\`, `;`, `|`, `&`, `<`, `>`,
`(`, `)`, `{`, `}`, `[`, `]`, `!`, `#`, `*`, `?`, `~`, `^`, newline, **and every non-ASCII rune
(Chinese, emoji)** — is unsafe and forces single-quoting.

Verified against the live interpreter:

| input | `shlex.quote` output |
|---|---|
| `abc` | `abc` |
| `a-b_c.d/e:f,g+h=i@j%k` | `a-b_c.d/e:f,g+h=i@j%k` |
| `你好` | `'你好'` |
| `你好 世界` | `'你好 世界'` |
| `emoji😀` | `'emoji😀'` |
| `$HOME` | `'$HOME'` |
| `a b` | `'a b'` |
| `it's` | `'it'"'"'s'` |
| `` (empty) | `''` |

Because `type_text` replaces spaces with `%s` *before* quoting, an ASCII-only string with
spaces ends up **unquoted** (`hello%sworld`) — that is fine, `%s` is in the safe set.

The quoted string is then concatenated into a single command string that is handed to
`adb shell` as **one argv element**. adb forwards the whole string to the device's `sh -c`,
which does the unquoting. Do **not** additionally quote for the host shell — Go's `exec.Command`
passes argv directly, matching Python's `subprocess.run(list)`. Both avoid a host shell entirely.

### 5.5 `paste(text) -> dict`

```
if text == "" -> AdbError("text must not be empty")
if runtime.is_active():
    result = runtime.paste(text)      // ScrcpyRuntimeError -> AdbError(str(exc))
                                      // scrcpy SET_CLIPBOARD packet, paste=1 flag; no adb, no sleep
else:
    client := self._ready()
    client.shell("cmd clipboard set-text " + shellQuote(text))
    sleep(0.15)                                            // 150 ms, fixed
    client.shell("input keyevent 279")                     // KEYCODES["paste"]
    result = {"ok": true, "action": "paste", "length": len(text), "serial": client.serial}
self._invalidate_ui_tree_cache()
return result
```

* Clipboard mechanism (adb path): `cmd clipboard set-text <text>` (Android 10+/`cmd` shell
  service), then keycode **279 = `KEYCODE_PASTE`** into the focused field. The 150 ms sleep
  covers the clipboard service round-trip; without it the paste keyevent can beat the write.
* `length` is again the **rune count**.
* Chinese/emoji work here because `shellQuote` single-quotes any non-ASCII string, and
  `cmd clipboard set-text` takes UTF-8 bytes.
* No verification, no retry, no check that anything was actually pasted.
* `scrcpymac_ui_paste` prefers `runtime.paste` directly when the stream is active and only
  falls back to `actions.paste`.

### 5.6 `launch_app(package, activity=nil) -> dict`

```
client := self._ready()
if activity != "" (non-nil, non-empty):
    client.shell("am start -n " + package + "/" + activity)
else:
    client.shell("monkey -p " + package + " -c android.intent.category.LAUNCHER 1")
self._invalidate_ui_tree_cache()
sleep(1.0)                                   // fixed 1 second, AFTER invalidating the cache
→ {
    "ok": true, "action": "launch",
    "package": package,
    "activity": activity,                    // the ORIGINAL value: nil -> JSON null, else string
    "foreground": self._foreground_app(),     // {package, activity, raw}
    "serial": client.serial,
  }
```

`server.py` passes `activity=activity or None`, so `phone_launch_app(package)` (empty activity)
yields `"activity": null` in the JSON. In Go, use `*string`/`json:",omitempty"`-free pointer so
`null` is emitted, not `""`.

> **BUG-4.** Neither `package` nor `activity` is quoted. `phone_launch_app(package="a; rm -rf /sdcard")`
> executes on the device. **Recommendation: replicate the command shape but ADD `shellQuote`**
> around `package` and `package + "/" + activity`. Quoting a normal package name is a no-op
> (`.` and alphanumerics are in the safe set, and `/` is too), so the emitted command is
> byte-identical for every legitimate input — zero contract risk, real hardening. Note
> `monkey`'s `-p` argument must be quoted as a single unit; the component `pkg/act` likewise.

### 5.7 `_scroll_once(direction="up")`

Only used by `_poll_for_node`'s `scroll_to_find`.

```
size := self._screen_size()
if size == nil { return }                    // silently does nothing
w, h := size
x := w / 2                                    // Python floor division on ints
if direction == "up":  y1, y2 := int(h*0.7), int(h*0.3)      // int() = TRUNCATION toward zero
else:                  y1, y2 := int(h*0.3), int(h*0.7)
self.swipe(x, y1, x, y2, duration_ms=350)     // this invalidates the ui_tree cache
sleep(0.4)
```

`direction` is always `"up"` at every call site: **content moves up, revealing what is below**
(finger drags from 70 % height to 30 % height). `int(h*0.7)` is IEEE-754 double multiplication
followed by **truncation toward zero**, not rounding — the product is frequently not exact
(`2280*0.7 == 1596.0000000000002` → `1596`; a height whose product lands just below an integer
truncates down). Go's `int(float64(h) * 0.7)` performs the identical double multiply and the
identical truncation, so the values match bit-for-bit. Do **not** substitute `h*7/10` integer
maths — that would differ. Verified on the attached device (`h = 2280`): `y1 = 1596`, `y2 = 684`.

---

## 6. `ui_tree(compact=True, force_refresh=False) -> dict`

### 6.1 Cache

`_ui_tree_cache` holds the **last successful result dict** (either a compact one, which has a
`nodes` key, or a raw one, which has an `xml` key).

```
if !force_refresh && _ui_tree_cache != nil:
    cached := _ui_tree_cache
    if compact  && cached has "nodes" { return shallowCopy(cached) }
    if !compact && cached has "xml"   { return shallowCopy(cached) }
    // else: fall through and re-dump (mode mismatch)
```

* The return is a **shallow copy** (`dict(cached)`), so top-level mutation by the caller
  (`server.py`'s `payload.setdefault("ok", True)`) does not poison the cache, but the `nodes`
  slice is **shared**. Go: copy the top-level struct/map, share the node slice — or just copy
  both, which is safer and observationally identical (nothing mutates nodes).
* The cache is invalidated by: `select_device`, `_tap_once`, `swipe`, `key`, `type_text`,
  `paste`, `launch_app`, and the `ET.ParseError` path. It is **not** invalidated by
  `screenshot`, `shell`, `current_app`, `device_info`, or the Wi-Fi methods.
  Notably `phone_shell("input tap 100 100")` leaves a stale tree cached. Replicate.
* The cache has **no TTL**. A tree can be arbitrarily stale if only non-invalidating calls happen.
  `_poll_for_node` works around this with `force_refresh=attempt > 0`.

### 6.2 Dump

```
_dump_ui_xml(): client := self._ready(); return client.ui_tree_xml(), client.serial
```

```
xml, serial := self._dump_ui_xml()
if strings.TrimSpace(xml) == "":
    sleep(0.3)                            // one retry only; uiautomator can return nothing
    xml, serial = self._dump_ui_xml()     // right after a navigation/animation
```

The one retry is unconditional-on-empty and happens **at most once**. If the second dump is also
empty, execution continues and `ET.fromstring("")` raises → the degraded path (§6.5).

### 6.3 Non-compact result

```
if !compact:
    result := {"ok": true, "xml": xml, "serial": serial}
    _ui_tree_cache = result
    return result
```

`xml` is the full stripped `uiautomator dump` output including the
`<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>` prologue. It is embedded as a JSON
string, so it is potentially hundreds of KB. No truncation.

### 6.4 Compact result — node filter and shape

Walk **every** `<node>` element in the document, in **document order (depth-first pre-order)**.
Python's `root.iter("node")` visits descendants in document order; Go's streaming
`xml.Decoder` `Token()` loop naturally produces the same order — use that, do not sort.

Per node, read attributes (missing attribute ⇒ `""`):

```
text       := strings.TrimSpace(attr["text"])            // Python str.strip() = Unicode whitespace
desc       := strings.TrimSpace(attr["content-desc"])
cls        := attr["class"]                              // NOT trimmed
clickable  := attr["clickable"]  == "true"
scrollable := attr["scrollable"] == "true"
checkable  := attr["checkable"]  == "true"
editable   := strings.Contains(cls, "EditText")          // substring on the class name
```

**Keep the node iff** `text != "" || desc != "" || clickable || scrollable || editable || checkable`.
Everything else is dropped. `enabled`, `focused`, `password`, `selected`, `long-clickable`,
`focusable`, `package`, `index`, `bounds` do **not** influence inclusion.

> Python's `str.strip()` strips Unicode whitespace (including U+00A0? — **no**, `str.strip()`
> strips characters where `str.isspace()` is true, which includes U+00A0 NBSP, U+3000
> ideographic space, U+2000–U+200A, and the ASCII set). Go's `strings.TrimSpace` uses
> `unicode.IsSpace`, which **excludes U+200B** (both do) but **includes** U+0085 and U+00A0 —
> matching Python closely enough. The one divergence: Python's `isspace()` is true for
> U+001C–U+001F (file/group/record/unit separators) while Go's `unicode.IsSpace` is false.
> These never appear in `uiautomator` text. Use `strings.TrimSpace`.

**Emitted node object** (keys in exactly this order):

```
{
  "index":        <position in the OUTPUT list, i.e. len(nodes) before append — NOT the XML index attribute>,
  "text":         text,
  "content_desc": desc,
  "resource_id":  attr["resource-id"],       // raw, not trimmed
  "class":        cls,
  "clickable":    clickable,
  "bounds":       attr["bounds"],            // raw string, e.g. "[0,80][1080,2153]"
  "center":       _bounds_center(bounds),    // [cx, cy] or null
}
```

then, **appended in this order and only when noteworthy**:

```
if scrollable                    -> "scrollable": true
if attr["enabled"]  == "false"   -> "enabled": false        // present ONLY when disabled
if attr["password"] == "true"    -> "password": true
if attr["focused"]  == "true"    -> "focused": true
if attr["selected"] == "true"    -> "selected": true
if checkable                     -> "checkable": true, then "checked": (attr["checked"] == "true")
```

Notes:

* `"checked"` is emitted **only alongside `checkable`**, and it can be `false`. Every other
  optional flag is emitted only in its non-default (`true`, or `enabled:false`) state.
  Absence means the default (`scrollable:false`, `enabled:true`, `password:false`,
  `focused:false`, `selected:false`, `checkable:false`).
* `"index"` is the **compact-list ordinal**, deliberately renumbered. It is what a human/model
  can quote back, but note **no tool takes it** — `find_and_tap`'s `index` is an index into the
  *matches*, not into this list. Do not conflate them.
* `editable` is computed but **never emitted** — it only affects inclusion. Replicate.
* In Go, node objects need per-node key ordering, so use an ordered representation
  (a struct with `omitempty`-style pointers won't reproduce the ordering rules exactly because
  `enabled:false` must appear only when false and `checked:false` must appear when checkable).
  Recommended: a small `orderedMap`-style type, or a struct with pointer fields
  (`Scrollable *bool`, `Enabled *bool`, `Password *bool`, `Focused *bool`, `Selected *bool`,
  `Checkable *bool`, `Checked *bool`) and `json:",omitempty"` — pointer+omitempty gives exactly
  the right presence semantics, and struct field order gives exactly the right key order.

`_bounds_center(bounds)`:

```
m := regexp `\[(\d+),(\d+)\]\[(\d+),(\d+)\]` applied with re.match (anchored at the START only,
     NOT fullmatch — trailing junk is allowed)
if no match -> null
x1,y1,x2,y2 := ints
return [ (x1+x2)/2, (y1+y2)/2 ]        // Python floor division of non-negative ints == Go int division
```

Only non-negative integers match (`\d+`), so bounds with negative coordinates (off-screen nodes
on some ROMs) yield `null` and are untappable by `find_and_tap`. Replicate.
Go: anchor with `^` and use `FindStringSubmatch`.

### 6.5 Parse failure → degraded

```
except ET.ParseError:
    self._invalidate_ui_tree_cache()          // never cache a broken dump
    return {
      "ok": true,                             // NOTE: ok is TRUE even though the dump failed
      "xml": xml,                             // whatever came back, possibly ""
      "serial": serial,
      "parse_error": true,
      "degraded": true,
      "hint": "UI dump was empty or unparseable. Retry phone_ui_tree or fall back to phone_screenshot for vision.",
    }
```

The hint string is assembled from an implicit Python string concatenation; the exact single-line
value is: `UI dump was empty or unparseable. Retry phone_ui_tree or fall back to phone_screenshot for vision.`

This branch fires for an empty dump (after the one retry), truncated XML, or invalid XML.
Go: any error from the XML decoder — including EOF on empty input — maps here.

### 6.6 Success → possibly degraded

```
has_webview := any node where strings.Contains(node["class"], "WebView")
interactive := count of nodes where node["clickable"] == true OR node["text"] != ""
result := {"ok": true, "nodes": nodes, "count": len(nodes), "serial": serial}
if has_webview || interactive < 3:
    result["degraded"] = true
    result["hint"] = "UI tree looks incomplete (WebView/Compose/custom-drawn). Fall back to phone_screenshot for vision."
_ui_tree_cache = result
return result
```

Exact `degraded` trigger, restated: **`degraded: true` is set when (a) the XML failed to parse
(§6.5), or (b) any kept node's `class` contains the substring `WebView`, or (c) fewer than 3
kept nodes have `clickable == true` or a non-empty `text`.** `count` is the *total* kept nodes,
which can be large while `interactive < 3` still fires.

The `interactive` predicate is `n.get("clickable") or n.get("text")` — Python truthiness, so a
node with `clickable:false` and `text:""` does not count, and a node with `text:"0"` **does**
(non-empty string is truthy). Go: `n.Clickable || n.Text != ""`.

The two hint strings are different; keep both verbatim:
* parse-error hint: `UI dump was empty or unparseable. Retry phone_ui_tree or fall back to phone_screenshot for vision.`
* degraded hint: `UI tree looks incomplete (WebView/Compose/custom-drawn). Fall back to phone_screenshot for vision.`

Key order on the compact success path: `ok, nodes, count, serial[, degraded, hint]`.
Key order on the parse-error path: `ok, xml, serial, parse_error, degraded, hint`.
Key order on the non-compact path: `ok, xml, serial`.

---

## 7. `NodeCriteria` — selector matching

```
NodeCriteria(text=nil, content_desc=nil, resource_id=nil, class_name=nil,
             require_all=false, exact=false, clickable_only=false, enabled_only=true)
```

`clickable_only` and `enabled_only` are **never overridden** by any caller in the codebase —
`find_and_tap` and `wait_for_text` both use the defaults. Keep them as fields anyway.

### 7.1 `_as_list(value) -> []string`

```
nil                  -> []
list/tuple           -> [str(v) for v in value if v is not None and v != ""]
scalar               -> [str(value)] if value != "" else []
```

So each of the four selector fields is a **list of alternatives**. The MCP tool layer only ever
passes a single string (`text or None`), but `recipes/wechat.py` passes real lists
(`["搜索","Search"]`). In Go, accept `any` at the actions API and normalise, or expose
`[]string` and let the MCP layer wrap a single value — the latter is cleaner and equivalent
as long as empty strings are dropped.

### 7.2 Construction guard

If all four normalised lists are empty →
`AdbError("NodeCriteria requires at least one attribute")`.
`server.py` guards earlier with `AdbError("Provide text, content_desc, resource_id, or class_name")`,
so the NodeCriteria message is only reachable from an internal caller.

### 7.3 `matches(node) -> bool`

```
if enabled_only && node["enabled"] is present and == false -> false
     // "enabled" is only present when the node is DISABLED, so this excludes greyed-out nodes.
     // Python: node.get("enabled") is False — an absent key is None, which is not False.
if clickable_only && !truthy(node["clickable"]) -> false

groups := [
   group(text,         node["text"]),
   group(content_desc, node["content_desc"]),
   group(resource_id,  node["resource_id"]),
   group(class_name,   node["class"]),        // NOTE the node key is "class", not "class_name"
]
where group(needles, value) = nil (tri-state "not specified") if len(needles)==0
                              else _any_match(needles, value, exact)

specified := groups with non-nil entries
if len(specified) == 0 -> false
return require_all ? all(specified) : any(specified)
```

`_any_match(needles, haystack, exact)`:

```
if len(needles) == 0 -> false
text := haystack or ""
if exact:  return any(needle == text)
else:      return any(needle != "" && strings.Contains(text, needle))
```

* **Substring is the default** (`exact=false`), matching is **case-sensitive**, and there is no
  normalisation (no NFKC, no whitespace collapsing). The haystack `text`/`content_desc` were
  already `strip()`ped when the compact tree was built; `resource_id` and `class` were not.
* `resource_id` matching is a substring match too, so `resource_id="menu_search"` matches
  `com.tencent.mm:id/menu_search` — this is exactly what `recipes/wechat.py` relies on.
  Do **not** "improve" it to a suffix or exact match.
* With `require_all=true`, only the **specified** attributes must all hit — unspecified ones
  are excluded from the AND, not treated as failures.
* Multiple alternatives inside one field are always OR'd, even under `require_all`.

### 7.4 `describe() -> string`

Used only inside error messages. Builds `", "`-joined parts, in this fixed order, skipping
empty lists:

```
text=<repr of []string>            e.g.  text=['搜索', 'Search']
content_desc=<repr>
resource_id=<repr>
class=<repr>                       <-- the LABEL is "class", not "class_name"
require_all=True                   (only when true)
exact=True                         (only when true)
```

`repr` of a Python list of strings: `['a', 'b']` — square brackets, single-quoted elements,
`, ` separator, and **`ensure_ascii` does not apply** (repr of a `str` in Python 3 keeps
printable non-ASCII literal, so `['搜索', 'Search']`). A string containing a `'` is repr'd with
double quotes (`["it's"]`). Implement a small `pyReprStringList([]string) string` helper in Go:
* element quoting: if the string contains `'` and not `"` → wrap in `"`; else wrap in `'` and
  backslash-escape any `'`. Also backslash-escape `\`, and escape non-printable chars as
  `\n`, `\r`, `\t`, `\xNN`, `\uNNNN`, `\UNNNNNNNN`.
* This only affects error text; a close approximation is acceptable, but the common cases
  (plain text, CJK) must be exact.

### 7.5 `_find_node(nodes, criteria, index=0)`

```
matches := [n for n in nodes if criteria.matches(n)]      // preserves tree order
if index < 0 || index >= len(matches) -> nil
return matches[index]
```

Negative `index` never matches (no Python-style negative indexing). `index` is an index into
the **match list**, not into the compact tree.

---

## 8. `_poll_for_node` — the shared wait/backoff engine

```
_poll_for_node(criteria, *, timeout_s, poll_interval_s=0.4, index=0, scroll_to_find=0)
        -> (node, last_tree)   or raises AdbError
```

```
deadline    := now() + timeout_s
last_tree   := {}
scrolls_used, attempt := 0, 0
for now() < deadline {
    last_tree = self.ui_tree(compact=true, force_refresh = attempt > 0)
    node := _find_node(last_tree["nodes"] or [], criteria, index)
    if node != nil { return node, last_tree }
    if scrolls_used < scroll_to_find {
        self._scroll_once(direction="up")       // swipe 350 ms + sleep 0.4 s; invalidates cache
        scrolls_used++
        attempt++
        continue                                 // re-dump immediately, NO backoff sleep
    }
    attempt++
    sleep( min( poll_interval_s * pow(1.5, max(attempt-1, 0)), 2.0 ) )
}
raise AdbError(...)
```

Exact cadence facts:

* **The deadline is checked only at the TOP of the loop.** A `timeout_s <= 0` performs
  **zero** dumps and raises immediately with `Last tree had 0 nodes`.
* **The first iteration uses the cache** (`force_refresh = attempt > 0` is false at attempt 0).
  So `find_and_tap` right after `launch_app` re-dumps (launch invalidated the cache), but
  `find_and_tap` right after a bare `screenshot` may match against a stale tree. Replicate —
  this is load-bearing for the fast path.
* Backoff is exponential with base 1.5 **capped at 2.0 s**, and the exponent uses the
  *post-increment* `attempt`. Concretely, with `poll_interval_s = 0.4` and no scrolling, the
  sleeps after successive failed dumps are:

  | failed dump # | `attempt` after ++ | sleep (s) |
  |---|---|---|
  | 1 | 1 | 0.4 |
  | 2 | 2 | 0.6 |
  | 3 | 3 | 0.9 |
  | 4 | 4 | 1.35 |
  | 5 | 5 | 2.0 (capped from 2.025) |
  | 6+ | ≥6 | 2.0 |

  With the default `timeout_s = 10` that is roughly 6–7 dumps before giving up (each dump
  itself costs 300–800 ms on a real device).
* **Scrolling consumes an `attempt` but skips the sleep**, so the first `scroll_to_find` scrolls
  happen back-to-back (each still costs the 350 ms swipe + 400 ms settle). After the scroll
  budget is exhausted, the backoff resumes from the *already-advanced* `attempt`, i.e. the
  first post-scroll sleep is already long. Replicate exactly.
* `_scroll_once` is a no-op if the screen size is unknown, but still burns a `scrolls_used`
  and an `attempt`. Replicate.

**Timeout error message** — built by string concatenation:

```
"Element not found within {timeout_s}s ({criteria.describe()}). Last tree had {count} nodes"
  + (", after {scrolls_used} scroll(s)"  if scroll_to_find else "")
  + "."
```

Details:
* `{timeout_s}` is Python `str(float)` of the value as passed: `10` (an int from a default) →
  `10`; `10.0` (a float from the MCP layer) → `10.0`; `min(15, 8)` → `8`. Because `server.py`
  declares `timeout_s: float = 10`, FastMCP coerces to float, so the tool path prints `10.0`.
  `wechat.py` passes `min(timeout_s, 6)` where `timeout_s: float = 15` → `6.0`.
  In Go, format with the shortest-roundtrip float representation and **always include a `.0`
  for integral values** (`strconv.FormatFloat(v, 'g', -1, 64)` gives `10`, not `10.0` — you need
  a helper that appends `.0` when the result has no `.`/`e`).
* `{count}` is `last_tree.get("count", 0)` — **0 when the last tree was the degraded parse-error
  shape** (which has no `count` key) or when no dump ran at all.
* The scroll suffix is appended when `scroll_to_find` is **truthy** (non-zero), regardless of
  how many scrolls actually happened.

Example: `Element not found within 10.0s (text=['Settings']). Last tree had 42 nodes.`
With scrolling: `Element not found within 10.0s (text=['Settings']). Last tree had 42 nodes, after 3 scroll(s).`

---

## 9. `find_and_tap`

```
find_and_tap(*, text=nil, content_desc=nil, resource_id=nil, class_name=nil,
             require_all=false, exact=false, index=0,
             timeout_s=10, poll_interval_s=0.4, scroll_to_find=0, verify=false)
```

MCP tool `phone_find_and_tap` defaults: `text="" content_desc="" resource_id="" class_name=""
require_all=False exact=False index=0 scroll_to_find=0 timeout_s=10.0 **verify=True**`.
Note the actions-level default for `verify` is `False` but the tool-level default is `True`;
`recipes/wechat.py` calls the actions API and therefore taps **without** verification.
`poll_interval_s` is **not exposed** by the tool.

```
criteria := NodeCriteria(text, content_desc, resource_id, class_name, require_all, exact)
node, _  := self._poll_for_node(criteria, timeout_s, poll_interval_s, index, scroll_to_find)
if node["center"] is nil/absent:
    -> AdbError("Matched node has no tappable bounds (<criteria.describe()>)")
x, y := node["center"][0], node["center"][1]
→ {"ok": true, "matched": node, "tap": self.tap(x, y, verify=verify)}
```

* `tap` here is called with **only** `verify` overridden — `retries` stays at the actions-level
  default of **2**, `retry_radius_px` at **32**, `settle_s` at **0.45**. The tool's `retries`
  parameter is not plumbed through `find_and_tap`.
* `matched` is the full compact node object (all its keys, in the order from §6.4).
* The empty-bounds check is `if not node.get("center")`, i.e. Python falsiness — `None` and
  an empty list both fail. `[0, 0]` is a **non-empty list → truthy**, so a node centred at the
  origin is still tappable. Go: `node.Center != nil`.
* `server.py` pre-validates that at least one selector string is non-empty, raising
  `AdbError("Provide text, content_desc, resource_id, or class_name")`.

Result key order: `ok, matched, tap` (then `_ok`'s `setdefault("ok")` is a no-op).

## 10. `wait_for_text(text, *, timeout_s=10, poll_interval_s=0.4)`

```
node, tree := self._poll_for_node(NodeCriteria(text=text), timeout_s, poll_interval_s)
      // index defaults to 0, scroll_to_find defaults to 0 -> NO scrolling
serial := tree["serial"] or (self._client.serial if self._client != nil else "")
→ {"ok": true, "found": node, "serial": serial}
```

* `text` may be a list (wechat passes `HOME_MARKERS` = 7 bilingual alternatives; any one
  matching wins).
* Matching is `text`-only, **substring**, `enabled_only=true`, `clickable_only=false`.
* It matches the node's `text` attribute only — **not** `content_desc`. A button whose label
  lives only in `content-desc` will never be found by `wait_for_text`. Replicate; `wechat.py`
  works around it by also passing `content_desc` to `find_and_tap`.
* The `serial` fallback reads `_client.serial` directly (may be `nil` → `""`), never
  instantiating the client.

## 11. Small read-only methods

### `current_app() -> dict`

```
app    := self._foreground_app()                       // self._ready().current_app()
serial := self.selected_serial() or self._ready().serial
→ {"foreground": app, "serial": serial}
```

`_ready()` runs twice in the fallback case; harmless (`ensure_device` short-circuits once
`serial` is set). `foreground` = `{package, activity, raw}`.

### `device_info() -> dict`

```
rt := runtime.status()
if rt["state"] == "streaming":
    → {
        "serial": rt["serial"],
        "screen": {"width": rt["deviceWidth"], "height": rt["deviceHeight"]},
        "video":  {"width": rt["frameWidth"], "height": rt["frameHeight"],
                   "fps": rt["fps"], "codec": rt["codec"]},
        "foreground": self._foreground_app(),     // still an adb call
        "backend": "plugin-h264",
      }
client := self._ready()
w, h := client.screen_size()
app  := client.current_app()
→ {"serial": client.serial, "screen": {"width": w, "height": h},
   "foreground": app, "backend": "adb"}
```

The streaming branch has a `video` sub-object; the adb branch does **not**. `rt["fps"]` is a
float rounded to 1 decimal by the runtime; `rt["codec"]` is e.g. `"avc1.42E01E"`.
The streaming branch indexes `rt["serial"]` etc. directly — a `KeyError` if `state == "streaming"`
without metadata, which the runtime guarantees cannot happen.

`mcp_ui.scrcpymac_ui_swipe`'s adb fallback reads `info["screen"]["width"|"height"]`, so both
branches must keep the `screen` sub-object shape.

### `shell(command) -> dict`

```
client := self._ready()
output := client.shell(command)                 // adb shell <command>, one argv, 60 s timeout
→ {"ok": true, "output": output, "serial": client.serial}
```

`command` is passed through **verbatim, unquoted, unfiltered** — that is the point of the tool.
Output is `strip()`ped; stderr is discarded on success; non-zero exit raises `AdbError`.
The ui-tree cache is **not** invalidated (see §6.1). Replicate.

---

## 12. Wi-Fi adb

### 12.1 `enable_wifi_adb(port=5555) -> dict`

```
client := self._ready()
output := client.enable_tcpip(port)      // == client.shell("tcpip " + str(int(port)))
→ {"ok": true, "action": "enable_tcpip", "port": port, "output": output, "serial": client.serial}
```

> **BUG-1 — this method cannot work as written.** `tcpip` is an **adb host/service command**
> (`adb -s S tcpip 5555`), not a device shell command. Verified live against the attached
> OnePlus 6 (serial `2f019965`):
>
> ```
> $ adb -s 2f019965 shell tcpip 5555
> /system/bin/sh: tcpip: inaccessible or not found
> exit=127
> ```
>
> With `check=True`, exit 127 makes `AdbClient.run` raise
> `AdbError("adb shell tcpip 5555 failed: /system/bin/sh: tcpip: inaccessible or not found")`,
> so `phone_enable_wifi_adb` **always** returns
> `{"ok": false, "error": "adb shell tcpip 5555 failed: …"}` on every device.
>
> **Recommendation: FIX in Go.** Emit `adb [-s S] tcpip <port>` (a host command,
> `run(["tcpip", str(port)])`). No caller can be depending on a tool that has never once
> succeeded, and the tool's whole purpose (`"Enable TCP/IP adb on a USB-connected device
> (required before Wi-Fi connect)"`) is unreachable otherwise. Keep the result keys identical
> — `{"ok", "action":"enable_tcpip", "port", "output", "serial"}`, with `output` now being the
> real adb stdout (`restarting in TCP mode port: 5555`). Note this in the migration notes as an
> intentional behaviour fix.

### 12.2 `get_device_ip() -> dict`

```
client := self._ready()
ip     := client.device_wifi_ip()
→ {"ok": true, "ip": ip, "serial": client.serial}
```

`device_wifi_ip()` — two attempts, in order:

1. `shell("ip route | awk '/wlan/ {print $9; exit}'")`
   → accept if the stripped output fully matches `^\d+\.\d+\.\d+\.\d+$`
   (Python `re.match` + `$`, so a trailing `\n` would still match — but the output is already
   stripped. Go: use `^\d+\.\d+\.\d+\.\d+$` with `MatchString`.)
2. `shell("ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1")`
   → same validation.
3. otherwise → `AdbError("Could not detect device Wi-Fi IP. Is Wi-Fi connected?")`

Both command strings contain single quotes and `$` and are passed as **one argv element** to
`adb shell`; the device-side `sh` interprets them. In Go, pass the identical literal string —
do not re-quote. Verified live: attempt 1 returned `192.168.8.174` on the OnePlus 6.

The regex is loose (`\d+` per octet, no range check), so `999.999.999.999` would be accepted.
Replicate; it costs nothing.

### 12.3 `connect_wifi(host, port=5555) -> dict`

```
output := self.client.connect_wifi(host, port)     // NOTE: self.client, NOT self._ready()
        // adb [-s S] connect <target>, target = host if strings.Contains(host, ":") else host+":"+port
→ {"ok": true, "action": "connect_wifi",
   "target": host if ":" in host else fmt.Sprintf("%s:%d", host, port),
   "output": output}
```

* **No `serial` key** on this result (unlike the other Wi-Fi methods).
* `target` is recomputed in `actions.py` with the same rule the client used — keep both in sync.
* Uses `self.client` (no `ensure_device`), so it works with no device attached, which is the
  point. But `_base_cmd()` still prepends `-s <serial>` when one is set; adb ignores `-s` for
  `connect`. Replicate (harmless).
* `mcp_ui.scrcpymac_ui_connect_wifi` calls `actions.connect_wifi(host.strip(), port=port)`.
  `phone_connect_wifi` does **not** strip. Replicate both.
* adb's `connect` exits 0 even on `failed to connect to …`, so `output` carries the real result
  and `ok` is `true` regardless. Replicate — do not add success parsing.

### 12.4 `disconnect_wifi(host="") -> dict`

```
output := self.client.disconnect_wifi(host)
        // args = ["disconnect"]; if host != "" append (host if ":" in host else host+":5555")
        // NOTE the hardcoded 5555 in the client — disconnect has no port parameter
→ {"ok": true, "action": "disconnect_wifi", "output": output}
```

No `target`, no `serial` key. Empty host disconnects **all** TCP/IP devices.

---

## 13. `json_result(payload) -> string`

```python
json.dumps(payload, ensure_ascii=False, indent=2)
```

This is the single serialiser for **every** `phone_*` tool result (`mcp_ui`'s tools use
`structuredContent` instead and do not go through it).

Exact properties the Go port must reproduce:

| Property | Python | Go recipe |
|---|---|---|
| Indent | 2 spaces, nested | `enc.SetIndent("", "  ")` |
| Item separator | `,` followed by newline (with `indent`, Python trims the trailing space from `", "`) | same as Go's indented output |
| Key separator | `": "` | same |
| Key order | **dict insertion order**, i.e. the order the code writes them | use structs (field order) or an ordered map — **never** `map[string]any`, which Go sorts alphabetically |
| Non-ASCII | `ensure_ascii=False` → literal UTF-8 (`"发送"`, not `"发送"`) | default in Go |
| HTML chars | `<`, `>`, `&` emitted **literally** | **must call `enc.SetEscapeHTML(false)`** — Go escapes them to `<` etc. by default. This matters: `shell` output and `ui_tree` XML are full of `<`/`>`/`&` |
| U+2028 / U+2029 | emitted literally | Go's `json.Encoder` escapes them to ` `/` ` **even with `SetEscapeHTML(false)`**. Divergence is cosmetic and JSON-equivalent; accept it, or post-process if byte-identity is required |
| Control chars | `\b \f \n \r \t` for those five, `\uXXXX` for other `< 0x20` | same in Go |
| `/` | not escaped | same |
| Invalid UTF-8 in strings | Python `str` is always valid; adb output is decoded with `errors="replace"` so lone surrogates cannot appear | in Go, sanitise adb output with `strings.ToValidUTF8(s, "�")` to match the `errors="replace"` decode |
| **Floats** | `repr(float)` → shortest round-trip, and **integral floats keep `.0`**: `0.0`, `1.0`, `10.0`, `0.035` | **Go marshals `float64(0)` as `0` and `float64(1)` as `1`.** You need a custom float type with a `MarshalJSON` that emits Python's `repr`: shortest round-trip via `strconv.FormatFloat(v, 'r'…)` — practically `strconv.FormatFloat(v, 'g', -1, 64)` then append `.0` if the result contains no `.`, `e`, `E`, `n` (NaN), or `i` (Inf) |
| Large/small floats | Python switches to exponent at `1e16`/`1e-5` with `e+16`/`e-05` style | irrelevant here — the only floats reaching `json_result` are `change_score` (0..1, 4 dp) and `fps` (1 dp), plus echoed `source: [x, y]` from `tap_relative` and `timeout_s`-derived values inside error strings. Handle the common range correctly and don't over-engineer |
| Booleans / null | `true`/`false`/`null` | same |
| Integers | arbitrary precision, no `.0` | Go `int`/`int64` — make sure sizes/counts are integer types, not `float64` |

`server.py`'s `_ok(payload)` does `payload.setdefault("ok", True)` **before** serialising, so
`ok` lands **last** in the key order for any payload that did not already contain it
(`screenshot`, `device_info`, `current_app`). For payloads that already have `ok` first
(everything else), it stays first. Reproduce this positioning.

`server.py`'s `_err(exc)` is `json_result({"ok": false, "error": str(exc)})` — key order
`ok, error`.

---

## 14. What `recipes/wechat.py` pins down

`send_message(actions, contact, message, *, timeout_s=15.0)` exercises the actions API in a way
that constrains the port. Constants: `WECHAT_PACKAGE = "com.tencent.mm"`,
`SEARCH_LABELS = ["搜索","Search"]`, `SEND_LABELS = ["发送","Send"]`,
`HOME_MARKERS = ["微信","通讯录","发现","我","WeChat","Chats","Contacts"]`.

Sequence, with the exact actions calls:

1. reject empty/whitespace `contact` → `AdbError("contact must not be empty")`;
   empty/whitespace `message` → `AdbError("message must not be empty")`.
2. `actions.launch_app("com.tencent.mm")` → step `launch_wechat`.
3. `actions.wait_for_text(HOME_MARKERS, timeout_s=min(timeout_s, 8))`, `AdbError` swallowed →
   step `{"step":"wait_wechat_ready","skipped":true}`. **Proves `wait_for_text` must accept a
   list of alternatives.**
4. `actions.find_and_tap(text=SEARCH_LABELS, content_desc=SEARCH_LABELS,
   resource_id="menu_search", timeout_s=min(timeout_s, 6))` — i.e. **lists for `text` and
   `content_desc` simultaneously, `require_all=False` (OR across attributes), substring
   `resource_id`, and `verify` left at the actions default `False`.**
5. `actions.wait_for_text(SEARCH_LABELS, timeout_s=4)`; on `AdbError`, `sleep(0.5)` and mark
   skipped.
6. `actions.paste(contact)` — **`paste`, not `type_text`**, because the contact is Chinese.
7. `actions.find_and_tap(text=contact, timeout_s=timeout_s)`; on `AdbError` →
   `actions.key("enter")` then `sleep(0.8)`. **`key("enter")` must work in both backends.**
8. `actions.paste(message)`.
9. `actions.find_and_tap(text=SEND_LABELS, content_desc=SEND_LABELS, timeout_s=3)`;
   on `AdbError` → `actions.key("enter")`.
10. `sleep(0.5)`, `actions.screenshot()`, and read `["width"]`, `["height"]`, `["size_bytes"]`
    into `verification`. **Those three screenshot keys are load-bearing.**
11. Result: `{ok:true, recipe:"wechat_send_message", contact, message, steps, verification}`.
12. On any `AdbError`: takes a best-effort screenshot (falling back to
    `{"width":0,"height":0,"size_bytes":0}`) and re-raises
    `AdbError("<orig>. Steps completed: <len(steps)>. Last screenshot: <w>x<h>")`.

## 15. What `server/tests/` pins down

The pytest suite has **no direct tests for `actions.py`**. What it does pin, indirectly:

* `tests/test_mcp_ui.py::FakeActions` implements only `backend()`, `selected_serial()`,
  `devices()`, `select_device(serial)`, `screenshot()`. Its `screenshot()` returns
  `{serial, width, height, png_bytes, backend}` — **no `format`, `base64`, or `size_bytes`** —
  and `_preview_frame` still works, confirming that `mcp_ui` only depends on
  `serial`, `width`, `height`, `png_bytes`, and (via `.get`) `backend`.
* `select_device` is expected to return a dict containing at least `{"ok": true, "serial": …}`.
* `scrcpymac_ui_snapshot` numbers (see §3.1).
* `test_internal_tools_are_app_only` requires ≥10 `scrcpymac_ui_*` tools, each with
  `meta["ui"]["visibility"] == ["app"]` and no `resourceUri` in `meta["ui"]`.
* `tests/test_scrcpy_runtime.py` only touches `runtime.tap_relative(0.5, 0.25)`.

Nothing in the suite constrains the tap/verify heuristics, the ui-tree compaction, or the
selector semantics — **this document is the only specification of those.**

---

## 16. Bug register and porting decisions

| ID | Location | Description | Recommendation |
|---|---|---|---|
| **BUG-1** | `enable_wifi_adb` → `AdbClient.enable_tcpip` | Runs `adb shell tcpip <port>`; `tcpip` is a host command. Verified to exit 127 on the attached device, so the tool has **never** succeeded. | **FIX.** Use `adb [-s S] tcpip <port>`. Keep result keys identical. |
| **BUG-2** | `tap()` baseline-capture failure path | Sets `verification.attempts` to the **integer `1`** while every other path sets a **list of attempt objects**. A consumer doing `len(attempts)` or iterating breaks. | **FIX**, minimally: emit `"attempts": []` … **no** — that loses information. Recommended: keep the union but make it a list of one synthetic entry `[{"point":[x,y],"screen_changed":false,"change_score":0.0}]`. This is a schema *repair* on an unreachable-in-practice path (screenshot failing while adb still accepts taps). Document it. If contract paranoia wins, replicate the integer and type the field as `any`. |
| **BUG-3** | `key("paste")` while streaming | `scrcpy_runtime.KEYCODES` lacks `paste`, so the documented `paste` key fails only when the H.264 stream is up. | **FIX** — add `paste: 279` to the runtime keycode table. |
| **BUG-4** | `launch_app` | `package`/`activity` are interpolated into the device shell command unquoted → command injection via a tool argument. | **FIX** by wrapping with `shellQuote`; a no-op for every legitimate package/component string. |
| **BUG-5** | `long_press` | Result reports `"action": "swipe"`, not `"long_press"`, and carries `from`/`to` instead of `x`/`y`. | **REPLICATE.** It is a visible part of the contract and the model already reads it fine. |
| **BUG-6** | `ui_tree` cache | Not invalidated by `shell()`, has no TTL, and the first `_poll_for_node` iteration reads it. A `phone_shell("input tap …")` followed by `phone_find_and_tap` matches a stale tree. | **REPLICATE.** The staleness is load-bearing for latency and `force_refresh` covers the retry. Consider adding a short TTL only if a device-validation run shows it causes misses; that would be a deliberate, documented change. |
| **BUG-7** | `screenshot()` width/height | Python used `wm size`, so metadata could disagree with the actual image and shift visual taps under a display-size override. | **FIXED IN GO.** Decode PNG dimensions and map taps through WindowManager's current display size. |
| **BUG-8** | `tap`/`swipe` streaming paths | `ScrcpyRuntimeError` from `runtime.tap_relative`/`swipe_relative` is **not** wrapped into `AdbError`, and `server.py` catches only `(AdbError, OSError)` — so it escapes the tool handler. `key`/`paste` **do** wrap it. | **FIX.** Wrap runtime errors from the tap and swipe paths the same way `key`/`paste` do, so the tool returns `{"ok": false, "error": "plugin scrcpy stream is not running"}` instead of an unhandled exception. In Go, make the top-level tool wrapper convert *any* error to the `{ok:false,error}` shape — which fixes this class of leak permanently. |
| **BUG-9** | `type_text` `%` escaping | A literal `%` is sent as `%25` and most Android `input text` builds do not decode `%XX` other than `%s`, so `50%` is typed as `50%25`. | **REPLICATE.** The tool is explicitly documented as ASCII-only and short; changing it risks breaking the space handling, which is the escape that actually matters. `phone_paste` is the sanctioned path for anything real. |
| **BUG-10** | duplicate tap points near screen edges | `_clamp_device_point` can collapse two offsets onto the same pixel, so the same point is tapped twice and appears twice in `attempts`. | **REPLICATE.** De-duping would change the `attempts` array shape and could reduce the retry count below what the caller asked for. |
| **BUG-11** | `_poll_for_node` with `timeout_s <= 0` | Zero dumps, immediate `AdbError` reporting `Last tree had 0 nodes`. | **REPLICATE.** Cheap, and a caller passing 0 means "don't wait". |

---

## 17. Go porting checklist (derived, not additional behaviour)

1. `pyRound` = `math.RoundToEven` for every `round()` in §4.6/§4.7; plain truncation for the
   `int()` calls in `_scroll_once`.
2. `pyFloat` JSON type with Python-`repr` formatting (`0.0`, not `0`).
3. Ordered JSON: structs everywhere, `SetEscapeHTML(false)`, `SetIndent("", "  ")`.
4. `shellQuote` = `shlex.quote` with the ASCII safe set `[A-Za-z0-9_@%+=:,./-]`.
5. Rune counts (not byte lengths) for `type_text`/`paste` `length`.
6. Ordered keycode table so the "Unknown key" message matches.
7. `pyReprStringList` for `NodeCriteria.describe()`.
8. Float-to-string with a forced `.0` for the `timeout_s` interpolation in the not-found error.
9. A mutex around `_client.serial` and `_ui_tree_cache` (Go handlers can run concurrently;
   Python's did not).
10. Area-average 72x128 downscale for the change score; per-channel threshold 20; fraction
    threshold 0.035 of 9216.
11. `adb shell` commands are one argv element — never invoke a host shell.
12. Every device-touching method starts with `ensure_device()`; `devices()`, `connect_wifi()`,
    `disconnect_wifi()`, `selected_serial()`, and `backend()` deliberately do **not**.
