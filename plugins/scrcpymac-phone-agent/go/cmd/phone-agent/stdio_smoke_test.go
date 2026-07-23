// Stdio smoke test: the real binary, real JSON-RPC, real handshake.
//
// Everything else in this repo tests the server in-process. This one builds
// cmd/phone-agent, runs `phone-agent mcp`, pipes newline-delimited JSON-RPC into
// its stdin and reads its stdout — the same path Codex and Cursor use.
//
// The assertion that matters most is the last one: stdout must carry JSON-RPC
// and nothing else. A single stray log line, banner or fmt.Print corrupts the
// framing and the client drops the session with a parse error, which is a
// release blocker and is invisible to every in-process test.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const smokeProtocolVersion = "2025-06-18"

// buildSmokeBinary compiles cmd/phone-agent into the test's temp directory.
func buildSmokeBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "phone-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// pluginRoot is the plugin directory two levels above go/, i.e. the layout the
// launcher exports as PHONE_AGENT_ROOT.
func pluginRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve plugin root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "mcp.json")); err != nil {
		t.Fatalf("plugin root %s does not look right: %v", root, err)
	}
	return root
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Method string `json:"method"`
}

// TestStdioHandshakeOverTheRealBinary drives initialize, notifications/
// initialized, tools/list and resources/list through the shipped executable.
func TestStdioHandshakeOverTheRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; skipped under -short")
	}
	bin := buildSmokeBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "mcp")
	cmd.Env = append(os.Environ(),
		"PHONE_AGENT_ROOT="+pluginRoot(t),
		// Loud on purpose: if any of this reached stdout the framing check below
		// would catch it. It is the strongest form of the assertion.
		"PHONE_AGENT_LOG_LEVEL=debug",
		// The shipped surface is the contract's, whatever the developer running
		// this happens to have exported.
		"PHONE_AGENT_STREAM_DIAGNOSTICS=",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s mcp: %v", bin, err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + smokeProtocolVersion +
			`","capabilities":{},"clientInfo":{"name":"stdio-smoke","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{}}`,
		// A tools/call the device cannot fail: phone_backend only reads runtime
		// state. It proves the handler path works end to end, not just listing.
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"phone_backend","arguments":{}}}`,
		// The shape that used to kill the process: an explicit null arguments
		// member against a tool that declares a schema default.
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"open_scrcpymac","arguments":null}}`,
	}
	for _, req := range requests {
		if _, err := io.WriteString(stdin, req+"\n"); err != nil {
			t.Fatalf("write request: %v\nstderr:\n%s", err, stderr.String())
		}
	}

	// Read exactly the five responses, then close stdin and drain whatever is
	// left so nothing written to stdout escapes inspection.
	reader := bufio.NewReader(stdout)
	responses := make(map[string]rpcResponse)
	var stdoutLines []string
	for len(responses) < 5 {
		line, err := readLineWithDeadline(t, reader, 30*time.Second)
		if err != nil {
			t.Fatalf("read response %d/5: %v\nstdout so far:\n%s\nstderr:\n%s",
				len(responses)+1, err, strings.Join(stdoutLines, "\n"), stderr.String())
		}
		stdoutLines = append(stdoutLines, line)
		resp := parseJSONRPCLine(t, line)
		if len(resp.ID) > 0 {
			responses[string(resp.ID)] = resp
		}
	}

	_ = stdin.Close()
	rest, _ := io.ReadAll(reader)
	for _, line := range strings.Split(string(rest), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		stdoutLines = append(stdoutLines, line)
		parseJSONRPCLine(t, line)
	}

	if err := cmd.Wait(); err != nil {
		t.Errorf("phone-agent mcp exited with %v; stdin EOF is an ordinary end of session\nstderr:\n%s",
			err, stderr.String())
	}

	// --- initialize -------------------------------------------------------
	init := mustResult(t, responses, "1", "initialize")
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(init, &initResult); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if initResult.ServerInfo.Name != "scrcpymac-phone-agent" {
		t.Errorf("serverInfo.name = %q", initResult.ServerInfo.Name)
	}
	if initResult.ServerInfo.Version == "" {
		t.Error("serverInfo.version is empty")
	}
	if initResult.ProtocolVersion != smokeProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", initResult.ProtocolVersion, smokeProtocolVersion)
	}
	if !strings.HasPrefix(initResult.Instructions, "Control a connected Android phone.") {
		t.Errorf("instructions = %q", initResult.Instructions)
	}
	for _, capability := range []string{"tools", "resources"} {
		if _, ok := initResult.Capabilities[capability]; !ok {
			t.Errorf("capabilities is missing %q: %v", capability, initResult.Capabilities)
		}
	}

	// --- tools/list -------------------------------------------------------
	var toolsResult struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(mustResult(t, responses, "2", "tools/list"), &toolsResult); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range toolsResult.Tools {
		if tool.Description == "" {
			t.Errorf("tool %q has an empty description", tool.Name)
		}
		if len(tool.InputSchema) == 0 {
			t.Errorf("tool %q has no inputSchema", tool.Name)
		}
		names[tool.Name] = true
	}
	// The exhaustive comparison lives in internal/mcpserver's contract test;
	// here it is enough that the shipped binary carries the whole surface.
	for _, want := range []string{
		"open_scrcpymac", "scrcpymac_ui_state", "scrcpymac_ui_stream_pull",
		"scrcpymac_ui_snapshot",
		"phone_backend", "phone_doctor", "phone_tap", "phone_ui_tree",
		"phone_send_wechat", "phone_connect_wifi",
	} {
		if !names[want] {
			t.Errorf("tools/list is missing %q", want)
		}
	}
	// Exactly 37, not "at least": the shipped binary must publish the frozen
	// plugin surface and nothing more.
	// Diagnostics are opt-in and cleared in cmd.Env above.
	if len(toolsResult.Tools) != 37 {
		t.Errorf("tools/list returned %d tools, want exactly the 37 in the contract", len(toolsResult.Tools))
	}

	// --- resources/list ---------------------------------------------------
	var resourcesResult struct {
		Resources []struct {
			URI      string `json:"uri"`
			Name     string `json:"name"`
			MIMEType string `json:"mimeType"`
			Meta     struct {
				UI struct {
					CSP struct {
						ConnectDomains []string `json:"connectDomains"`
					} `json:"csp"`
				} `json:"ui"`
			} `json:"_meta"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(mustResult(t, responses, "3", "resources/list"), &resourcesResult); err != nil {
		t.Fatalf("decode resources/list: %v", err)
	}
	if len(resourcesResult.Resources) != 1 {
		t.Fatalf("resources/list returned %d resources, want 1", len(resourcesResult.Resources))
	}
	res := resourcesResult.Resources[0]
	if res.URI != "ui://widget/scrcpymac/app.html" {
		t.Errorf("resource uri = %q", res.URI)
	}
	if res.MIMEType != "text/html;profile=mcp-app" {
		t.Errorf("resource mimeType = %q", res.MIMEType)
	}
	// The loopback listener binds before the resource is published, so the CSP
	// must name a real port rather than the wildcards alone. A regression here
	// means the widget's own stream URL is blocked by its own CSP.
	if len(res.Meta.UI.CSP.ConnectDomains) != 8 {
		t.Fatalf("resource CSP connectDomains = %v, want 8 entries", res.Meta.UI.CSP.ConnectDomains)
	}
	if strings.HasSuffix(res.Meta.UI.CSP.ConnectDomains[0], ":0") {
		t.Errorf("resource CSP connectDomains[0] = %q; the loopback listener did not bind",
			res.Meta.UI.CSP.ConnectDomains[0])
	}

	// --- tools/call -------------------------------------------------------
	var backend struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal(mustResult(t, responses, "4", "tools/call phone_backend"), &backend); err != nil {
		t.Fatalf("decode tools/call: %v", err)
	}
	if backend.IsError {
		t.Errorf("phone_backend reported isError: %+v", backend.Content)
	}
	if len(backend.Content) == 0 || backend.Content[0].Type != "text" {
		t.Fatalf("phone_backend content = %+v, want one text block", backend.Content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(backend.Content[0].Text), &payload); err != nil {
		t.Fatalf("phone_backend text block is not JSON: %v\n%s", err, backend.Content[0].Text)
	}
	if payload["backend"] != "adb" {
		t.Errorf("phone_backend backend = %v, want \"adb\" with no stream running", payload["backend"])
	}

	// The null-arguments call must have been answered, not have killed the
	// process. cmd.Wait above already proves the process survived; this proves
	// the call itself succeeded.
	nullArgs := mustResult(t, responses, "5", "tools/call open_scrcpymac (arguments:null)")
	var opened struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(nullArgs, &opened); err != nil {
		t.Fatalf("decode open_scrcpymac result: %v", err)
	}
	if opened.IsError {
		t.Errorf("open_scrcpymac with null arguments returned an error result: %s", nullArgs)
	}

	// --- stdout hygiene ---------------------------------------------------
	// Every line was parsed as JSON-RPC on the way in; assert the log actually
	// went somewhere, and that it went to stderr.
	if stderr.Len() == 0 {
		t.Error("nothing on stderr: the startup log is missing, so this check proved nothing")
	}
	if !strings.Contains(stderr.String(), "phone-agent mcp starting") {
		t.Errorf("stderr does not carry the startup log:\n%s", stderr.String())
	}
	t.Logf("stdout: %d JSON-RPC messages, no other output; stderr: %d bytes",
		len(stdoutLines), stderr.Len())
}

// parseJSONRPCLine fails the test if a line of stdout is anything other than a
// well-formed JSON-RPC 2.0 message. This is the release-blocker check: one
// stray byte on stdout corrupts the transport.
func parseJSONRPCLine(t *testing.T, line string) rpcResponse {
	t.Helper()
	var resp rpcResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("stdout carried a non-JSON-RPC line — this corrupts the transport:\n%q\n%v", line, err)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("stdout line is not JSON-RPC 2.0: %q", line)
	}
	if resp.Error != nil {
		t.Fatalf("server returned a JSON-RPC error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	return resp
}

func mustResult(t *testing.T, responses map[string]rpcResponse, id, what string) json.RawMessage {
	t.Helper()
	resp, ok := responses[id]
	if !ok {
		t.Fatalf("no response with id %s (%s)", id, what)
	}
	if len(resp.Result) == 0 {
		t.Fatalf("%s returned no result", what)
	}
	return resp.Result
}

// readLineWithDeadline reads one newline-terminated line, failing rather than
// hanging the whole test binary if the server never answers.
func readLineWithDeadline(t *testing.T, r *bufio.Reader, timeout time.Duration) (string, error) {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{strings.TrimRight(line, "\r\n"), err}
	}()
	select {
	case res := <-ch:
		if res.err != nil && res.line == "" {
			return "", res.err
		}
		return res.line, nil
	case <-time.After(timeout):
		return "", errors.New("timed out waiting for a response line")
	}
}
