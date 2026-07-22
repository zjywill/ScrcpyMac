package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScrcpyServerCandidateOrder(t *testing.T) {
	t.Setenv(ScrcpyServerEnv, "")
	got := ScrcpyServerCandidates("/plugin")
	want := []string{
		"/plugin/share/scrcpy-server",
		"/plugin/bin/scrcpy-server",
		"/plugin/bin/darwin/share/scrcpy-server",
		"/opt/homebrew/share/scrcpy/scrcpy-server",
		"/usr/local/share/scrcpy/scrcpy-server",
	}
	assertEqualSlices(t, got, want)

	// SCRCPY_SERVER_PATH wins, and is the only candidate that gets ~ expansion.
	t.Setenv(ScrcpyServerEnv, "~/custom/scrcpy-server")
	got = ScrcpyServerCandidates("/plugin")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got[0] != filepath.Join(home, "custom", "scrcpy-server") {
		t.Errorf("candidate[0] = %q, want the expanded home path", got[0])
	}
}

func TestScrcpyServerFindsTheNewShareLayout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(root, "share", "scrcpy-server")
	if err := os.WriteFile(jar, []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ScrcpyServerEnv, "")
	t.Setenv(RootEnv, root)

	// Resolve() is cached per process, so exercise the candidate list directly.
	for _, candidate := range ScrcpyServerCandidates(root) {
		if IsFile(candidate) {
			if candidate != jar {
				t.Errorf("resolved %q, want %q", candidate, jar)
			}
			return
		}
	}
	t.Fatalf("share/scrcpy-server was not found among %v", ScrcpyServerCandidates(root))
}

func TestBundledADBPrefersTheUniversalBinary(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no bundled adb layout for this platform")
	}
	candidates := BundledADBCandidates("/plugin")
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v", candidates)
	}
	if !strings.HasSuffix(candidates[0], filepath.Join("bin", runtime.GOOS, "adb")) {
		t.Errorf("candidate[0] = %q, want the universal bin/<os>/adb", candidates[0])
	}
	if !strings.Contains(candidates[1], archDir()) {
		t.Errorf("candidate[1] = %q, want the arch mirror", candidates[1])
	}
}

func TestBundledADBNeedsAnExecutableFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bin", runtime.GOOS)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "adb")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BundledADB(root); got != "" {
		t.Errorf("a non-executable adb must be skipped, got %q", got)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := BundledADB(root); got != path {
		t.Errorf("BundledADB = %q, want %q", got, path)
	}
}

func TestBundledIsAPrefixTest(t *testing.T) {
	if !Bundled("/plugin", "/plugin/bin/darwin/adb") {
		t.Error("a path under the root is bundled")
	}
	if Bundled("/plugin", "/usr/local/bin/adb") {
		t.Error("a system path is not bundled")
	}
	// The Python used "\0" as the default root, which made everything bundled.
	if Bundled("", "/anything") {
		t.Error("an empty root must never report bundled")
	}
}

func TestExpandUser(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := ExpandUser("~"); got != home {
		t.Errorf("ExpandUser(~) = %q", got)
	}
	if got := ExpandUser("~/x"); got != filepath.Join(home, "x") {
		t.Errorf("ExpandUser(~/x) = %q", got)
	}
	if got := ExpandUser("/abs/path"); got != "/abs/path" {
		t.Errorf("ExpandUser(/abs/path) = %q", got)
	}
	if got := ExpandUser(""); got != "" {
		t.Errorf("ExpandUser(empty) = %q", got)
	}
}

func assertEqualSlices(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
