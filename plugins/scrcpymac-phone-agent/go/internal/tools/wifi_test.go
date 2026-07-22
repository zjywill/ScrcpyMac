package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

// ---------------------------------------------------------------------------
// Fixtures captured from the attached OnePlus 6 (serial 2f019965, adb 1.0.41).
// adb's text output is CRLF, which Client normalises before anything sees it;
// the raw bytes here keep the \r\n so the normalisation stays covered.
// ---------------------------------------------------------------------------

const (
	// `adb shell "ip route | awk '/wlan/ {print $9; exit}'"`
	wifiFixtureIPRouteProbe = "192.168.8.174\r\n"
	// `adb shell "ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1"`
	wifiFixtureIPAddrProbe = "192.168.8.174\r\n"
	// `adb shell "ip route"` — what the first probe would return if awk picked
	// the wrong column, i.e. a non-address that must be rejected.
	wifiFixtureIPRouteRaw = "192.168.8.0/24 dev wlan0 proto kernel scope link src 192.168.8.174 \r\n"
	// `adb shell "ip -f inet addr show wlan0"`, unfiltered.
	wifiFixtureIPAddrRaw = "30: wlan0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP group default qlen 3000\r\n" +
		"    inet 192.168.8.174/24 brd 192.168.8.255 scope global wlan0\r\n" +
		"       valid_lft forever preferred_lft forever\r\n"
	// `adb shell "ifconfig wlan0"`, the pre-`ip` layout some ROMs still ship.
	wifiFixtureIfconfigRaw = "wlan0     Link encap:UNSPEC    Driver icnss\r\n" +
		"          inet addr:192.168.8.174  Bcast:192.168.8.255  Mask:255.255.255.0 \r\n" +
		"          inet6 addr: fe80::240f:aaff:feb7:82f4/64 Scope: Link\r\n" +
		"          UP BROADCAST RUNNING MULTICAST  MTU:1500  Metric:1\r\n"
	// Wi-Fi off, or the interface absent: both pipelines exit 0 with no output.
	wifiFixtureNoInterface = ""

	// `adb tcpip 5555` (the host command; the Python's `adb shell tcpip` exits 127).
	wifiFixtureTCPIPOutput = "restarting in TCP mode port: 5555\r\n"

	wifiTestSerial = "2f019965"
)

// ---------------------------------------------------------------------------
// A Client backed by a scripted runner: no adb binary, no device.
// ---------------------------------------------------------------------------

type wifiRecordedRun struct {
	argv    []string
	timeout time.Duration
}

// wifiScriptedClient returns a real *adb.Client whose invocations are answered
// from replies, keyed by the LAST argv element (the shell command for
// `adb shell <cmd>`, the target for `adb connect <target>`, ...). Using the real
// Client keeps CRLF normalisation, trimming and error formatting in the test.
func wifiScriptedClient(t *testing.T, replies map[string]string, log *[]wifiRecordedRun) *adb.Client {
	t.Helper()
	return adb.NewWithRunner(wifiTestSerial, "/fake/adb",
		adb.RunnerFunc(func(_ context.Context, argv []string, timeout time.Duration) (adb.Output, error) {
			*log = append(*log, wifiRecordedRun{argv: append([]string(nil), argv...), timeout: timeout})
			key := argv[len(argv)-1]
			reply, ok := replies[key]
			if !ok {
				t.Errorf("unscripted adb invocation: %q", strings.Join(argv, " "))
			}
			return adb.Output{Stdout: []byte(reply)}, nil
		}))
}

// ---------------------------------------------------------------------------
// device_wifi_ip: the two probes, against real device output
// ---------------------------------------------------------------------------

