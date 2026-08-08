// Package paths resolves everything the plugin needs to find on disk: the
// plugin root, the bundled adb binary and the scrcpy-server jar.
//
// Two rules that look like details but are load-bearing:
//
//   - The plugin root is NOT symlink-resolved. scripts/configure.sh symlinks the
//     plugin into ~/.cursor/plugins/local and bash's `cd` is logical, so the
//     Python launcher exports a symlinked PHONE_AGENT_ROOT. doctor's "bundled"
//     flags are prefix comparisons against that same string, so calling
//     filepath.EvalSymlinks would make a symlinked install report bundled:false
//     for genuinely bundled files.
//   - Neither adb nor scrcpy-server may be //go:embed-ed. `adb push` and exec
//     need real filesystem paths; embedding would force a temp-file extraction
//     on every stream start. They ship adjacent to the binary.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Environment variables this package reads.
const (
	RootEnv         = "PHONE_AGENT_ROOT"
	ScrcpyServerEnv = "SCRCPY_SERVER_PATH"
)

// ScrcpyVersion is passed as the first app_process argument to scrcpy-server and
// must match the bundled jar exactly, or scrcpy-server aborts with a version
// mismatch. Bump it only together with share/scrcpy-server and the SHA-256 in
// THIRD_PARTY_NOTICES.md.
const ScrcpyVersion = "3.3.4"

// ErrScrcpyServerMissing carries the exact message the Python raised, which
// reaches the model verbatim through {"ok":false,"error":...}.
var ErrScrcpyServerMissing = errors.New(
	"scrcpy-server is missing from the plugin. Reinstall the plugin or set SCRCPY_SERVER_PATH.")

// Root describes the resolved plugin root.
type Root struct {
	// Path is the plugin root directory, never symlink-resolved.
	Path string
	// Derived is true when the root was computed from os.Executable() because
	// PHONE_AGENT_ROOT was unset. The doctor "plugin_root" check reports it.
	Derived bool
	// Verified is true when Path/mcp.json exists, i.e. the root really looks
	// like a plugin install.
	Verified bool
}

var (
	rootOnce sync.Once
	rootVal  Root
)

// Resolve returns the plugin root, computing it once per process.
//
// PHONE_AGENT_ROOT wins verbatim when set (the bash launcher exports it).
// Otherwise the root is derived from the executable location: the binary ships
// at <root>/bin/<os>/<arch>/phone-agent, so three levels up is the usual answer;
// two and one level up are tried as fallbacks so a binary dropped at
// <root>/bin/<os>/phone-agent or <root>/bin/phone-agent still works. The first
// candidate containing mcp.json wins; if none does, three levels up is used and
// Verified stays false so the doctor can say so instead of aborting.
func Resolve() Root {
	rootOnce.Do(func() { rootVal = resolveRoot() })
	return rootVal
}

// Path is shorthand for Resolve().Path.
func Path() string { return Resolve().Path }

// Export writes the resolved root back into the environment so child processes
// — the long-lived `adb shell app_process` that runs scrcpy-server, and any
// helper script — see the same value the bash launcher would have exported.
func Export() {
	_ = os.Setenv(RootEnv, Path())
}

func resolveRoot() Root {
	if v := os.Getenv(RootEnv); v != "" {
		return Root{Path: v, Derived: false, Verified: IsFile(filepath.Join(v, "mcp.json"))}
	}

	exe, err := os.Executable()
	if err != nil {
		wd, wdErr := os.Getwd()
		if wdErr != nil {
			wd = "."
		}
		return Root{Path: wd, Derived: true, Verified: IsFile(filepath.Join(wd, "mcp.json"))}
	}
	dir := filepath.Dir(exe)

	// <root>/bin/<os>/<arch>/phone-agent, then the shallower layouts.
	fallback := filepath.Clean(filepath.Join(dir, "..", "..", ".."))
	for _, up := range [][]string{
		{"..", "..", ".."},
		{"..", ".."},
		{".."},
	} {
		candidate := filepath.Clean(filepath.Join(append([]string{dir}, up...)...))
		if IsFile(filepath.Join(candidate, "mcp.json")) {
			return Root{Path: candidate, Derived: true, Verified: true}
		}
	}
	return Root{Path: fallback, Derived: true, Verified: false}
}

