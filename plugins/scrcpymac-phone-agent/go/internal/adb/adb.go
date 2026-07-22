// Package adb is the thin adb wrapper the whole plugin runs on. It is a direct
// port of server/phone_agent/adb.py.
//
// Rules that must not drift, because their output reaches the model verbatim:
//
//   - Error messages are byte-identical to the Python AdbError templates. The
//     timeout message contains the FULL argv (adb path + -s serial + args); the
//     non-zero-exit message contains only the args.
//   - Text output is CRLF-normalised, because Python's subprocess text=True
//     enables universal newlines and Android shell output is full of \r\n.
//     Binary output (screenshots) is never touched.
//   - A shell command is ONE argv element. adb joins the post-`shell` argv with
//     spaces and hands it to the device's /system/bin/sh, so pipes, &&, ; and
//     redirection are parsed on the device. Splitting it host-side silently
//     breaks ui_tree_xml and current_app.
//   - The child inherits the environment verbatim. ANDROID_SERIAL is not read by
//     this code but IS honoured by adb, which bypasses the multi-device guard.
//     That is existing behaviour; do not scrub the env to "fix" it.
//
// Layout: this file holds the client, invocation and binary discovery;
// devices.go the `devices -l` parser and the device probes; probes.go the rest
// of the adb.py surface (screencap, uiautomator, push, forward, Wi-Fi);
// quote.go the shlex.quote port every device-side command string must go
// through; runner.go the seam that lets callers in any package test against a
// fake adb.
package adb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/paths"
)

// Timeouts. Python's default is 60s; screenshots and UI dumps use 30s and
// teardown uses 10s.
const (
	DefaultTimeout  = 60 * time.Second
	ShortTimeout    = 30 * time.Second
	TeardownTimeout = 10 * time.Second
)

// Environment variables this package reads.
const (
	PathEnv       = "ADB_PATH"
	AndroidADBEnv = "ANDROID_ADB"
	SerialEnv     = "PHONE_AGENT_SERIAL"
)

// NotFoundMessage is raised when no adb binary can be found. Byte-identical to
// the Python.
const NotFoundMessage = "adb not found. Install Android platform-tools, run scripts/install.sh, or set ADB_PATH."

// Error is the Go equivalent of Python's AdbError. server.py catches AdbError
// (and OSError) and turns it into {"ok": false, "error": <message>}, so every
// message here is model-visible.
type Error struct {
	Msg string
}

func (e *Error) Error() string { return e.Msg }

func errorf(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// IsError reports whether err is (or wraps) an adb Error.
func IsError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// Result is one completed adb invocation.
type Result struct {
	// Args is the argument list as passed by the caller, without the adb path
	// or the -s serial prefix. Error messages quote this, not the full argv.
	Args []string
	// Stdout and Stderr are CRLF-normalised text.
	Stdout string
	Stderr string
	// Raw is the undecoded stdout, for binary callers such as screencap.
	Raw []byte
	// ExitCode is the process exit status, or -1 if it never ran.
	ExitCode int
}

// Client invokes adb, optionally pinned to a serial.
//
// The serial is mutable — select_device() and ensure_device() write it — and the
// MCP server shares one client across concurrent tool calls, so every access is
// mutex-guarded.
type Client struct {
	mu     sync.Mutex
	path   string
	serial string
	runner Runner
}

// New resolves the adb binary and returns a client for the given serial. An
// empty serial falls back to PHONE_AGENT_SERIAL, then to none.
func New(serial string) (*Client, error) {
	path, err := ResolvePath()
	if err != nil {
		return nil, err
	}
	return NewWithPath(serial, path), nil
}

// NewWithPath returns a client using an already-resolved adb path.
func NewWithPath(serial, path string) *Client {
	if serial == "" {
		serial = os.Getenv(SerialEnv)
	}
	return &Client{path: path, serial: serial}
}

// NewWithRunner returns a client whose invocations go through r instead of
// exec. Tests in any package can use it to drive the whole action layer without
// an adb binary and without a device; see Runner.
func NewWithRunner(serial, path string, r Runner) *Client {
	client := NewWithPath(serial, path)
	client.SetRunner(r)
	return client
}

// SetRunner replaces the invocation backend. Passing nil restores ExecRunner.
func (c *Client) SetRunner(r Runner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runner = r
}

func (c *Client) currentRunner() Runner {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runner == nil {
		return execRunner{}
	}
	return c.runner
}

// Path returns the adb binary path. The scrcpy runtime needs it to build its own
// long-lived `adb shell app_process` command, which is streamed rather than
// captured and therefore cannot go through Run.
func (c *Client) Path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

// Serial returns the currently selected serial, possibly "".
func (c *Client) Serial() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serial
}

// SetSerial pins the client to a device.
func (c *Client) SetSerial(serial string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serial = serial
}

// Argv returns the full command line for args, i.e. [adb, -s, serial, args...].
// Exported so the scrcpy runtime can spawn its own process with identical
// targeting.
func (c *Client) Argv(args ...string) []string {
	c.mu.Lock()
	path, serial := c.path, c.serial
	c.mu.Unlock()

	argv := make([]string, 0, len(args)+3)
	argv = append(argv, path)
	if serial != "" {
		argv = append(argv, "-s", serial)
	}
	return append(argv, args...)
}

