package adb

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a fake Runner: it records the argv, timeout and context state of
// every invocation and answers from a handler. No adb binary, no device.
type recorder struct {
	mu       sync.Mutex
	argv     [][]string
	timeouts []time.Duration
	ctxLive  []bool // ctx.Err() == nil at call time
	handler  func(call int, argv []string) (Output, error)
}

func newRecorder(h func(call int, argv []string) (Output, error)) *recorder {
	return &recorder{handler: h}
}

// reply answers every invocation with the same stdout and exit 0.
func reply(stdout string) func(int, []string) (Output, error) {
	return func(int, []string) (Output, error) {
		return Output{Stdout: []byte(stdout)}, nil
	}
}

// replies answers the nth invocation with the nth string, then repeats the last.
func replies(stdouts ...string) func(int, []string) (Output, error) {
	return func(call int, _ []string) (Output, error) {
		if call >= len(stdouts) {
			call = len(stdouts) - 1
		}
		return Output{Stdout: []byte(stdouts[call])}, nil
	}
}

func (r *recorder) RunADB(ctx context.Context, argv []string, timeout time.Duration) (Output, error) {
	r.mu.Lock()
	call := len(r.argv)
	r.argv = append(r.argv, append([]string(nil), argv...))
	r.timeouts = append(r.timeouts, timeout)
	r.ctxLive = append(r.ctxLive, ctx.Err() == nil)
	handler := r.handler
	r.mu.Unlock()

	if handler == nil {
		return Output{}, nil
	}
	return handler(call, argv)
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.argv)
}

func (r *recorder) wantCalls(t *testing.T, want int) {
	t.Helper()
	if got := r.calls(); got != want {
		t.Fatalf("adb was invoked %d times, want %d: %v", got, want, r.argv)
	}
}

func (r *recorder) wantArgv(t *testing.T, call int, want ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if call >= len(r.argv) {
		t.Fatalf("no call %d; recorded %d", call, len(r.argv))
	}
	if !reflect.DeepEqual(r.argv[call], want) {
		t.Errorf("call %d argv =\n %q\nwant %q", call, r.argv[call], want)
	}
}

func (r *recorder) wantTimeout(t *testing.T, call int, want time.Duration) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if call >= len(r.timeouts) {
		t.Fatalf("no call %d; recorded %d", call, len(r.timeouts))
	}
	if r.timeouts[call] != want {
		t.Errorf("call %d timeout = %s, want %s", call, r.timeouts[call], want)
	}
}

// wantOneShellElement asserts the device command travelled as a single argv
// element. If it were split host-side, the pipes and && in ui_tree_xml and
// current_app would be handed to adb as separate arguments and silently break.
func (r *recorder) wantOneShellElement(t *testing.T, call int, wantCommand string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	argv := r.argv[call]
	shellAt := -1
	for i, arg := range argv {
		if arg == "shell" {
			shellAt = i
			break
		}
	}
	if shellAt < 0 {
		t.Fatalf("call %d is not a shell invocation: %q", call, argv)
	}
	rest := argv[shellAt+1:]
	if len(rest) != 1 {
		t.Fatalf("shell command was split into %d argv elements: %q", len(rest), rest)
	}
	if rest[0] != wantCommand {
		t.Errorf("shell command =\n %q\nwant %q", rest[0], wantCommand)
	}
}

func mustNotContain(t *testing.T, s, sub, why string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Errorf("%q must not contain %q: %s", s, sub, why)
	}
}