func TestWiFiDeviceIPUsesTheRoutingTableFirst(t *testing.T) {
	var log []wifiRecordedRun
	client := wifiScriptedClient(t, map[string]string{
		`ip route | awk '/wlan/ {print $9; exit}'`: wifiFixtureIPRouteProbe,
	}, &log)

	payload, err := wifiDeviceIPPayload(context.Background(), client)
	if err != nil {
		t.Fatalf("wifiDeviceIPPayload: %v", err)
	}
	want := "{\n  \"ok\": true,\n  \"ip\": \"192.168.8.174\",\n  \"serial\": \"2f019965\"\n}"
	if got := jsonresult.Text(payload); got != want {
		t.Errorf("payload =\n%s\nwant\n%s", got, want)
	}
	// EnsureDevice short-circuits on a known serial, so the probe is the only
	// invocation: exactly one round trip on the happy path.
	if len(log) != 1 {
		t.Fatalf("want 1 adb invocation, got %d: %v", len(log), log)
	}
}

func TestWiFiDeviceIPProbeCommandsAreVerbatimAndOneArgvElement(t *testing.T) {
	var log []wifiRecordedRun
	client := wifiScriptedClient(t, map[string]string{
		`ip route | awk '/wlan/ {print $9; exit}'`:                                        wifiFixtureNoInterface,
		`ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1`: wifiFixtureIPAddrProbe,
	}, &log)

	if _, err := client.DeviceWiFiIP(context.Background()); err != nil {
		t.Fatalf("DeviceWiFiIP: %v", err)
	}

	wantCommands := []string{
		`ip route | awk '/wlan/ {print $9; exit}'`,
		`ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1`,
	}
	if len(log) != len(wantCommands) {
		t.Fatalf("want %d invocations, got %d", len(wantCommands), len(log))
	}
	for i, want := range wantCommands {
		argv := log[i].argv
		// [adb, -s, serial, shell, <command>] — the command must survive as ONE
		// element or the device's sh never sees the pipes and the awk program.
		if len(argv) != 5 {
			t.Fatalf("probe %d argv = %v, want 5 elements", i, argv)
		}
		if argv[3] != "shell" {
			t.Errorf("probe %d argv[3] = %q, want \"shell\"", i, argv[3])
		}
		if argv[4] != want {
			t.Errorf("probe %d command =\n%q\nwant\n%q", i, argv[4], want)
		}
	}
}

func TestWiFiDeviceIPFallsBackToTheInterface(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"empty", wifiFixtureNoInterface},
		{"awk picked the wrong column", wifiFixtureIPRouteRaw},
		{"unfiltered ip addr", wifiFixtureIPAddrRaw},
		{"unfiltered ifconfig", wifiFixtureIfconfigRaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log []wifiRecordedRun
			client := wifiScriptedClient(t, map[string]string{
				`ip route | awk '/wlan/ {print $9; exit}'`:                                        tc.first,
				`ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1`: wifiFixtureIPAddrProbe,
			}, &log)

			ip, err := client.DeviceWiFiIP(context.Background())
			if err != nil {
				t.Fatalf("DeviceWiFiIP: %v", err)
			}
			if ip != "192.168.8.174" {
				t.Errorf("ip = %q, want 192.168.8.174", ip)
			}
			if len(log) != 2 {
				t.Errorf("want both probes to run, got %d invocation(s)", len(log))
			}
		})
	}
}

func TestWiFiDeviceIPReportsWiFiOff(t *testing.T) {
	var log []wifiRecordedRun
	client := wifiScriptedClient(t, map[string]string{
		`ip route | awk '/wlan/ {print $9; exit}'`:                                        wifiFixtureNoInterface,
		`ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1`: wifiFixtureNoInterface,
	}, &log)

	_, err := wifiDeviceIPPayload(context.Background(), client)
	if err == nil {
		t.Fatal("want an error when neither probe yields an address")
	}
	// Model-visible string; byte-identical to the Python.
	const want = "Could not detect device Wi-Fi IP. Is Wi-Fi connected?"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if !adb.IsError(err) {
		t.Errorf("error should be an adb.Error so the tool reports {ok:false,error}: %T", err)
	}
	// And the tool turns it into the failure payload rather than an MCP error.
	res, out, jerr := JSON(wifiDeviceIPPayload(context.Background(), client))
	if jerr != nil {
		t.Fatalf("JSON returned a Go error: %v", jerr)
	}
	if res.IsError {
		t.Error("phone_get_device_ip must report isError:false; failure lives in the payload")
	}
	if !strings.Contains(out.Result, `"ok": false`) || !strings.Contains(out.Result, want) {
		t.Errorf("failure payload = %s", out.Result)
	}
}

