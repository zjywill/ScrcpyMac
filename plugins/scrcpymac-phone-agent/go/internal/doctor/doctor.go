// Package doctor reports environment diagnostics, replacing
// server/phone_agent/doctor.py.
//
// The payload is model-visible: it is what phone_doctor returns and what
// `phone-agent doctor` prints. Key order, check order and the exact detail
// strings are all part of that contract.
//
// Deliberate changes from the Python, all flagged in the migration notes:
//
//   - The "python" check becomes "binary" (there is no interpreter any more;
//     what matters now is which architecture the user got).
//   - The "mcp_package" check becomes "plugin_root" (the MCP SDK is statically
//     linked, so "pip install mcp" would be actively misleading guidance;
//     without a root report, an adb/scrcpy-server miss is unattributable).
//   - The top-level "uv_available" key is dropped — uv is a Python installer.
//   - Two Python bugs are not ported: the "\0" sentinel that made the adb
//     `bundled` flag true for everything when PHONE_AGENT_ROOT was "", and the
//     duplicate screen_size check emitted when current_app raised after
//     screen_size had already succeeded.
//
// Everything else — the "plugin-h264-ready" backend string, the em-dashed device
// messages, the summary strings, ok/backend semantics — is preserved verbatim.
package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/paths"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/version"
)

// Check is one diagnostic line. JSON key order is name, ok, detail, then the
// extras in the order they were added.
type Check struct {
	Name   string
	OK     bool
	Detail string
	extras *jsonresult.Obj
}

// With appends an extra key to the check, preserving call order.
func (c Check) With(key string, value any) Check {
	if c.extras == nil {
		c.extras = jsonresult.New()
	}
	c.extras.Set(key, value)
	return c
}

// Obj renders the check as an ordered JSON object.
func (c Check) Obj() *jsonresult.Obj {
	o := jsonresult.Of("name", c.Name, "ok", c.OK, "detail", c.Detail)
	if c.extras != nil {
		for _, k := range c.extras.Keys() {
			v, _ := c.extras.Get(k)
			o.Set(k, v)
		}
	}
	return o
}

// Check names, in emission order.
const (
	CheckPlatform     = "platform"
	CheckBinary       = "binary"
	CheckPluginRoot   = "plugin_root"
	CheckScrcpyServer = "scrcpy_server"
	CheckADB          = "adb"
	CheckADBVersion   = "adb_version"
	CheckDevice       = "device"
	CheckScreenSize   = "screen_size"
	CheckForeground   = "foreground_app"
	CheckRuntimeArch  = "runtime_architecture"
)

