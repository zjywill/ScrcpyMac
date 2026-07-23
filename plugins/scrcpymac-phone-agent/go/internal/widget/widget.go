// Package widget embeds the ScrcpyMac MCP App widget and describes the resource
// that serves it.
//
// # Where the HTML comes from
//
// The widget is built from ui/src by scripts/build-ui.sh directly into
// assets/scrcpymac-app.html. It is committed because a Marketplace install must
// never depend on npm being reachable.
//
// Refresh it with `make widget` or scripts/build-go.sh before compiling release
// binaries. Building Go without refreshing embeds the previous UI.
package widget

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync/atomic"
)

//go:embed assets/scrcpymac-app.html
var html string

// Resource identity. Every field is frozen contract: Codex keys off the URI and
// the mimeType profile parameter.
const (
	URI         = "ui://widget/scrcpymac/app.html"
	Name        = "scrcpymac-app"
	Title       = "ScrcpyMac"
	Description = "Interactive Android mirroring and control workspace."
	MIMEType    = "text/html;profile=mcp-app"
)

// HTML returns the embedded widget document.
func HTML() string { return html }

// Size returns the embedded document size in bytes.
func Size() int { return len(html) }

// ConnectDomains returns the CSP connect-domain list for the widget, given the
// port the plugin's loopback WebSocket listener is bound to.
//
// The eight entries and their order are copied from
// scrcpy_runtime.stream_connect_domains(). The four wildcard entries are what
// keep the widget working if the literal port ever goes stale — the Python binds
// the loopback at import time and never rebinds it while the published _meta
// still advertises the old port.
//
// port <= 0 means the loopback is not bound yet, in which case only the wildcard
// entries are emitted; there is no port to advertise and inventing one would put
// a wrong literal in the widget's CSP.
func ConnectDomains(port int) []string {
	wildcards := []string{
		"http://127.0.0.1:*",
		"ws://127.0.0.1:*",
		"http://localhost:*",
		"ws://localhost:*",
	}
	if port <= 0 {
		return wildcards
	}
	return append([]string{
		fmt.Sprintf("http://127.0.0.1:%d", port),
		fmt.Sprintf("ws://127.0.0.1:%d", port),
		fmt.Sprintf("http://localhost:%d", port),
		fmt.Sprintf("ws://localhost:%d", port),
	}, wildcards...)
}

// loopbackPort holds the port of the plugin's loopback WebSocket listener.
//
// It exists because of a hard ordering problem inherited from the Python. The
// widget's CSP has to name the port its own stream is served on, and the CSP is
// published in the resource _meta — which the host reads once, when it creates
// the widget iframe. The Python got away with computing it at registration time
// only because it binds 127.0.0.1:0 at *import* of phone_agent.server, before
// any tool exists. In Go the listener is owned by the scrcpy runtime, which is
// registered after the resource, so a value captured at New() time would always
// be zero.
//
// Making the port a process-wide atomic that the CSP reads at marshal time
// removes the ordering constraint entirely: whoever binds the listener calls
// SetLoopbackPort, and every subsequent resources/list and resources/read
// carries the real port. Binding before mcpserver.New still works — New seeds
// this from Options.LoopbackPort — it is simply no longer required.
//
// Note what is NOT fixed by this: a host that has already instantiated the
// widget keeps the CSP it was given. Bind the listener at process start (as the
// Python does) so the port is known before the first resources/read; the four
// wildcard entries are the safety net if it is not.
var loopbackPort atomic.Int64

// SetLoopbackPort publishes the loopback WebSocket port used by the widget CSP.
// A port <= 0 clears it, which leaves only the wildcard entries.
//
// Call it from whatever binds the listener, immediately after the bind and
// before serving. It must never be called with a port that is not already
// listening: the CSP would then permit a port nothing answers on, which is
// exactly the failure this indirection exists to prevent.
func SetLoopbackPort(port int) {
	if port < 0 {
		port = 0
	}
	loopbackPort.Store(int64(port))
}

// LoopbackPort returns the port last published by SetLoopbackPort, or 0.
func LoopbackPort() int { return int(loopbackPort.Load()) }

// LiveConnectDomains is ConnectDomains for the currently published port.
func LiveConnectDomains() []string { return ConnectDomains(LoopbackPort()) }

// liveConnectDomains renders the connect-domain list at marshal time rather than
// at construction time. mcp.Meta is a plain map[string]any that the SDK marshals
// per response, so a json.Marshaler stored in it is evaluated on every
// resources/list and resources/read.
type liveConnectDomains struct{}

// MarshalJSON implements json.Marshaler.
func (liveConnectDomains) MarshalJSON() ([]byte, error) {
	return json.Marshal(LiveConnectDomains())
}

// ResourceMeta builds the resource _meta block for a fixed connect-domain list.
//
// Note the deliberate key-style split, which is contract: the ui.csp block uses
// camelCase (connectDomains/resourceDomains) while openai/widgetCSP uses
// snake_case (connect_domains/resource_domains), with identical values.
func ResourceMeta(connectDomains []string) map[string]any {
	return resourceMeta(connectDomains)
}

// LiveResourceMeta builds the resource _meta block with both CSP connect-domain
// lists resolved from SetLoopbackPort at marshal time. This is what the server
// publishes; see the loopbackPort comment for why.
func LiveResourceMeta() map[string]any {
	return resourceMeta(liveConnectDomains{})
}

// resourceMeta is the single definition of the _meta block. connectDomains is
// either a []string or a json.Marshaler that produces one.
func resourceMeta(connectDomains any) map[string]any {
	resourceDomains := []string{"data:", "blob:"}
	return map[string]any{
		"ui": map[string]any{
			"prefersBorder": false,
			"csp": map[string]any{
				"connectDomains":  connectDomains,
				"resourceDomains": resourceDomains,
			},
		},
		"openai/widgetDescription":   "Full ScrcpyMac phone control workspace.",
		"openai/widgetPrefersBorder": false,
		"openai/widgetCSP": map[string]any{
			"connect_domains":  connectDomains,
			"resource_domains": resourceDomains,
		},
	}
}