func TestWiFiDeviceIPStopsOnAShellFailure(t *testing.T) {
	// AdbClient.shell raises on a non-zero exit, so probe 1 failing aborts —
	// it does not fall through to probe 2.
	var seen int
	client := adb.NewWithRunner(wifiTestSerial, "/fake/adb",
		adb.RunnerFunc(func(_ context.Context, _ []string, _ time.Duration) (adb.Output, error) {
			seen++
			return adb.Output{Stderr: []byte("device offline\r\n"), ExitCode: 1}, nil
		}))

	_, err := client.DeviceWiFiIP(context.Background())
	if err == nil {
		t.Fatal("want the shell failure to surface")
	}
	if seen != 1 {
		t.Errorf("want the second probe to be skipped, got %d invocations", seen)
	}
	if !strings.Contains(err.Error(), "device offline") {
		t.Errorf("error = %q, want it to carry adb's stderr", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Payload shapes. Key order is contract: Go maps sort, Python dicts do not.
// ---------------------------------------------------------------------------

// wifiFakeClient answers at the wifiClient seam, for the payload assertions that
// have nothing to do with argv construction.
type wifiFakeClient struct {
	serial     string
	ensureErr  error
	tcpipOut   string
	tcpipErr   error
	tcpipPort  int
	ipOut      string
	ipErr      error
	connectOut string
	connectErr error
	connectArg struct {
		host string
		port int
	}
	disconnectOut  string
	disconnectErr  error
	disconnectHost string
	calls          []string
}

func (c *wifiFakeClient) EnsureDevice(context.Context) (string, error) {
	c.calls = append(c.calls, "EnsureDevice")
	return c.serial, c.ensureErr
}

func (c *wifiFakeClient) EnableTCPIP(_ context.Context, port int) (string, error) {
	c.calls = append(c.calls, "EnableTCPIP")
	c.tcpipPort = port
	return c.tcpipOut, c.tcpipErr
}

func (c *wifiFakeClient) DeviceWiFiIP(context.Context) (string, error) {
	c.calls = append(c.calls, "DeviceWiFiIP")
	return c.ipOut, c.ipErr
}

func (c *wifiFakeClient) ConnectWiFi(_ context.Context, host string, port int) (string, error) {
	c.calls = append(c.calls, "ConnectWiFi")
	c.connectArg.host, c.connectArg.port = host, port
	return c.connectOut, c.connectErr
}

func (c *wifiFakeClient) DisconnectWiFi(_ context.Context, host string) (string, error) {
	c.calls = append(c.calls, "DisconnectWiFi")
	c.disconnectHost = host
	return c.disconnectOut, c.disconnectErr
}

func TestWiFiEnableAdbPayload(t *testing.T) {
	client := &wifiFakeClient{serial: wifiTestSerial, tcpipOut: "restarting in TCP mode port: 5555"}

	payload, err := wifiEnableAdbPayload(context.Background(), client, 5555)
	if err != nil {
		t.Fatalf("wifiEnableAdbPayload: %v", err)
	}
	want := "{\n" +
		"  \"ok\": true,\n" +
		"  \"action\": \"enable_tcpip\",\n" +
		"  \"port\": 5555,\n" +
		"  \"output\": \"restarting in TCP mode port: 5555\",\n" +
		"  \"serial\": \"2f019965\"\n" +
		"}"
	if got := jsonresult.Text(payload); got != want {
		t.Errorf("payload =\n%s\nwant\n%s", got, want)
	}
	if client.tcpipPort != 5555 {
		t.Errorf("port = %d, want 5555", client.tcpipPort)
	}
	// The device must be resolved before the host command runs.
	if strings.Join(client.calls, ",") != "EnsureDevice,EnableTCPIP" {
		t.Errorf("calls = %v", client.calls)
	}
}

func TestWiFiEnableAdbIssuesTheHostCommand(t *testing.T) {
	// The Python ran `adb shell tcpip <port>`, which exits 127 on every device
	// ("/system/bin/sh: tcpip: inaccessible or not found"). This must be the
	// host-side command, and the trimmed confirmation must reach "output".
	var log []wifiRecordedRun
	client := wifiScriptedClient(t, map[string]string{"5555": wifiFixtureTCPIPOutput}, &log)

	payload, err := wifiEnableAdbPayload(context.Background(), client, 5555)
	if err != nil {
		t.Fatalf("wifiEnableAdbPayload: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("want 1 invocation, got %d", len(log))
	}
	argv := log[0].argv
	if len(argv) != 5 || argv[3] != "tcpip" || argv[4] != "5555" {
		t.Errorf("argv = %v, want [adb -s <serial> tcpip 5555]", argv)
	}
	for _, arg := range argv {
		if arg == "shell" {
			t.Error("tcpip is a host command; it must not be sent through adb shell")
		}
	}
	if out, _ := payload.Get("output"); out != "restarting in TCP mode port: 5555" {
		t.Errorf("output = %v, want the CRLF-normalised, trimmed confirmation", out)
	}
}

func TestWiFiEnableAdbHonoursACustomPort(t *testing.T) {
	client := &wifiFakeClient{serial: wifiTestSerial, tcpipOut: "restarting in TCP mode port: 5037"}
	payload, err := wifiEnableAdbPayload(context.Background(), client, 5037)
	if err != nil {
		t.Fatalf("wifiEnableAdbPayload: %v", err)
	}
	if client.tcpipPort != 5037 {
		t.Errorf("port passed through = %d, want 5037", client.tcpipPort)
	}
	if !strings.Contains(jsonresult.Text(payload), "\"port\": 5037") {
		t.Errorf("payload did not echo the port: %s", jsonresult.Text(payload))
	}
}

func TestWiFiEnableAdbRequiresADevice(t *testing.T) {
	client := &wifiFakeClient{ensureErr: &adb.Error{Msg: "No Android device connected. Plug in USB or run adb connect."}}
	if _, err := wifiEnableAdbPayload(context.Background(), client, 5555); err == nil {
		t.Fatal("want the EnsureDevice error")
	}
	for _, call := range client.calls {
		if call == "EnableTCPIP" {
			t.Error("tcpip must not run when no device is selected")
		}
	}
}

func TestWiFiDeviceIPPayloadRequiresADevice(t *testing.T) {
	client := &wifiFakeClient{ensureErr: &adb.Error{Msg: "No Android device connected. Plug in USB or run adb connect."}}
	if _, err := wifiDeviceIPPayload(context.Background(), client); err == nil {
		t.Fatal("want the EnsureDevice error")
	}
	for _, call := range client.calls {
		if call == "DeviceWiFiIP" {
			t.Error("the probes must not run when no device is selected")
		}
	}
}

func TestWiFiConnectPayload(t *testing.T) {
	client := &wifiFakeClient{connectOut: "connected to 192.168.8.174:5555"}

	payload, err := wifiConnectPayload(context.Background(), client, "192.168.8.174", 5555)
	if err != nil {
		t.Fatalf("wifiConnectPayload: %v", err)
	}
	want := "{\n" +
		"  \"ok\": true,\n" +
		"  \"action\": \"connect_wifi\",\n" +
		"  \"target\": \"192.168.8.174:5555\",\n" +
		"  \"output\": \"connected to 192.168.8.174:5555\"\n" +
		"}"
	if got := jsonresult.Text(payload); got != want {
		t.Errorf("payload =\n%s\nwant\n%s", got, want)
	}
	// No serial key, and no EnsureDevice: connecting is what you do when there
	// is no device yet.
	if payload.Has("serial") {
		t.Error("phone_connect_wifi must not emit a serial key")
	}
	if strings.Join(client.calls, ",") != "ConnectWiFi" {
		t.Errorf("calls = %v, want ConnectWiFi only", client.calls)
	}
}

func TestWiFiConnectReportsAFailureWithOkTrue(t *testing.T) {
	// adb exits 0 for "failed to connect", so ok stays true and the outcome is
	// only in output. Replicated deliberately: no success parsing.
	client := &wifiFakeClient{connectOut: "failed to connect to '192.168.8.9:5555': Connection refused"}
	payload, err := wifiConnectPayload(context.Background(), client, "192.168.8.9", 5555)
	if err != nil {
		t.Fatalf("wifiConnectPayload: %v", err)
	}
	if ok, _ := payload.Get("ok"); ok != true {
		t.Errorf("ok = %v, want true even on a failed connect", ok)
	}
	if out, _ := payload.Get("output"); !strings.Contains(out.(string), "failed to connect") {
		t.Errorf("output = %v", out)
	}
}

func TestWiFiConnectDoesNotTrimTheHost(t *testing.T) {
	// phone_connect_wifi does NOT strip; only scrcpymac_ui_connect_wifi does.
	client := &wifiFakeClient{connectOut: ""}
	payload, err := wifiConnectPayload(context.Background(), client, "  192.168.8.174 ", 5555)
	if err != nil {
		t.Fatalf("wifiConnectPayload: %v", err)
	}
	if client.connectArg.host != "  192.168.8.174 " {
		t.Errorf("host passed to adb = %q, want it untrimmed", client.connectArg.host)
	}
	if target, _ := payload.Get("target"); target != "  192.168.8.174 :5555" {
		t.Errorf("target = %v, want the untrimmed host with the port appended", target)
	}
}

func TestWiFiConnectTarget(t *testing.T) {
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{"192.168.8.174", 5555, "192.168.8.174:5555"},
		{"192.168.8.174", 5037, "192.168.8.174:5037"},
		{"192.168.8.174:5555", 5037, "192.168.8.174:5555"}, // an explicit port wins
		{"phone.local", 5555, "phone.local:5555"},
		{"", 5555, ":5555"},
	} {
		if got := wifiConnectTarget(tc.host, tc.port); got != tc.want {
			t.Errorf("wifiConnectTarget(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}

func TestWiFiDisconnectPayload(t *testing.T) {
	client := &wifiFakeClient{disconnectOut: "disconnected 192.168.8.174:5555"}

	payload, err := wifiDisconnectPayload(context.Background(), client, "192.168.8.174")
	if err != nil {
		t.Fatalf("wifiDisconnectPayload: %v", err)
	}
	want := "{\n" +
		"  \"ok\": true,\n" +
		"  \"action\": \"disconnect_wifi\",\n" +
		"  \"output\": \"disconnected 192.168.8.174:5555\"\n" +
		"}"
	if got := jsonresult.Text(payload); got != want {
		t.Errorf("payload =\n%s\nwant\n%s", got, want)
	}
	if payload.Has("target") || payload.Has("serial") {
		t.Errorf("phone_disconnect_wifi emits neither target nor serial: %v", payload.Keys())
	}
	if client.disconnectHost != "192.168.8.174" {
		t.Errorf("host = %q", client.disconnectHost)
	}
}

func TestWiFiDisconnectAllOnAnEmptyHost(t *testing.T) {
	client := &wifiFakeClient{disconnectOut: "disconnected everything"}
	payload, err := wifiDisconnectPayload(context.Background(), client, "")
	if err != nil {
		t.Fatalf("wifiDisconnectPayload: %v", err)
	}
	if client.disconnectHost != "" {
		t.Errorf("host = %q, want empty so adb disconnects all", client.disconnectHost)
	}
	if out, _ := payload.Get("output"); out != "disconnected everything" {
		t.Errorf("output = %v", out)
	}
}

func TestWiFiDisconnectAppendsThePortWithNoPortParameter(t *testing.T) {
	// AdbClient.disconnect_wifi hardcodes 5555 and the tool has no port
	// parameter; assert the shared client keeps doing that.
	var log []wifiRecordedRun
	client := wifiScriptedClient(t, map[string]string{
		"192.168.8.174:5555": "disconnected 192.168.8.174:5555",
	}, &log)

	if _, err := wifiDisconnectPayload(context.Background(), client, "192.168.8.174"); err != nil {
		t.Fatalf("wifiDisconnectPayload: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("want 1 invocation, got %d", len(log))
	}
	argv := log[0].argv
	if len(argv) != 5 || argv[3] != "disconnect" || argv[4] != "192.168.8.174:5555" {
		t.Errorf("argv = %v, want [... disconnect 192.168.8.174:5555]", argv)
	}
}

func TestWiFiDisconnectAllPassesNoTarget(t *testing.T) {
	var log []wifiRecordedRun
	client := wifiScriptedClient(t, map[string]string{"disconnect": ""}, &log)

	if _, err := wifiDisconnectPayload(context.Background(), client, ""); err != nil {
		t.Fatalf("wifiDisconnectPayload: %v", err)
	}
	argv := log[0].argv
	if len(argv) != 4 || argv[3] != "disconnect" {
		t.Errorf("argv = %v, want a bare disconnect", argv)
	}
}

func TestWiFiPayloadErrorsBecomeTheFailureShape(t *testing.T) {
	boom := &adb.Error{Msg: "adb connect 1.2.3.4:5555 failed: no route to host"}
	for name, run := range map[string]func() (*jsonresult.Obj, error){
		"connect": func() (*jsonresult.Obj, error) {
			return wifiConnectPayload(context.Background(), &wifiFakeClient{connectErr: boom}, "1.2.3.4", 5555)
		},
		"disconnect": func() (*jsonresult.Obj, error) {
			return wifiDisconnectPayload(context.Background(), &wifiFakeClient{disconnectErr: boom}, "")
		},
		"enable": func() (*jsonresult.Obj, error) {
			return wifiEnableAdbPayload(context.Background(), &wifiFakeClient{tcpipErr: boom}, 5555)
		},
		"device_ip": func() (*jsonresult.Obj, error) {
			return wifiDeviceIPPayload(context.Background(), &wifiFakeClient{ipErr: boom})
		},
	} {
		t.Run(name, func(t *testing.T) {
			res, out, err := JSON(run())
			if err != nil {
				t.Fatalf("JSON returned a Go error: %v", err)
			}
			if res.IsError {
				t.Error("phone_* tools report isError:false; failure lives in the payload")
			}
			want := "{\n  \"ok\": false,\n  \"error\": \"" + boom.Msg + "\"\n}"
			if out.Result != want {
				t.Errorf("payload =\n%s\nwant\n%s", out.Result, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The registered surface
// ---------------------------------------------------------------------------

func TestWiFiToolSurface(t *testing.T) {
	tools := wrListTools(t, "phone-wifi", registerPhoneWiFi)

	want := map[string]string{
		"phone_enable_wifi_adb": "Enable TCP/IP adb on a USB-connected device (required before Wi-Fi connect).",
		"phone_get_device_ip":   "Get the device's Wi-Fi IP address (for wireless adb).",
		"phone_connect_wifi":    "Connect to a device over Wi-Fi adb. Example host: 192.168.1.100.",
		"phone_disconnect_wifi": "Disconnect a Wi-Fi adb session. Leave host empty to disconnect all.",
	}
	if len(tools) != len(want) {
		t.Fatalf("registered %d tools, want %d", len(tools), len(want))
	}
	for name, description := range want {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("%s was not registered", name)
		}
		if tool.Description != description {
			t.Errorf("%s description = %q", name, tool.Description)
		}
		// phone_* tools carry no annotations and no _meta: they are plain
		// model-visible tools.
		if tool.Annotations != nil {
			t.Errorf("%s must not declare annotations", name)
		}
		if len(tool.Meta) != 0 {
			t.Errorf("%s must not declare _meta, got %v", name, tool.Meta)
		}
		// Shape A: FastMCP synthesised {"result": string} for every `-> str` tool.
		wrAssertSchema(t, name+" outputSchema", tool.OutputSchema, `{
			"type": "object",
			"title": "`+name+`Output",
			"properties": {"result": {"type": "string", "title": "Result"}},
			"required": ["result"]
		}`)
	}

	// The input schemas, verbatim from docs/contract.json.
	wrAssertSchema(t, "phone_enable_wifi_adb", tools["phone_enable_wifi_adb"].InputSchema, `{
		"type": "object",
		"title": "phone_enable_wifi_adbArguments",
		"properties": {"port": {"type": "integer", "title": "Port", "default": 5555}}
	}`)

	wrAssertSchema(t, "phone_get_device_ip", tools["phone_get_device_ip"].InputSchema, `{
		"type": "object",
		"title": "phone_get_device_ipArguments",
		"properties": {}
	}`)

	wrAssertSchema(t, "phone_connect_wifi", tools["phone_connect_wifi"].InputSchema, `{
		"type": "object",
		"title": "phone_connect_wifiArguments",
		"properties": {
			"host": {"type": "string", "title": "Host"},
			"port": {"type": "integer", "title": "Port", "default": 5555}
		},
		"required": ["host"]
	}`)

	wrAssertSchema(t, "phone_disconnect_wifi", tools["phone_disconnect_wifi"].InputSchema, `{
		"type": "object",
		"title": "phone_disconnect_wifiArguments",
		"properties": {"host": {"type": "string", "title": "Host", "default": ""}}
	}`)
}

// ---------------------------------------------------------------------------
// Test helpers owned by the wifi/recipes group. Prefixed wr* so a second file
// in this package can declare its own without colliding.
// ---------------------------------------------------------------------------

// wrListTools applies one registrar to an isolated server and returns what
// tools/list reports. It goes through a real client session, so an AddTool panic
// (nil or non-object schema) fails the test instead of the shipped binary.
func wrListTools(t *testing.T, name string, apply mcpserver.ToolRegistrar) map[string]*mcp.Tool {
	t.Helper()

	registry := mcpserver.NewRegistry()
	registry.Add(mcpserver.Registration{Name: name, Order: 1, Apply: apply})

	server, err := mcpserver.New(t.Context(), mcpserver.Options{Registry: registry})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCP.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "tools-test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

// wrAssertSchema compares a tool schema, as it comes back over the wire, with the
// JSON the Python emitted. Tool.InputSchema/OutputSchema are `any` in the SDK, so
// both sides are normalised through encoding/json and compared structurally —
// which is also what makes an accidental extra key (or a lost default) a failure.
func wrAssertSchema(t *testing.T, what string, got any, wantJSON string) {
	t.Helper()
	gotMap := wrNormalizeJSON(t, got)
	var wantMap any
	if err := json.Unmarshal([]byte(wantJSON), &wantMap); err != nil {
		t.Fatalf("%s: bad expectation: %v", what, err)
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		gotText, _ := json.MarshalIndent(gotMap, "", "  ")
		wantText, _ := json.MarshalIndent(wantMap, "", "  ")
		t.Errorf("%s =\n%s\nwant\n%s", what, gotText, wantText)
	}
}

func wrNormalizeJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}
