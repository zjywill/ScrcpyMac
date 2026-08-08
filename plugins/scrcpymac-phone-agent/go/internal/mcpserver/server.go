// Package mcpserver builds the MCP server: identity, capabilities, the widget
// resource, graceful shutdown, and the registration interface that tool packages
// hook into.
//
// # How tools get registered
//
// Tool implementations live in internal/tools, one file per group, and register
// themselves from an init() function:
//
//	func init() {
//	    mcpserver.Register(mcpserver.Registration{
//	        Name:  "phone-input",
//	        Order: mcpserver.OrderPhoneTools + 20,
//	        Apply: registerPhoneInput,
//	    })
//	}
//
//	func registerPhoneInput(s *mcp.Server, env *mcpserver.Env) error { ... }
//
// cmd/phone-agent blank-imports internal/tools, so every file's init runs before
// New is called; New then applies the registrations sorted by Order (ties broken
// by registration sequence, which is file-name order within a package). Because
// each group owns its own file and its own Order band, several agents can add
// tools in parallel without touching a shared file.
//
// Apply runs at server-construction time, not at init time, so Env — and
// anything a tool package lazily builds from it — is fully available.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/paths"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/version"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/widget"
)

// ServerName is the MCP server name. mcp.json and the Codex/Cursor plugin
// manifests key off it.
const ServerName = "scrcpymac-phone-agent"

// ServerTitle is the human-readable name.
const ServerTitle = "ScrcpyMac Phone Agent"

// Instructions is sent verbatim in the initialize result. The Python builds it
// from four adjacent string literals; this is the resulting single line, with no
// trailing newline and single spaces at the seams.
const Instructions = "Control a connected Android phone. " +
	"The plugin owns its standalone scrcpy H.264 and control session; ScrcpyMac.app is not required. " +
	"ADB remains available for screenshots and accessibility automation. " +
	"For Chinese text, use phone_paste. WeChat: com.tencent.mm."

// Registration order bands. The MCP spec does not define an ordering for
// tools/list and this SDK sorts its feature set by name, so these do not control
// the wire order — they make startup deterministic and keep related tools
// together in logs and tests. Leave gaps so a group can be split later.
const (
	OrderWidgetTool = 100 // open_scrcpymac + the widget-facing surface
	OrderAppTools   = 200 // scrcpymac_ui_* (app-only)
	OrderPhoneTools = 300 // phone_* (model-visible)
)

// ToolRegistrar adds tools (or resources) to the server.
type ToolRegistrar func(s *mcp.Server, env *Env) error

// Registration is one tool group.
type Registration struct {
	// Name identifies the group in logs and errors, e.g. "phone-input".
	Name string
	// Order places the group; see the Order* constants.
	Order int
	// Apply performs the registration. It must not block.
	Apply ToolRegistrar
}

// Registry collects registrations.
type Registry struct {
	mu   sync.Mutex
	regs []Registration
}

// NewRegistry returns an empty registry. Tests use this to build an isolated
// server; production code uses the package-level Register.
func NewRegistry() *Registry { return &Registry{} }

// Add appends registrations. It panics on an unusable registration, which is a
// programming error that should fail at startup rather than silently drop tools.
func (r *Registry) Add(regs ...Registration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, reg := range regs {
		if reg.Name == "" {
			panic("mcpserver: registration with empty Name")
		}
		if reg.Apply == nil {
			panic("mcpserver: registration " + reg.Name + " has nil Apply")
		}
		r.regs = append(r.regs, reg)
	}
}

// List returns the registrations sorted by Order, ties broken by insertion.
func (r *Registry) List() []Registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Registration, len(r.regs))
	copy(out, r.regs)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

var defaultRegistry = NewRegistry()

// Register adds registrations to the default registry, which New uses when
// Options.Registry is nil. Intended to be called from init().
func Register(regs ...Registration) { defaultRegistry.Add(regs...) }