// Run invokes adb with the default 60s timeout and fails on a non-zero exit.
func (c *Client) Run(ctx context.Context, args ...string) (Result, error) {
	return c.RunTimeout(ctx, DefaultTimeout, args...)
}

// RunTimeout invokes adb with an explicit timeout and fails on a non-zero exit.
func (c *Client) RunTimeout(ctx context.Context, timeout time.Duration, args ...string) (Result, error) {
	res, err := c.RunNoCheck(ctx, timeout, args...)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, errorf("adb %s failed: %s", strings.Join(args, " "), res.detail())
	}
	return res, nil
}

// detail is Python's `stderr.strip() or stdout.strip() or f"exit {rc}"`, with
// _as_text's UTF-8 replacement applied: run_bytes captures undecoded bytes, and
// a screencap failure must still produce a printable message. Python replaces
// each invalid byte and ToValidUTF8 each invalid run, so the number of U+FFFD
// can differ — only ever inside output that was already garbage.
func (r Result) detail() string {
	detail := strings.TrimSpace(r.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(r.Stdout)
	}
	if detail == "" {
		return fmt.Sprintf("exit %d", r.ExitCode)
	}
	return strings.ToValidUTF8(detail, "�")
}

// RunNoCheck invokes adb and returns the result even for a non-zero exit. Only
// process-level failures (timeout, unusable binary) produce an error.
//
// Python never uses check=False, but teardown paths want it: `forward --remove`
// for a port another process already released is expected and benign.
func (c *Client) RunNoCheck(ctx context.Context, timeout time.Duration, args ...string) (Result, error) {
	argv := c.Argv(args...)

	out, runErr := c.currentRunner().RunADB(ctx, argv, timeout)

	res := Result{
		Args:     args,
		Raw:      out.Stdout,
		Stdout:   normalizeNewlines(string(out.Stdout)),
		Stderr:   normalizeNewlines(string(out.Stderr)),
		ExitCode: out.ExitCode,
	}

	switch {
	case out.TimedOut:
		// Python: f"adb timed out: {' '.join(cmd)}" — the FULL argv, adb path
		// and -s serial included. Deliberately different from the failure
		// message below; do not unify them.
		return res, errorf("adb timed out: %s", strings.Join(argv, " "))
	case out.Cancelled:
		// The server is shutting down; make that legible rather than reporting
		// it as an adb failure.
		return res, errorf("adb cancelled: %s", strings.Join(argv, " "))
	case runErr != nil:
		// Could not start it at all: missing, not executable, wrong
		// architecture. Python surfaces the OSError text here; server.py
		// catches OSError too.
		return res, errorf("adb %s failed: %v", strings.Join(args, " "), runErr)
	}
	return res, nil
}

// RunBytes returns raw stdout with no newline translation. Mandatory for
// `exec-out screencap -p`: CRLF translation corrupts a PNG.
func (c *Client) RunBytes(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	res, err := c.RunTimeout(ctx, timeout, args...)
	if err != nil {
		return nil, err
	}
	return res.Raw, nil
}

// Shell runs one device-side shell command with the default timeout and returns
// stripped stdout. command is passed as a single argv element; see the package
// comment.
func (c *Client) Shell(ctx context.Context, command string) (string, error) {
	return c.ShellTimeout(ctx, DefaultTimeout, command)
}

// ShellTimeout is Shell with an explicit timeout.
func (c *Client) ShellTimeout(ctx context.Context, timeout time.Duration, command string) (string, error) {
	res, err := c.RunTimeout(ctx, timeout, "shell", command)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// ResolvePath finds the adb binary.
//
// Precedence:
//
//  1. $ADB_PATH, else $ANDROID_ADB — accepted only if it is an executable file.
//     A bad value is silently ignored, exactly like the Python, and resolution
//     continues.
//  2. the plugin-bundled adb (see paths.BundledADB)
//  3. adb on PATH
//  4. ~/Library/Android/sdk/platform-tools/adb, /opt/homebrew/bin/adb,
//     /usr/local/bin/adb, /usr/bin/adb
//  5. Error(NotFoundMessage)
//
// Resolution is cheap (a handful of stats) and is deliberately not cached here:
// a user who installs platform-tools mid-session gets a working server without
// restarting, which is how the Python behaved.
func ResolvePath() (string, error) {
	env := os.Getenv(PathEnv)
	if env == "" {
		env = os.Getenv(AndroidADBEnv)
	}
	if env != "" && paths.IsExecutableFile(env) {
		return env, nil
	}

	if bundled := paths.BundledADB(paths.Path()); bundled != "" {
		return bundled, nil
	}

	if found, err := exec.LookPath("adb"); err == nil {
		return found, nil
	}

	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, "Library", "Android", "sdk", "platform-tools", "adb"))
	}
	candidates = append(candidates,
		"/opt/homebrew/bin/adb",
		"/usr/local/bin/adb",
		"/usr/bin/adb",
	)
	for _, candidate := range candidates {
		if paths.IsExecutableFile(candidate) {
			return candidate, nil
		}
	}
	return "", &Error{Msg: NotFoundMessage}
}

// normalizeNewlines applies Python's universal-newline translation: \r\n and a
// lone \r both become \n.
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
