package adb

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScreenshotPNGArgvAndTimeout(t *testing.T) {
	// A PNG containing a CRLF pair: RunBytes must hand it back untouched.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\r\n")
	fake := newRecorder(func(int, []string) (Output, error) {
		return Output{Stdout: png}, nil
	})
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	got, err := client.ScreenshotPNG(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(png) {
		t.Errorf("ScreenshotPNG = %q, want the bytes untouched", got)
	}
	fake.wantArgv(t, 0, "/fake/adb", "-s", "2f019965", "exec-out", "screencap", "-p")
	fake.wantTimeout(t, 0, ShortTimeout)
}

func TestUITreeXMLIsOneRoundTrip(t *testing.T) {
	xml := "<?xml version='1.0' encoding='UTF-8'?><hierarchy rotation=\"0\" />\r\n"
	fake := newRecorder(reply(xml))
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	got, err := client.UITreeXML(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "<?xml version='1.0' encoding='UTF-8'?><hierarchy rotation=\"0\" />" {
		t.Errorf("UITreeXML = %q (must be CRLF-normalised and stripped)", got)
	}
	fake.wantCalls(t, 1)
	fake.wantTimeout(t, 0, ShortTimeout)
	fake.wantOneShellElement(t, 0,
		"uiautomator dump /sdcard/window_dump.xml >/dev/null 2>&1 && cat /sdcard/window_dump.xml; rm -f /sdcard/window_dump.xml")
}

func TestPushArgvAndTimeout(t *testing.T) {
	fake := newRecorder(reply("/path/jar: 1 file pushed. 4.2 MB/s (92160 bytes in 0.021s)\n"))
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	out, err := client.Push(context.Background(), "/local/scrcpy-server", "/data/local/tmp/x.jar")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Error("Push must return adb's stdout, stripped")
	}
	fake.wantArgv(t, 0, "/fake/adb", "-s", "2f019965", "push", "/local/scrcpy-server", "/data/local/tmp/x.jar")
	fake.wantTimeout(t, 0, DefaultTimeout)
}

func TestForwardAndParsePort(t *testing.T) {
	fake := newRecorder(reply("41763\n"))
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	out, err := client.Forward(context.Background(), "tcp:0", "localabstract:scrcpy_0000beef")
	if err != nil {
		t.Fatal(err)
	}
	fake.wantArgv(t, 0, "/fake/adb", "-s", "2f019965", "forward", "tcp:0", "localabstract:scrcpy_0000beef")

	port, ok := ParseForwardPort(out)
	if !ok || port != 41763 {
		t.Errorf("ParseForwardPort(%q) = %d, %v", out, port, ok)
	}
}

func TestParseForwardPort(t *testing.T) {
	tests := []struct {
		in       string
		wantPort int
		wantOK   bool
	}{
		{"41763\n", 41763, true},
		{"  27196  ", 27196, true},
		{"1", 1, true},
		{"65535", 65535, true},
		{"", 0, false},
		{"\n", 0, false},
		{"error: cannot bind listener", 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"65536", 0, false},
		{"41763 extra", 0, false},
	}
	for _, tt := range tests {
		port, ok := ParseForwardPort(tt.in)
		if port != tt.wantPort || ok != tt.wantOK {
			t.Errorf("ParseForwardPort(%q) = %d, %v; want %d, %v", tt.in, port, ok, tt.wantPort, tt.wantOK)
		}
	}
}

func TestRemoveForwardSurvivesACancelledContextAndANonZeroExit(t *testing.T) {
	fake := newRecorder(func(int, []string) (Output, error) {
		// adb exits 1 when the forward is already gone — expected during a
		// concurrent teardown, and not an error.
		return Output{Stdout: []byte("error: listener 'tcp:41763' not found\n"), ExitCode: 1}, nil
	})
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	// Cleanups run after the server context is cancelled. If RemoveForward
	// propagated that cancellation, exec would kill adb before it did anything
	// and the forward would leak past process exit.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := client.RemoveForward(ctx, "tcp:41763"); err != nil {
		t.Errorf("RemoveForward must not fail on a non-zero exit: %v", err)
	}
	fake.wantArgv(t, 0, "/fake/adb", "-s", "2f019965", "forward", "--remove", "tcp:41763")
	fake.wantTimeout(t, 0, TeardownTimeout)
	if !fake.ctxLive[0] {
		t.Error("RemoveForward must strip cancellation from the context, or shutdown leaks the forward")
	}
}

