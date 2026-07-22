// Command phone-agent is the ScrcpyMac Phone Agent plugin binary: the MCP
// server plus the small CLI surface bin/phone-agent exposes.
//
// It replaces the Python server and its first-run bootstrap. Nothing is
// installed at runtime: adb and scrcpy-server ship next to the binary and the
// widget is embedded.
//
// Usage: phone-agent {mcp|doctor|devices|version}
//
// The one inviolable rule: while `mcp` runs, stdout carries JSON-RPC and
// nothing else. Every log line, warning and subprocess output goes to stderr.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/doctor"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/paths"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/scrcpy"
	versionpkg "github.com/zjywill/scrcpyMac/phone-agent/internal/version"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/widget"

	// Registers every MCP tool group. Tool packages self-register from init(),
	// so this import is what puts them on the server; see internal/tools.
	_ "github.com/zjywill/scrcpyMac/phone-agent/internal/tools"
)

// Overridden at link time by scripts/build-go.sh:
//
//	-ldflags "-X main.version=<plugin version> -X main.commit=<short sha>"
var (
	version string
	commit  string
)

func main() {
	// Before anything else: the standard logger must not touch stdout.
	log.SetFlags(0)
	log.SetOutput(os.Stderr)
	versionpkg.Set(version, commit)

	os.Exit(run())
}

// run holds every code path so that deferred cleanup always executes; os.Exit
// happens only in main, after run has returned. A stray os.Exit deeper down
// would leak the scrcpy process, the adb forward and the loopback listener.
func run() int {
	// Make the resolved root visible to child processes — the long-lived
	// `adb shell app_process` for scrcpy-server, and any helper script — so
	// they see exactly what the bash launcher would have exported.
	paths.Export()

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "mcp":
		err = runMCP(ctx)
	case "doctor":
		err = runDoctor(ctx)
	case "devices":
		err = runDevices(ctx)
	case "version":
		fmt.Fprintln(os.Stdout, versionpkg.CLI())
	default:
		usage()
		return 1
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "phone-agent %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "ScrcpyMac Phone Agent")
	fmt.Fprintln(os.Stderr, "Usage: phone-agent {mcp|doctor|devices|version}")
}

// runMCP serves MCP over stdio until the client disconnects or a signal
// arrives.
func runMCP(ctx context.Context) error {
	logger := mcpserver.NewLogger(os.Stderr)
	ensureADB(logger)

	server, err := mcpserver.New(ctx, mcpserver.Options{
		Log: logger,
		// The listener must already be bound here: its port is baked into the
		// widget resource's CSP connectDomains for the process lifetime, so
		// binding after New would publish a CSP that forbids the very port the
		// stream is served on. This mirrors the Python, which binds the
		// loopback on the first line of register_scrcpymac_app. A failure is
		// logged and returns 0, which leaves only the wildcard CSP entries —
		// degraded, not fatal.
		LoopbackPort: scrcpy.EnsureDefaultLoopback(logger),
	})
	if err != nil {
		return err
	}
	// Releases the scrcpy session, adb forwards and the loopback listener,
	// whatever ends the session.
	defer server.Env.Shutdown()

	logger.Info("phone-agent mcp starting",
		"version", versionpkg.String(),
		"root", server.Env.Root.Path,
		"rootDerived", server.Env.Root.Derived,
		"widgetBytes", widgetSize())

	if err := server.Serve(ctx); err != nil && !mcpserver.IsCleanShutdown(err) {
		return err
	}
	return nil
}

// ensureADB resolves adb once at startup so ADB_PATH is exported for child
// processes and a missing binary is reported on stderr rather than as a
// mysterious per-tool failure.
//
// It is never fatal: the Python launcher behaves the same way, and the tools
// report setup guidance in their own results. Auto-downloading platform-tools
// (PHONE_AGENT_AUTO_DOWNLOAD_ADB) is still handled by the bash launcher today;
// the in-process downloader is a follow-up.
func ensureADB(logger *slog.Logger) {
	path, err := adb.ResolvePath()
	if err != nil {
		logger.Warn("adb not found; phone tools will report setup guidance", "err", err)
		return
	}
	if os.Getenv(adb.PathEnv) != path {
		_ = os.Setenv(adb.PathEnv, path)
	}
}

// runDoctor prints the diagnostics payload. It exits 0 even when ok is false —
// the checks are the report, not the exit status.
func runDoctor(ctx context.Context) error {
	fmt.Fprintln(os.Stdout, jsonresult.Text(doctor.Run(ctx)))
	return nil
}

// runDevices prints a BARE JSON array of devices. The {"devices": [...]} wrapper
// exists only on the phone_list_devices MCP tool.
func runDevices(ctx context.Context) error {
	client, err := adb.New("")
	if err != nil {
		return err
	}
	devices, err := client.ListDevices(ctx)
	if err != nil {
		return err
	}
	if devices == nil {
		devices = []adb.Device{}
	}
	fmt.Fprintln(os.Stdout, jsonresult.Text(devices))
	return nil
}

// widgetSize reports the embedded widget size and warns about a build that
// embedded an empty document, which would serve a blank workspace.
func widgetSize() int {
	size := widget.Size()
	if size == 0 {
		log.Println("WARN: embedded widget is empty; run scripts/build-ui.sh, then make widget, then rebuild")
	}
	return size
}