// DefaultRegistry exposes the registry Register writes to.
func DefaultRegistry() *Registry { return defaultRegistry }

// Env is the shared runtime context handed to every registrar and, through
// closures, to every tool handler.
type Env struct {
	// Root is the resolved plugin root.
	Root paths.Root
	// Version is the plugin version string.
	Version string
	// Log writes to stderr. Never to stdout: stdout is the JSON-RPC transport.
	Log *slog.Logger

	ctx context.Context

	mu       sync.Mutex
	adb      *adb.Client
	cleanups []cleanup
	closed   bool
}

type cleanup struct {
	name string
	fn   func() error
}

// Context returns the server lifetime context. It is cancelled on SIGINT,
// SIGTERM, or when the client closes stdin. Long-running work — the scrcpy
// session, the loopback listener, the packet pump — must select on it.
func (e *Env) Context() context.Context {
	if e.ctx == nil {
		return context.Background()
	}
	return e.ctx
}

// OnShutdown registers a cleanup function. They run in reverse registration
// order after the transport stops, exactly once, whether the server exited
// cleanly or not. This is where the scrcpy process, `adb forward --remove` and
// the loopback listener belong: nothing else guarantees they are released.
func (e *Env) OnShutdown(name string, fn func() error) {
	if fn == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		// Already shutting down; run it now rather than dropping it.
		go func() { _ = fn() }()
		return
	}
	e.cleanups = append(e.cleanups, cleanup{name: name, fn: fn})
}

// Shutdown runs the registered cleanups, most recent first. It is idempotent, so
// it is safe both as a defer and on an explicit error path.
func (e *Env) Shutdown() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	pending := e.cleanups
	e.cleanups = nil
	e.mu.Unlock()

	for i := len(pending) - 1; i >= 0; i-- {
		if err := pending[i].fn(); err != nil {
			e.Log.Warn("shutdown step failed", "step", pending[i].name, "err", err)
		}
	}
}

// ADB returns the shared adb client, resolving the binary on first use.
//
// A failure is not cached: a user who installs platform-tools mid-session gets a
// working server on the next call, which is how the Python behaved.
func (e *Env) ADB() (*adb.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.adb != nil {
		return e.adb, nil
	}
	client, err := adb.New("")
	if err != nil {
		return nil, err
	}
	e.adb = client
	return client, nil
}

// Options configures New.
type Options struct {
	// Version defaults to the compiled-in plugin version.
	Version string
	// Instructions defaults to the frozen Instructions constant.
	Instructions string
	// Log defaults to a stderr text logger at info level.
	Log *slog.Logger
	// Registry defaults to the package registry populated by Register.
	Registry *Registry
	// LoopbackPort is the port the plugin's loopback WebSocket listener is
	// bound to. It is baked into the widget resource's CSP connectDomains for
	// the process lifetime, so the listener must already be bound when New runs
	// — that ordering is inherited from the Python, where the resource _meta is
	// computed on the first line of register_scrcpymac_app. Zero means "not
	// bound yet": only the wildcard CSP entries are published.
	LoopbackPort int
}

// Server is the assembled MCP server.
type Server struct {
	MCP *mcp.Server
	Env *Env
}

