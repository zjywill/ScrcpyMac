package adb

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// The rest of the adb.py client surface: the two capture probes, the transport
// plumbing the scrcpy runtime needs (push, forward, forward --remove) and the
// four Wi-Fi calls.
//
// Every device-side command here is one argv element containing device shell
// syntax — pipes, &&, ;, redirection — parsed by /system/bin/sh on the phone.
// Splitting any of them host-side silently breaks them.

// UITreeRemotePath is where uiautomator writes its dump before it is cat'ed
// back and deleted, in a single round trip.
const UITreeRemotePath = "/sdcard/window_dump.xml"

// DefaultWiFiPort is the port `adb connect`/`adb disconnect` assume when the
// caller gives a bare host, and the port `adb tcpip` switches the device to.
const DefaultWiFiPort = 5555

// ScreenshotPNG captures the screen as PNG bytes.
//
// It must go through RunBytes: CRLF normalisation would corrupt the PNG. The
// 30s timeout is Python's explicit override of the 60s default.
func (c *Client) ScreenshotPNG(ctx context.Context) ([]byte, error) {
	return c.RunBytes(ctx, ShortTimeout, "exec-out", "screencap", "-p")
}

// UITreeXML dumps the accessibility tree.
//
// One round trip by design. The `; rm -f` always runs, so a failed dump still
// exits 0 with empty stdout rather than raising — the caller decides what an
// empty tree means.
func (c *Client) UITreeXML(ctx context.Context) (string, error) {
	return c.ShellTimeout(ctx, ShortTimeout,
		"uiautomator dump "+UITreeRemotePath+" >/dev/null 2>&1 && cat "+UITreeRemotePath+
			"; rm -f "+UITreeRemotePath)
}

// Push copies a local file to the device and returns adb's stripped stdout
// (a transfer-rate line). The scrcpy runtime pushes the ~92 KB scrcpy-server jar
// through it on every stream start.
func (c *Client) Push(ctx context.Context, local, remote string) (string, error) {
	res, err := c.RunTimeout(ctx, DefaultTimeout, "push", local, remote)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Forward creates a host→device forward and returns adb's stripped stdout.
//
// With local == "tcp:0" adb allocates a free host port and prints it; feed the
// result to ParseForwardPort. Every forward this creates must be released with
// RemoveForward, including on an abnormal shutdown — a leaked
// localabstract:scrcpy_* forward outlives the process and confuses the next run.
func (c *Client) Forward(ctx context.Context, local, remote string) (string, error) {
	res, err := c.Run(ctx, "forward", local, remote)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// ParseForwardPort reads the host port adb allocated for `forward tcp:0`.
//
// It reports false rather than formatting a message, because the caller owns the
// wording: the runtime raises ScrcpyRuntimeError with the raw stdout in it.
func ParseForwardPort(stdout string) (int, bool) {
	port, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// RemoveForward releases a forward. It is teardown, so it is written to be
// impossible to get wrong:
//
//   - the caller's context is stripped of cancellation, because cleanups run
//     *after* the server context is cancelled and would otherwise be killed
//     before adb could do anything;
//   - a 10s timeout bounds it so shutdown cannot hang;
//   - a non-zero exit is not an error. Removing a forward another process
//     already released is expected and benign.
//
// The returned error only ever describes a failure to run adb at all, and even
// that is for logging — never propagate it out of a cleanup path.
func (c *Client) RemoveForward(ctx context.Context, local string) error {
	_, err := c.RunNoCheck(context.WithoutCancel(ctx), TeardownTimeout, "forward", "--remove", local)
	return err
}

// ConnectWiFi runs `adb connect`. A host that already carries a port is used
// verbatim; otherwise port is appended.
func (c *Client) ConnectWiFi(ctx context.Context, host string, port int) (string, error) {
	target := host
	if !strings.Contains(host, ":") {
		target = host + ":" + strconv.Itoa(port)
	}
	res, err := c.Run(ctx, "connect", target)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// DisconnectWiFi runs `adb disconnect`, optionally for one host. An empty host
// disconnects every TCP/IP device.
//
// Note the asymmetry with ConnectWiFi, which is contract: there is no port
// parameter here, so a bare host always gets ":5555" appended even if it was
// connected on another port.
func (c *Client) DisconnectWiFi(ctx context.Context, host string) (string, error) {
	args := []string{"disconnect"}
	if host != "" {
		if strings.Contains(host, ":") {
			args = append(args, host)
		} else {
			args = append(args, host+":"+strconv.Itoa(DefaultWiFiPort))
		}
	}
	res, err := c.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// EnableTCPIP restarts adbd on the device in TCP/IP mode and returns adb's
// stripped stdout, e.g. "restarting in TCP mode port: 5555".
//
// DEVIATION (docs/spec-adb-doctor.md §1.10, option b): the Python ran
// `adb shell tcpip <port>`, which invokes a device binary that does not exist —
// verified live on the OnePlus 6 as exit 127,
// "/system/bin/sh: tcpip: inaccessible or not found". `tcpip` is a *host*-side
// adb command, so phone_enable_wifi_adb has never once succeeded. This runs the
// host command instead. No result key moves; only `output` changes, from an
// error string to adb's confirmation.
func (c *Client) EnableTCPIP(ctx context.Context, port int) (string, error) {
	res, err := c.Run(ctx, "tcpip", strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// dottedQuad is Python's r"^\d+\.\d+\.\d+\.\d+$" — deliberately not a real IPv4
// validator (999.999.999.999 passes). Preserved as-is.
var dottedQuad = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

// DeviceWiFiIP reports the device's Wi-Fi address, trying the routing table
// first and the interface second. Both commands rely on device-side pipes and
// awk quoting and are passed through verbatim.
func (c *Client) DeviceWiFiIP(ctx context.Context) (string, error) {
	probes := []string{
		"ip route | awk '/wlan/ {print $9; exit}'",
		"ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1",
	}
	for _, probe := range probes {
		output, err := c.Shell(ctx, probe)
		if err != nil {
			return "", err
		}
		if dottedQuad.MatchString(output) {
			return output, nil
		}
	}
	return "", &Error{Msg: "Could not detect device Wi-Fi IP. Is Wi-Fi connected?"}
}