// Run performs every diagnostic and returns the payload.
//
// It never returns an error: an internal failure becomes a failing check. That
// matches phone_doctor, which is the only tool in the Python server with no
// try/except around it.
func Run(ctx context.Context) *jsonresult.Obj {
	var checks []Check
	add := func(c Check) { checks = append(checks, c) }

	add(Check{Name: CheckPlatform, OK: true, Detail: version.PlatformDetail()})
	add(Check{Name: CheckBinary, OK: true, Detail: version.Describe()})

	root := paths.Resolve()
	add(Check{
		Name:   CheckPluginRoot,
		OK:     root.Verified,
		Detail: root.Path,
	}.With("derived", root.Derived))

	scrcpyServerReady := false
	if server, err := paths.ScrcpyServer(); err == nil {
		scrcpyServerReady = true
		add(Check{Name: CheckScrcpyServer, OK: true, Detail: server}.
			With("bundled", paths.Bundled(root.Path, server)))
	} else {
		add(Check{Name: CheckScrcpyServer, OK: false, Detail: err.Error()})
	}

	adbPath, err := adb.ResolvePath()
	if err != nil {
		add(Check{Name: CheckADB, OK: false, Detail: err.Error()})
		// Early return: without adb nothing else can be probed. Note the shape
		// difference — no "backend" key here.
		return jsonresult.Of(
			"ok", false,
			"version", version.String(),
			"checks", checkObjs(checks),
			"summary", "adb is required before controlling a phone",
		)
	}
	add(Check{Name: CheckADB, OK: true, Detail: adbPath}.
		With("bundled", paths.Bundled(root.Path, adbPath)))

	// One client for every probe. Python built three, each re-resolving the
	// path; the argv and therefore the output are identical. PHONE_AGENT_SERIAL
	// is picked up here exactly as it was in the Python.
	client := adb.NewWithPath("", adbPath)

	if res, err := client.Run(ctx, "version"); err != nil {
		add(Check{Name: CheckADBVersion, OK: false, Detail: err.Error()})
	} else {
		add(Check{Name: CheckADBVersion, OK: true, Detail: firstLine(strings.TrimSpace(res.Stdout))})
	}

	var devices []adb.Device
	if listed, err := client.ListDevices(ctx); err != nil {
		add(Check{Name: CheckDevice, OK: false, Detail: err.Error()})
	} else {
		devices = listed
		ready := readyDevices(devices)
		switch {
		case len(ready) > 0:
			add(Check{
				Name:   CheckDevice,
				OK:     true,
				Detail: fmt.Sprintf("%d ready device(s)", len(ready)),
			}.With("devices", devices))
		case len(devices) > 0:
			add(Check{
				Name:   CheckDevice,
				OK:     false,
				Detail: "device(s) found but not authorized — accept USB debugging prompt",
			}.With("devices", devices))
		default:
			add(Check{
				Name:   CheckDevice,
				OK:     false,
				Detail: "no devices — connect USB or adb connect",
			}.With("devices", devices))
		}
	}

	if ready := readyDevices(devices); len(ready) == 1 {
		probe := adb.NewWithPath(ready[0].Serial, adbPath)
		width, height, err := probe.ScreenSize(ctx)
		if err != nil {
			add(Check{Name: CheckScreenSize, OK: false, Detail: err.Error()})
		} else {
			add(Check{Name: CheckScreenSize, OK: true, Detail: fmt.Sprintf("%dx%d", width, height)})
			app, appErr := probe.CurrentApp(ctx)
			if appErr != nil {
				// Python emitted a second, duplicate screen_size check here.
				// Report the failing probe instead; foreground_app is excluded
				// from the overall ok, so the meaning is unchanged.
				add(Check{Name: CheckForeground, OK: false, Detail: appErr.Error()}.
					With("activity", ""))
			} else {
				pkg := app.Package
				if pkg == "" {
					pkg = "unknown"
				}
				add(Check{Name: CheckForeground, OK: true, Detail: pkg}.
					With("activity", app.Activity))
			}
		}
	}

	add(Check{
		Name:   CheckRuntimeArch,
		OK:     true,
		Detail: "standalone plugin H.264 runtime; ScrcpyMac.app is not used",
	}.With("backend", "plugin-h264"))

	backend := "adb"
	if scrcpyServerReady {
		backend = "plugin-h264-ready"
	}
	allOK := true
	for _, c := range checks {
		if c.Name == CheckForeground {
			continue
		}
		if !c.OK {
			allOK = false
		}
	}
	summary := "fix failed checks above"
	if allOK {
		summary = "ready"
	}

	return jsonresult.Of(
		"ok", allOK,
		"version", version.String(),
		"backend", backend,
		"checks", checkObjs(checks),
		"summary", summary,
	)
}

func checkObjs(checks []Check) []*jsonresult.Obj {
	out := make([]*jsonresult.Obj, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Obj())
	}
	return out
}

func readyDevices(devices []adb.Device) []adb.Device {
	var ready []adb.Device
	for _, d := range devices {
		if d.State == adb.StateReady {
			ready = append(ready, d)
		}
	}
	return ready
}

func firstLine(s string) string {
	if s == "" {
		return "unknown"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
