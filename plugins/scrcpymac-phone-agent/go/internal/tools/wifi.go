package tools

// Wireless adb: phone_enable_wifi_adb, phone_get_device_ip, phone_connect_wifi
// and phone_disconnect_wifi.
//
// Port of actions.PhoneActions.{enable_wifi_adb,get_device_ip,connect_wifi,
// disconnect_wifi}. The adb invocations themselves live in internal/adb
// (Client.EnableTCPIP / DeviceWiFiIP / ConnectWiFi / DisconnectWiFi), which is
// where the Python kept them too — this file is the payload layer and nothing
// more. All four tools are Shape A: the bare JSON is the text block and
// {"result": "<same text>"} is structuredContent.
//
// Four details are contract, not style:
//
//   - connect and disconnect deliberately do NOT call EnsureDevice. Connecting
//     over Wi-Fi is exactly what you do when no device is attached yet, and the
//     Python used the raw client for both.
//   - connect_wifi has no "serial" key, and disconnect_wifi has neither "serial"
//     nor "target", while the other two carry "serial".
//   - "target" is recomputed here with the same rule Client.ConnectWiFi applies
//     internally, exactly as actions.py recomputes it. The two must stay in sync.
//   - host is NOT trimmed by phone_connect_wifi. scrcpymac_ui_connect_wifi does
//     trim; that difference is real and both sides are reproduced in their own
//     tools.

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/mcpserver"
)

func init() {
	mcpserver.Register(mcpserver.Registration{
		Name:  "phone-wifi",
		Order: mcpserver.OrderPhoneTools + 60,
		Apply: registerPhoneWiFi,
	})
}

// wifiDefaultPort is the adb TCP/IP port the tools default to.
const wifiDefaultPort = adb.DefaultWiFiPort

// wifiClient is the slice of *adb.Client these tools need. An interface rather
// than the concrete type so the payload shapes are unit-testable with no adb
// binary and no device.
type wifiClient interface {
	EnsureDevice(ctx context.Context) (string, error)
	EnableTCPIP(ctx context.Context, port int) (string, error)
	DeviceWiFiIP(ctx context.Context) (string, error)
	ConnectWiFi(ctx context.Context, host string, port int) (string, error)
	DisconnectWiFi(ctx context.Context, host string) (string, error)
}

// Compile-time proof that the shared client satisfies it.
var _ wifiClient = (*adb.Client)(nil)

type wifiEnableIn struct {
	Port int `json:"port,omitempty"`
}

type wifiNoArgsIn struct{}

type wifiConnectIn struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

type wifiDisconnectIn struct {
	Host string `json:"host,omitempty"`
}

func registerPhoneWiFi(s *mcp.Server, env *mcpserver.Env) error {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_enable_wifi_adb",
		Description: "Enable TCP/IP adb on a USB-connected device (required before Wi-Fi connect).",
		InputSchema: ObjectSchema("phone_enable_wifi_adbArguments",
			Prop{Name: "port", Type: "integer", Title: "Port", Default: wifiDefaultPort},
		),
		OutputSchema: StringOutputSchema("phone_enable_wifi_adb"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in wifiEnableIn) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(wifiEnableAdbPayload(ctx, client, in.Port))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:         "phone_get_device_ip",
		Description:  "Get the device's Wi-Fi IP address (for wireless adb).",
		InputSchema:  ObjectSchema("phone_get_device_ipArguments"),
		OutputSchema: StringOutputSchema("phone_get_device_ip"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ wifiNoArgsIn) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(wifiDeviceIPPayload(ctx, client))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_connect_wifi",
		Description: "Connect to a device over Wi-Fi adb. Example host: 192.168.1.100.",
		InputSchema: ObjectSchema("phone_connect_wifiArguments",
			Prop{Name: "host", Type: "string", Title: "Host", Required: true},
			Prop{Name: "port", Type: "integer", Title: "Port", Default: wifiDefaultPort},
		),
		OutputSchema: StringOutputSchema("phone_connect_wifi"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in wifiConnectIn) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(wifiConnectPayload(ctx, client, in.Host, in.Port))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "phone_disconnect_wifi",
		Description: "Disconnect a Wi-Fi adb session. Leave host empty to disconnect all.",
		InputSchema: ObjectSchema("phone_disconnect_wifiArguments",
			Prop{Name: "host", Type: "string", Title: "Host", Default: ""},
		),
		OutputSchema: StringOutputSchema("phone_disconnect_wifi"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in wifiDisconnectIn) (*mcp.CallToolResult, StringOut, error) {
		client, err := env.ADB()
		if err != nil {
			return JSONFail(err)
		}
		return JSON(wifiDisconnectPayload(ctx, client, in.Host))
	})

	return nil
}

// wifiEnableAdbPayload restarts adbd in TCP/IP mode:
//
//	{"ok": true, "action": "enable_tcpip", "port": p, "output": out, "serial": s}
//
// The action string stays "enable_tcpip" even though the underlying invocation
// was fixed from `adb shell tcpip` to the host command `adb tcpip` (see
// Client.EnableTCPIP): no result key moves, only what "output" contains.
func wifiEnableAdbPayload(ctx context.Context, client wifiClient, port int) (*jsonresult.Obj, error) {
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	output, err := client.EnableTCPIP(ctx, port)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"action", "enable_tcpip",
		"port", port,
		"output", output,
		"serial", serial,
	), nil
}

// wifiDeviceIPPayload reports the device's Wi-Fi address:
//
//	{"ok": true, "ip": "192.168.8.174", "serial": s}
func wifiDeviceIPPayload(ctx context.Context, client wifiClient) (*jsonresult.Obj, error) {
	serial, err := client.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	ip, err := client.DeviceWiFiIP(ctx)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of("ok", true, "ip", ip, "serial", serial), nil
}

// wifiConnectPayload pairs with a device over TCP/IP:
//
//	{"ok": true, "action": "connect_wifi", "target": "host:port", "output": out}
//
// adb exits 0 even for "failed to connect to ...", so ok is true regardless and
// the real outcome is in "output". Do not add success parsing — the Python has
// none and the model reads the string.
func wifiConnectPayload(ctx context.Context, client wifiClient, host string, port int) (*jsonresult.Obj, error) {
	output, err := client.ConnectWiFi(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"action", "connect_wifi",
		"target", wifiConnectTarget(host, port),
		"output", output,
	), nil
}

// wifiDisconnectPayload drops one Wi-Fi session, or all of them when host is
// empty:
//
//	{"ok": true, "action": "disconnect_wifi", "output": out}
func wifiDisconnectPayload(ctx context.Context, client wifiClient, host string) (*jsonresult.Obj, error) {
	output, err := client.DisconnectWiFi(ctx, host)
	if err != nil {
		return nil, err
	}
	return jsonresult.Of(
		"ok", true,
		"action", "disconnect_wifi",
		"output", output,
	), nil
}

// wifiConnectTarget is `host if ":" in host else f"{host}:{port}"` — the value
// echoed back as "target". A host that already carries a port keeps it and the
// port argument is ignored, which is also what Client.ConnectWiFi does with it.
func wifiConnectTarget(host string, port int) string {
	if strings.Contains(host, ":") {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}
