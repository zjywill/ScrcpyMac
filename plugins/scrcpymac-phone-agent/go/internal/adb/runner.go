package adb

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Runner performs one adb invocation. Splitting it out of Client is what lets
// every layer above this package be unit-tested without an adb binary and
// without a device: swap in a fake with NewWithRunner or Client.SetRunner and
// assert on the argv you receive.
//
// A Runner reports *process-level* outcomes only. Turning an exit status into an
// AdbError, normalising CRLF and formatting the model-visible message stay in
// Client, so a fake runner cannot accidentally change the wire contract.
type Runner interface {
	// RunADB executes argv (argv[0] is the adb binary) with stdout and stderr
	// captured, honouring timeout when it is positive. It returns an error only
	// when the process could not be run at all; a non-zero exit, a timeout and a
	// cancellation are all reported through Output.
	RunADB(ctx context.Context, argv []string, timeout time.Duration) (Output, error)
}

// Output is one raw invocation result, before any decoding or message building.
type Output struct {
	// Stdout and Stderr are exactly the bytes the process wrote — no newline
	// translation, because RunBytes callers (screencap) need them untouched.
	Stdout []byte
	Stderr []byte
	// ExitCode is the process exit status, or -1 when it never ran or was
	// killed by a signal.
	ExitCode int
	// TimedOut is set when the invocation's own timeout killed the process.
	TimedOut bool
	// Cancelled is set when the caller's context ended the process, i.e. the
	// server is shutting down.
	Cancelled bool
}

// ExecRunner returns the production Runner: exec.CommandContext with both pipes
// captured into buffers.
func ExecRunner() Runner { return execRunner{} }

type execRunner struct{}

func (execRunner) RunADB(ctx context.Context, argv []string, timeout time.Duration) (Output, error) {
	out := Output{ExitCode: -1}
	if len(argv) == 0 {
		return out, errors.New("empty adb command")
	}

	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	// Never cmd.Stdout = os.Stdout: stdout is the MCP transport and one stray
	// byte corrupts the session.
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// adb may fork a server daemon that inherits our pipes. Bound how long Wait
	// blocks draining them once the process itself has exited, so starting the
	// daemon can never wedge a tool call. The environment is inherited verbatim
	// (ANDROID_SERIAL included — see the package comment).
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()

	out.Stdout = stdout.Bytes()
	out.Stderr = stderr.Bytes()
	if cmd.ProcessState != nil {
		out.ExitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case runErr == nil:
		return out, nil
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The process finished; only the pipe drain timed out. Whatever was
		// captured is what Python would have seen.
		return out, nil
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		// CommandContext already sent SIGKILL and Wait reaped the child, so no
		// orphaned adb is left behind — same as subprocess.run(timeout=...).
		out.TimedOut = true
		return out, nil
	case ctx.Err() != nil:
		out.Cancelled = true
		return out, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		out.ExitCode = exitErr.ExitCode()
		return out, nil
	}
	// Could not start it at all: missing, not executable, wrong architecture.
	// Python surfaces the OSError text here; server.py catches OSError too.
	return out, runErr
}

// RunnerFunc adapts a plain function to Runner, for tests:
//
//	client := adb.NewWithRunner("SERIAL", "/fake/adb", adb.RunnerFunc(
//	    func(_ context.Context, argv []string, _ time.Duration) (adb.Output, error) {
//	        return adb.Output{Stdout: []byte("Physical size: 1080x2280\n")}, nil
//	    }))
type RunnerFunc func(ctx context.Context, argv []string, timeout time.Duration) (Output, error)

// RunADB implements Runner.
func (f RunnerFunc) RunADB(ctx context.Context, argv []string, timeout time.Duration) (Output, error) {
	return f(ctx, argv, timeout)
}
