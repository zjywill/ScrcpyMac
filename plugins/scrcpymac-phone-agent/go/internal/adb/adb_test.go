package adb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `devices -l` parsing is covered exhaustively in devices_test.go.

func TestArgvIncludesSerial(t *testing.T) {
	c := NewWithPath("2f019965", "/usr/bin/adb")
	got := strings.Join(c.Argv("shell", "wm size"), "|")
	if got != "/usr/bin/adb|-s|2f019965|shell|wm size" {
		t.Errorf("Argv = %q", got)
	}

	c.SetSerial("")
	got = strings.Join(c.Argv("devices", "-l"), "|")
	if got != "/usr/bin/adb|devices|-l" {
		t.Errorf("Argv without serial = %q", got)
	}
}

func TestNormalizeNewlines(t *testing.T) {
	if got := normalizeNewlines("a\r\nb\rc\nd"); got != "a\nb\nc\nd" {
		t.Errorf("normalizeNewlines = %q", got)
	}
}

// newFakeADB writes a shell script standing in for adb and returns a client for
// it. The scrcpy investigation may be using the real device, so behaviour tests
// never touch it.
func newFakeADB(t *testing.T, body string) *Client {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "adb")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewWithPath("", path)
}

func TestRunFailureMessageQuotesArgsOnly(t *testing.T) {
	client := newFakeADB(t, "echo 'device offline' >&2\nexit 1\n")
	client.SetSerial("SERIAL")

	_, err := client.Run(context.Background(), "shell", "wm size")
	if err == nil {
		t.Fatal("want an error on a non-zero exit")
	}
	// Python: f"adb {' '.join(args)} failed: {detail}" — args only, no adb path
	// and no -s serial, with stderr preferred over stdout.
	if got := err.Error(); got != "adb shell wm size failed: device offline" {
		t.Errorf("error = %q", got)
	}
	if !IsError(err) {
		t.Error("failure must be an adb.Error so server.py's AdbError branch matches")
	}
}

func TestRunFailureFallsBackToStdoutThenExitCode(t *testing.T) {
	client := newFakeADB(t, "echo 'on stdout'\nexit 3\n")
	_, err := client.Run(context.Background(), "connect", "1.2.3.4")
	if err == nil || err.Error() != "adb connect 1.2.3.4 failed: on stdout" {
		t.Errorf("stdout fallback: %v", err)
	}

	client = newFakeADB(t, "exit 7\n")
	_, err = client.Run(context.Background(), "boom")
	if err == nil || err.Error() != "adb boom failed: exit 7" {
		t.Errorf("exit-code fallback: %v", err)
	}
}

func TestTimeoutMessageQuotesFullArgv(t *testing.T) {
	// `exec` so the sleeping process IS the direct child: CommandContext kills
	// what it started, and a grandchild holding the stdout pipe would otherwise
	// stall Wait until WaitDelay expires.
	client := newFakeADB(t, "exec sleep 5\n")
	client.SetSerial("SERIAL")

	_, err := client.RunTimeout(context.Background(), 150*time.Millisecond, "shell", "sleep")
	if err == nil {
		t.Fatal("want a timeout error")
	}
	// Python: f"adb timed out: {' '.join(cmd)}" — the FULL argv this time.
	if !strings.HasPrefix(err.Error(), "adb timed out: ") || !strings.HasSuffix(err.Error(), " -s SERIAL shell sleep") {
		t.Errorf("timeout error = %q", err.Error())
	}
}