// New builds the server: identity, capabilities, the widget resource, then every
// registered tool group in Order.
//
// ctx becomes Env.Context() and should already carry signal cancellation.
func New(ctx context.Context, opts Options) (*Server, error) {
	if opts.Version == "" {
		opts.Version = version.String()
	}
	if opts.Instructions == "" {
		opts.Instructions = Instructions
	}
	if opts.Log == nil {
		opts.Log = NewLogger(os.Stderr)
	}
	if opts.Registry == nil {
		opts.Registry = defaultRegistry
	}
	if ctx == nil {
		ctx = context.Background()
	}

	env := &Env{
		Root:    paths.Resolve(),
		Version: opts.Version,
		Log:     opts.Log,
		ctx:     ctx,
	}

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    ServerName,
			Title:   ServerTitle,
			Version: opts.Version,
		},
		&mcp.ServerOptions{
			Instructions: opts.Instructions,
			// Must be explicit. A nil Capabilities makes the SDK advertise
			// {"logging":{}} plus listChanged:true; the Python advertises
			// listChanged:false and no logging.
			Capabilities: &mcp.ServerCapabilities{
				Tools:     &mcp.ToolCapabilities{ListChanged: false},
				Resources: &mcp.ResourceCapabilities{ListChanged: false, Subscribe: false},
				Prompts:   &mcp.PromptCapabilities{ListChanged: false},
			},
			// Logger stays nil => slog.DiscardHandler inside the SDK. Anything
			// else risks writing to stdout, which is the transport.
		},
	)

	// Applied before anything is registered so it covers every request the
	// server will ever see, including the ones tool groups add below.
	server.AddReceivingMiddleware(hardeningMiddleware(opts.Log)...)

	addWidgetResource(server, opts.LoopbackPort)

	for _, reg := range opts.Registry.List() {
		if err := reg.Apply(server, env); err != nil {
			return nil, fmt.Errorf("register %s: %w", reg.Name, err)
		}
	}

	return &Server{MCP: server, Env: env}, nil
}

// addWidgetResource publishes ui://widget/scrcpymac/app.html.
//
// The CSP connect-domain lists inside meta are resolved at marshal time from
// widget.SetLoopbackPort, so a runtime that binds its listener after this point
// (during its own registration, or later) still publishes the right port —
// internal/tools installs widget.SetLoopbackPort as the runtime's loopback
// observer, which is what makes a late bind reach this CSP. Seeding from
// opts.LoopbackPort keeps "bind before New and pass the port" working unchanged,
// and keeps the value deterministic per server instance.
func addWidgetResource(server *mcp.Server, loopbackPort int) {
	widget.SetLoopbackPort(loopbackPort)
	meta := widget.LiveResourceMeta()
	server.AddResource(
		&mcp.Resource{
			Meta:        meta,
			URI:         widget.URI,
			Name:        widget.Name,
			Title:       widget.Title,
			Description: widget.Description,
			MIMEType:    widget.MIMEType,
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			html := widget.HTML()
			if html == "" {
				return nil, errors.New("ScrcpyMac widget build is missing. Run scripts/build-ui.sh.")
			}
			return &mcp.ReadResourceResult{
				Meta: meta,
				Contents: []*mcp.ResourceContents{{
					URI:      widget.URI,
					MIMEType: widget.MIMEType,
					Text:     html,
				}},
			}, nil
		},
	)
}

// Serve runs the server over stdio until the context is cancelled or the client
// disconnects.
//
// Nothing but JSON-RPC may reach stdout while this runs: mcp.StdioTransport is
// literally {os.Stdin, os.Stdout}, so a single stray byte corrupts the session.
func (s *Server) Serve(ctx context.Context) error {
	return s.MCP.Run(ctx, &mcp.StdioTransport{})
}

// IsCleanShutdown reports whether err from Serve means an ordinary end of
// session rather than a failure.
//
// Three measured cases: SIGTERM/SIGINT yields context.Canceled; stdin EOF after
// a completed handshake yields nil; stdin EOF before the handshake completes
// yields a non-nil "server is closing: EOF", which is what a client that spawns
// and immediately closes the process produces.
func IsCleanShutdown(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return true
	}
	return strings.Contains(err.Error(), "server is closing") && strings.Contains(err.Error(), "EOF")
}

// NewLogger returns the plugin's standard logger. It must never be pointed at
// stdout.
func NewLogger(w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	level := slog.LevelInfo
	if v := strings.ToLower(os.Getenv("PHONE_AGENT_LOG_LEVEL")); v != "" {
		switch v {
		case "debug":
			level = slog.LevelDebug
		case "warn", "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