// BundledADB returns the plugin-bundled adb for this platform, or "" when there
// is none.
//
// Order differs deliberately from the bash launcher: the target package layout
// ships a single universal bin/darwin/adb and no arch mirrors, so the universal
// path is tried first. scripts/download-adb.sh writes byte-identical copies to
// both locations, so on today's tree the two orders are indistinguishable.
func BundledADB(root string) string {
	for _, candidate := range BundledADBCandidates(root) {
		if IsExecutableFile(candidate) {
			return candidate
		}
	}
	return ""
}

// BundledADBCandidates lists the bundled adb locations for this platform, in
// preference order. It returns nil for a platform the plugin does not bundle
// adb for (matching Python: darwin any arch, linux x86_64 only).
func BundledADBCandidates(root string) []string {
	if root == "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{
			filepath.Join(root, "bin", "darwin", "adb"),
			filepath.Join(root, "bin", "darwin", archDir(), "adb"),
		}
	case "linux":
		if runtime.GOARCH != "amd64" {
			return nil
		}
		return []string{
			filepath.Join(root, "bin", "linux", "adb"),
			filepath.Join(root, "bin", "linux", "x86_64", "adb"),
		}
	default:
		return nil
	}
}

// archDir is the directory name scripts/download-adb.sh and scripts/build-go.sh
// use: arm64 or x86_64. Anything unexpected falls into the x86_64 bucket, which
// is what the Python did.
func archDir() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x86_64"
}

// ScrcpyServer resolves the scrcpy-server jar, or returns ErrScrcpyServerMissing.
//
// Executability is deliberately not checked: it is a jar that gets `adb push`ed
// to the device, not something we exec.
func ScrcpyServer() (string, error) {
	for _, candidate := range ScrcpyServerCandidates(Path()) {
		if IsFile(candidate) {
			return candidate, nil
		}
	}
	return "", ErrScrcpyServerMissing
}

// ScrcpyServerCandidates lists jar locations in preference order.
//
// Entries 3 and 4 are the pre-migration locations; keeping them means a Go
// binary dropped into today's tree finds the jar before the files move to
// share/.
func ScrcpyServerCandidates(root string) []string {
	var candidates []string
	if requested := os.Getenv(ScrcpyServerEnv); requested != "" {
		candidates = append(candidates, ExpandUser(requested))
	}
	if root != "" {
		candidates = append(candidates,
			filepath.Join(root, "share", "scrcpy-server"),                  // target layout
			filepath.Join(root, "bin", "scrcpy-server"),                    // legacy
			filepath.Join(root, "bin", "darwin", "share", "scrcpy-server"), // legacy, current
		)
	}
	return append(candidates,
		"/opt/homebrew/share/scrcpy/scrcpy-server",
		"/usr/local/share/scrcpy/scrcpy-server",
	)
}

// Bundled reports whether p lives inside the plugin root, which is what the
// doctor's "bundled" extra means. It is a prefix test on the un-resolved root
// string, exactly like the Python.
func Bundled(root, p string) bool {
	return root != "" && strings.HasPrefix(p, root)
}

// ExpandUser expands a leading ~ or ~/ to $HOME, mirroring Python's
// Path.expanduser(). A ~user form is left untouched — the Python would expand
// it, but nothing in the plugin uses one.
func ExpandUser(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// IsFile reports whether p exists and is a regular file (following symlinks).
func IsFile(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// IsExecutableFile reports whether p is a regular file with an execute bit set.
// It approximates os.access(p, X_OK) closely enough for binary discovery and
// needs no syscall package.
func IsExecutableFile(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