func TestRunNoCheckReturnsNonZeroWithoutError(t *testing.T) {
	client := newFakeADB(t, "exit 1\n")
	res, err := client.RunNoCheck(context.Background(), DefaultTimeout, "forward", "--remove", "tcp:1")
	if err != nil {
		t.Fatalf("RunNoCheck must not fail on a non-zero exit: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

func TestShellPassesOneArgvElement(t *testing.T) {
	// If the command were split host-side, $# would be greater than 2.
	client := newFakeADB(t, "echo \"argc=$#\"\n")
	out, err := client.Shell(context.Background(), "dumpsys window | grep -E 'mCurrentFocus' | head -1")
	if err != nil {
		t.Fatal(err)
	}
	if out != "argc=2" {
		t.Errorf("shell argv = %q, want argc=2 (shell + one command element)", out)
	}
}

func TestRunBytesIsNotNewlineTranslated(t *testing.T) {
	client := newFakeADB(t, "printf 'PNG\\r\\n\\x01'\n")
	raw, err := client.RunBytes(context.Background(), ShortTimeout, "exec-out", "screencap", "-p")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "PNG\r\n\x01" {
		t.Errorf("RunBytes = %q, want the bytes untouched (CRLF translation corrupts a PNG)", raw)
	}
}

func TestEnsureDeviceShortCircuitsOnAKnownSerial(t *testing.T) {
	// The fake would fail if it were invoked; a preset serial must not call adb.
	client := newFakeADB(t, "exit 1\n")
	client.SetSerial("already-selected")
	serial, err := client.EnsureDevice(context.Background())
	if err != nil || serial != "already-selected" {
		t.Fatalf("EnsureDevice = %q, %v", serial, err)
	}
}

func TestEnsureDeviceMessages(t *testing.T) {
	none := newFakeADB(t, "echo 'List of devices attached'\n")
	if _, err := none.EnsureDevice(context.Background()); err == nil ||
		err.Error() != "No Android device connected. Plug in USB or run adb connect." {
		t.Errorf("no devices: %v", err)
	}

	many := newFakeADB(t, "printf 'List of devices attached\\nAAA\\tdevice\\nBBB\\tdevice\\n'\n")
	if _, err := many.EnsureDevice(context.Background()); err == nil ||
		err.Error() != "Multiple devices connected (AAA, BBB). Set PHONE_AGENT_SERIAL or pass serial." {
		t.Errorf("multiple devices: %v", err)
	}
}

func TestScreenSizeTakesTheFirstMatch(t *testing.T) {
	// With an override active the physical size must win — every tap coordinate
	// is clamped against it.
	client := newFakeADB(t, "printf 'Physical size: 1080x2280\\r\\nOverride size: 720x1520\\r\\n'\n")
	w, h, err := client.ScreenSize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w != 1080 || h != 2280 {
		t.Errorf("ScreenSize = %dx%d, want 1080x2280", w, h)
	}
}

func TestScreenSizeParseFailureMessage(t *testing.T) {
	client := newFakeADB(t, "echo 'no size here'\n")
	_, _, err := client.ScreenSize(context.Background())
	if err == nil || err.Error() != "Could not parse screen size from: 'no size here'" {
		t.Errorf("error = %v", err)
	}
}

func TestCurrentAppNeverFailsOnNoMatch(t *testing.T) {
	client := newFakeADB(t, "echo 'nothing useful'\n")
	info, err := client.CurrentApp(context.Background())
	if err != nil {
		t.Fatalf("CurrentApp must not fail on a no-match: %v", err)
	}
	if info.Package != "" || info.Activity != "" || info.Raw != "nothing useful" {
		t.Errorf("CurrentApp = %#v", info)
	}
}

func TestResolvePathPrefersEnv(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "adb")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PathEnv, fake)
	got, err := ResolvePath()
	if err != nil || got != fake {
		t.Fatalf("ResolvePath = %q, %v; want %q", got, err, fake)
	}

	// A bad ADB_PATH is silently ignored and resolution continues, rather than
	// failing outright.
	t.Setenv(PathEnv, filepath.Join(dir, "does-not-exist"))
	if got, err := ResolvePath(); err == nil && got == filepath.Join(dir, "does-not-exist") {
		t.Error("a non-existent ADB_PATH must be ignored, not returned")
	}
}