func TestConnectWiFi(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		port       int
		wantTarget string
	}{
		{"bare host gets the port appended", "192.168.1.5", 5555, "192.168.1.5:5555"},
		{"non-default port", "192.168.1.5", 39755, "192.168.1.5:39755"},
		{"host:port is used verbatim", "192.168.1.5:39755", 5555, "192.168.1.5:39755"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newRecorder(reply("connected to " + tt.wantTarget + "\n"))
			client := NewWithRunner("", "/fake/adb", fake)

			out, err := client.ConnectWiFi(context.Background(), tt.host, tt.port)
			if err != nil {
				t.Fatal(err)
			}
			if out != "connected to "+tt.wantTarget {
				t.Errorf("output = %q", out)
			}
			fake.wantArgv(t, 0, "/fake/adb", "connect", tt.wantTarget)
		})
	}
}

func TestDisconnectWiFi(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		wantArgs []string
	}{
		{"no host disconnects everything", "", []string{"/fake/adb", "disconnect"}},
		// No port parameter exists, so a bare host always gets :5555 — even if
		// it was connected on another port. Contract, not an oversight.
		{"bare host gets :5555", "192.168.1.5", []string{"/fake/adb", "disconnect", "192.168.1.5:5555"}},
		{"host:port verbatim", "192.168.1.5:39755", []string{"/fake/adb", "disconnect", "192.168.1.5:39755"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newRecorder(reply("disconnected\n"))
			client := NewWithRunner("", "/fake/adb", fake)

			if _, err := client.DisconnectWiFi(context.Background(), tt.host); err != nil {
				t.Fatal(err)
			}
			fake.wantArgv(t, 0, tt.wantArgs...)
		})
	}
}

func TestEnableTCPIPUsesTheHostCommand(t *testing.T) {
	fake := newRecorder(reply("restarting in TCP mode port: 5555\n"))
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	out, err := client.EnableTCPIP(context.Background(), 5555)
	if err != nil {
		t.Fatal(err)
	}
	if out != "restarting in TCP mode port: 5555" {
		t.Errorf("output = %q", out)
	}
	// DEVIATION: the Python ran `adb shell tcpip 5555`, which is exit 127 on a
	// real device ("/system/bin/sh: tcpip: inaccessible or not found").
	fake.wantArgv(t, 0, "/fake/adb", "-s", "2f019965", "tcpip", "5555")
	mustNotContain(t, fake.argv[0][3], "shell", "tcpip is a host command, not a device binary")
}

func TestDeviceWiFiIP(t *testing.T) {
	const routeProbe = "ip route | awk '/wlan/ {print $9; exit}'"
	const addrProbe = "ip -f inet addr show wlan0 2>/dev/null | awk '/inet / {print $2}' | cut -d/ -f1"

	t.Run("first probe wins", func(t *testing.T) {
		fake := newRecorder(reply("192.168.8.174\n"))
		client := NewWithRunner("2f019965", "/fake/adb", fake)

		ip, err := client.DeviceWiFiIP(context.Background())
		if err != nil || ip != "192.168.8.174" {
			t.Fatalf("DeviceWiFiIP = %q, %v", ip, err)
		}
		fake.wantCalls(t, 1)
		fake.wantOneShellElement(t, 0, routeProbe)
	})

	t.Run("falls back to the interface probe", func(t *testing.T) {
		fake := newRecorder(replies("\n", "10.0.0.42\n"))
		client := NewWithRunner("2f019965", "/fake/adb", fake)

		ip, err := client.DeviceWiFiIP(context.Background())
		if err != nil || ip != "10.0.0.42" {
			t.Fatalf("DeviceWiFiIP = %q, %v", ip, err)
		}
		fake.wantCalls(t, 2)
		fake.wantOneShellElement(t, 1, addrProbe)
	})

	t.Run("neither probe matches", func(t *testing.T) {
		fake := newRecorder(reply("unreachable\n"))
		client := NewWithRunner("2f019965", "/fake/adb", fake)

		_, err := client.DeviceWiFiIP(context.Background())
		if err == nil || err.Error() != "Could not detect device Wi-Fi IP. Is Wi-Fi connected?" {
			t.Fatalf("error = %v", err)
		}
		if !IsError(err) {
			t.Error("must be an adb.Error so the tool layer reports {\"ok\": false, \"error\": ...}")
		}
	})

	t.Run("octet ranges are deliberately not validated", func(t *testing.T) {
		fake := newRecorder(reply("999.999.999.999\n"))
		client := NewWithRunner("", "/fake/adb", fake)

		ip, err := client.DeviceWiFiIP(context.Background())
		if err != nil || ip != "999.999.999.999" {
			t.Fatalf("DeviceWiFiIP = %q, %v", ip, err)
		}
	})
}

