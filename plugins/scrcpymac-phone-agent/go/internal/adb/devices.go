package adb

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// This file holds the device-facing half of the adb wrapper: `devices -l`
// parsing plus the probes the CLI and the doctor need. The rest of the adb.py
// surface — screenshot_png, ui_tree_xml, push, forward, connect_wifi,
// disconnect_wifi, enable_tcpip, device_wifi_ip — lives in probes.go. Call
// those; do not redeclare them in the action layer.

// Device is one entry of `adb devices -l`.
//
// The JSON key order — serial, state, model, product — is the contract:
// Device.to_dict() emitted exactly these four keys and nothing else. The two
// extra tokens adb prints are parsed but deliberately NOT serialised, so
// phone_list_devices, `phone-agent devices` and the doctor payload stay
// byte-identical to the Python while the runtime still has access to them.
type Device struct {
	Serial  string `json:"serial"`
	State   string `json:"state"`
	Model   string `json:"model"`
	Product string `json:"product"`

	// Codename is the `device:` token, i.e. ro.product.device. Never serialised.
	Codename string `json:"-"`
	// TransportID is the `transport_id:` token: an adb-server-local handle that
	// changes on every reconnect. Never serialised.
	TransportID string `json:"-"`
}

// StateReady is the only state that counts as usable.
const StateReady = "device"

const devicesHeader = "List of devices attached"

// ParseDevices parses `adb devices -l` output.
//
// Three documented deviations from the Python, all flagged in
// docs/spec-adb-doctor.md §1.6 and none of which changes a JSON key:
//
//   - The header is located rather than assumed at index 0. `* daemon not
//     running; starting now at tcp:5037 *` goes to stderr on adb 1.0.41, but it
//     has not always, and it would otherwise parse as Device{serial:"*",
//     state:"daemon"}. Lines starting with `*` are skipped for the same reason.
//     With no header at all, line 0 is dropped exactly like the Python.
//   - The `no permissions` state is re-joined into one string instead of being
//     truncated to `no`. adb's own non-`-l` output renders it that way, and
//     every consumer only ever tests state == "device".
//   - `device:` and `transport_id:` are captured into unserialised fields.
//
// Transport order is preserved: it decides which serial EnsureDevice
// auto-selects and the join order of the "Multiple devices" message.
func ParseDevices(stdout string) []Device {
	devices := []Device{}
	lines := strings.Split(stdout, "\n")

	start := 1 // Python's blind splitlines()[1:].
	for i, line := range lines {
		if strings.TrimSpace(line) == devicesHeader {
			start = i + 1
			break
		}
	}
	// start is at most len(lines), so the slice below is always in bounds.
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		device := Device{Serial: parts[0], State: parts[1]}
		rest := parts[2:]
		// "no permissions", optionally followed by "(user in plugdev group…)"
		// and "; see [http://…]" — one state spelled across several fields.
		if device.State == "no" && len(rest) > 0 && strings.HasPrefix(rest[0], "permissions") {
			device.State = "no permissions"
		}
		for _, token := range rest {
			key, value, ok := strings.Cut(token, ":")
			if !ok {
				continue
			}
			switch key {
			case "model":
				device.Model = value
			case "product":
				device.Product = value
			case "device":
				device.Codename = value
			case "transport_id":
				device.TransportID = value
			}
		}
		devices = append(devices, device)
	}
	return devices
}

// ListDevices runs `adb devices -l`. Transport order is preserved: it decides
// which serial EnsureDevice auto-selects and the join order of the
// "Multiple devices" message.
func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	res, err := c.Run(ctx, "devices", "-l")
	if err != nil {
		return nil, err
	}
	return ParseDevices(res.Stdout), nil
}

// EnsureDevice returns the selected serial, choosing one when exactly one device
// is ready.
//
// The short-circuit matters: once a serial is set, EnsureDevice performs no adb
// call at all and does not validate it. That is on the hot path of every action.
func (c *Client) EnsureDevice(ctx context.Context) (string, error) {
	if serial := c.Serial(); serial != "" {
		return serial, nil
	}
	devices, err := c.ListDevices(ctx)
	if err != nil {
		return "", err
	}
	var ready []Device
	for _, d := range devices {
		if d.State == StateReady {
			ready = append(ready, d)
		}
	}
	switch len(ready) {
	case 0:
		return "", &Error{Msg: "No Android device connected. Plug in USB or run adb connect."}
	case 1:
		c.SetSerial(ready[0].Serial)
		return ready[0].Serial, nil
	default:
		serials := make([]string, 0, len(ready))
		for _, d := range ready {
			serials = append(serials, d.Serial)
		}
		return "", errorf("Multiple devices connected (%s). Set PHONE_AGENT_SERIAL or pass serial.",
			strings.Join(serials, ", "))
	}
}

var (
	screenSizeRe = regexp.MustCompile(`(\d+)x(\d+)`)
	currentAppRe = regexp.MustCompile(`([a-zA-Z0-9_.]+)/([a-zA-Z0-9_.$]+)`)
)

// ScreenSize returns the device resolution from `wm size`.
//
// It takes the FIRST match, so with a display override active it reports the
// physical size rather than the effective one. That is arguably wrong, but every
// tap coordinate is clamped against this value, so changing it would silently
// shift every tap. Do not "fix" it here.
func (c *Client) ScreenSize(ctx context.Context) (int, int, error) {
	output, err := c.Shell(ctx, "wm size")
	if err != nil {
		return 0, 0, err
	}
	match := screenSizeRe.FindStringSubmatch(output)
	if match == nil {
		return 0, 0, errorf("Could not parse screen size from: %s", pyRepr(output))
	}
	width, werr := strconv.Atoi(match[1])
	height, herr := strconv.Atoi(match[2])
	if werr != nil || herr != nil {
		return 0, 0, errorf("Could not parse screen size from: %s", pyRepr(output))
	}
	return width, height, nil
}

// AppInfo is the foreground app, as reported by dumpsys.
type AppInfo struct {
	Package  string
	Activity string
	Raw      string
}

// CurrentApp returns the foreground package/activity. It never fails on a
// no-match: both fields come back empty with Raw carrying the full output.
func (c *Client) CurrentApp(ctx context.Context) (AppInfo, error) {
	output, err := c.Shell(ctx, "dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -1")
	if err != nil {
		return AppInfo{}, err
	}
	info := AppInfo{Raw: output}
	if match := currentAppRe.FindStringSubmatch(output); match != nil {
		info.Package = match[1]
		info.Activity = match[2]
	}
	return info, nil
}

// pyRepr renders a string the way Python's {!r} does for the screen-size error,
// which the model sees verbatim: single quotes unless the value contains one.
func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + escapeRepr(s, '"') + `"`
	}
	return "'" + escapeRepr(s, '\'') + "'"
}

func escapeRepr(s string, quote byte) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
