package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hardeningMiddleware returns the receiving middleware every session runs
// through. Both entries exist because a fault here kills the whole process, and
// with it the scrcpy session, the adb forwards and the loopback listener — none
// of whose cleanups run, because the fault happens on an SDK goroutine and
// main's deferred Env.Shutdown never sees it.
//
// Order matters: recovery is outermost so it also covers the normaliser.
func hardeningMiddleware(log *slog.Logger) []mcp.Middleware {
	return []mcp.Middleware{recoverMiddleware(log), normalizeArgumentsMiddleware()}
}

// jsonNull is the four bytes a client sends for an explicitly null value.
var jsonNull = []byte("null")

// normalizeArgumentsMiddleware rewrites a tools/call whose "arguments" member is
// JSON null into one with no arguments at all.
//
// Without it such a call panics the server. go-sdk v1.6.1 mcp.applySchema
// (mcp/tool.go:91) unmarshals the raw arguments into a map[string]any — and
// unmarshalling null into a map produces a NIL map, not an empty one — then
// hands it to jsonschema-go v0.4.3 Resolved.ApplyDefaults, which calls
// SetMapIndex on it (jsonschema/validate.go:741): "assignment to entry in nil
// map". It fires for every tool that declares a schema default, which is most of
// this server, and it fires before any tool code runs, so no handler can defend
// against it.
//
// null is a legal JSON value for an optional member and some clients emit it
// rather than omitting the member; the SDK's own ClientSession.CallTool does
// exactly that when handed a typed nil map. Treating it as absent is what the
// SDK already does for a missing member, so this only widens what is accepted —
// no tool sees a different argument set than it would have.
func normalizeArgumentsMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
				if isJSONNull(call.Params.Arguments) {
					call.Params.Arguments = nil
				}
			}
			return next(ctx, method, req)
		}
	}
}

func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), jsonNull)
}

// recoverMiddleware turns a panic anywhere below it into a JSON-RPC error for
// that one request, leaving the session — and the scrcpy stream — alive.
//
// A panic on an SDK handler goroutine otherwise terminates the process: main's
// deferred Env.Shutdown does not run, so the scrcpy-server process keeps running
// on the device, `adb forward` entries survive and the loopback port stays
// bound. Reporting one failed call is strictly better than that.
//
// The stack goes to stderr. Never stdout: stdout is the JSON-RPC transport.
func recoverMiddleware(log *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				stack := string(debug.Stack())
				if log != nil {
					log.Error("panic while handling request", "method", method, "panic", r, "stack", stack)
				}
				result = nil
				err = fmt.Errorf("internal error handling %s: %v", method, r)
			}()
			return next(ctx, method, req)
		}
	}
}