func TestScreenSizeAndCurrentAppArgv(t *testing.T) {
	fake := newRecorder(replies(
		"Physical size: 1080x2280\r\n",
		"  mCurrentFocus=Window{d2a4d5 u0 com.tencent.mm/com.tencent.mm.ui.LauncherUI}\r\n",
	))
	client := NewWithRunner("2f019965", "/fake/adb", fake)

	w, h, err := client.ScreenSize(context.Background())
	if err != nil || w != 1080 || h != 2280 {
		t.Fatalf("ScreenSize = %dx%d, %v", w, h, err)
	}
	fake.wantOneShellElement(t, 0, screenSizeCommand)

	app, err := client.CurrentApp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if app.Package != "com.tencent.mm" || app.Activity != "com.tencent.mm.ui.LauncherUI" {
		t.Errorf("CurrentApp = %#v", app)
	}
	fake.wantOneShellElement(t, 1, "dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -1")
}

func TestRunnerErrorBecomesAnAdbError(t *testing.T) {
	fake := newRecorder(func(int, []string) (Output, error) {
		return Output{ExitCode: -1}, errors.New("fork/exec /fake/adb: no such file or directory")
	})
	client := NewWithRunner("", "/fake/adb", fake)

	_, err := client.Run(context.Background(), "devices", "-l")
	if err == nil {
		t.Fatal("want an error")
	}
	if err.Error() != "adb devices -l failed: fork/exec /fake/adb: no such file or directory" {
		t.Errorf("error = %q", err.Error())
	}
	if !IsError(err) {
		t.Error("a start failure must still be an adb.Error")
	}
}

func TestTimeoutAndCancellationMessages(t *testing.T) {
	timedOut := newRecorder(func(int, []string) (Output, error) {
		return Output{ExitCode: -1, TimedOut: true}, nil
	})
	client := NewWithRunner("SERIAL", "/fake/adb", timedOut)
	_, err := client.RunTimeout(context.Background(), 30*time.Millisecond, "shell", "wm size")
	if err == nil || err.Error() != "adb timed out: /fake/adb -s SERIAL shell wm size" {
		t.Errorf("timeout message = %v", err)
	}

	cancelled := newRecorder(func(int, []string) (Output, error) {
		return Output{ExitCode: -1, Cancelled: true}, nil
	})
	client = NewWithRunner("SERIAL", "/fake/adb", cancelled)
	_, err = client.Run(context.Background(), "shell", "wm size")
	if err == nil || err.Error() != "adb cancelled: /fake/adb -s SERIAL shell wm size" {
		t.Errorf("cancellation message = %v", err)
	}
}

func TestFailureDetailReplacesInvalidUTF8(t *testing.T) {
	// run_bytes captures undecoded bytes; Python's _as_text decodes them with
	// errors="replace" so a screencap failure still yields a printable message.
	fake := newRecorder(func(int, []string) (Output, error) {
		return Output{Stderr: []byte("bad \xff\xfe byte"), ExitCode: 1}, nil
	})
	client := NewWithRunner("", "/fake/adb", fake)

	_, err := client.RunBytes(context.Background(), ShortTimeout, "exec-out", "screencap", "-p")
	if err == nil {
		t.Fatal("want an error")
	}
	// Python replaces each invalid byte, Go's ToValidUTF8 each invalid run, so
	// the count of U+FFFD can differ in already-garbled output. What matters is
	// that the message is printable UTF-8 and safe to put in JSON.
	if err.Error() != "adb exec-out screencap -p failed: bad � byte" {
		t.Errorf("error = %q", err.Error())
	}
}
