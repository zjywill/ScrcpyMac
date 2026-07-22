// Package version carries the plugin version and build stamp.
//
// The version string is part of the plugin's packaging contract:
// server/tests/test_packaging.py asserts that .agents/plugins/marketplace.json
// (twice), .codex-plugin/plugin.json, .cursor-plugin/plugin.json,
// server/pyproject.toml, ui/package.json and server/phone_agent/__init__.py all
// agree. This constant joins that set — bump it together with the others.
package version

import (
	"fmt"
	"runtime"
	"strings"
)

// Version is the plugin version. scripts/build-go.sh overrides it at link time
// via -X main.version=<value> read out of .codex-plugin/plugin.json; main then
// calls Set. The literal here is the fallback for `go build ./...` and `go test`.
var Version = "0.7.2"

// Commit is the short git commit the binary was built from, or "" when unknown.
// Set at link time via -X main.commit=<value>.
var Commit = ""

// Set installs link-time build information. Empty values are ignored so a plain
// `go build` keeps the compiled-in defaults.
func Set(v, commit string) {
	if v != "" {
		Version = v
	}
	if commit != "" {
		Commit = commit
	}
}

// String returns the bare version, e.g. "0.7.2".
func String() string { return Version }

// CLI is what `phone-agent version` prints, matching the Python launcher's
// "scrcpymac-phone-agent 0.7.2".
func CLI() string { return "scrcpymac-phone-agent " + Version }

// Describe is the doctor "binary" check detail, e.g.
// "phone-agent 0.7.2 darwin/arm64 (go1.25.7)". A binary running under Rosetta 2
// gets a trailing note because it works but is measurably slower.
func Describe() string {
	s := fmt.Sprintf("phone-agent %s %s/%s (%s)", Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	if Commit != "" {
		s += " " + Commit
	}
	if Translated() {
		s += " (running under Rosetta 2)"
	}
	return s
}

// PlatformDetail is the doctor "platform" check detail. It must stay in uname
// vocabulary — "Darwin arm64", not "darwin arm64" and not "darwin/arm64" —
// because that is what Python's platform.system()/platform.machine() produced.
func PlatformDetail() string {
	return UnameSystem() + " " + UnameMachine()
}

// UnameSystem renders runtime.GOOS the way `uname -s` does.
func UnameSystem() string {
	switch runtime.GOOS {
	case "darwin":
		return "Darwin"
	case "linux":
		return "Linux"
	default:
		if runtime.GOOS == "" {
			return "Unknown"
		}
		return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
	}
}

// UnameMachine renders runtime.GOARCH the way `uname -m` does on macOS: amd64
// is reported as x86_64, which is also what Python's platform.machine() returns.
func UnameMachine() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "386":
		return "i386"
	default:
		return runtime.GOARCH
	}
}
