package scrcpy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLiveStream drives a real device end to end: it starts scrcpy-server,
// attaches a WebSocket client to the loopback relay, generates sustained motion
// with `adb shell input swipe`, measures the packet rate, and then asserts that
// stopping leaves no forward and no device-side process behind.
//
// It is skipped unless PHONE_AGENT_LIVE_SERIAL names an attached device, so the
// default `go test ./...` needs no hardware. Run it with:
//
//	PHONE_AGENT_ROOT=.. PHONE_AGENT_LIVE_SERIAL=<serial> \
//	  go test ./internal/scrcpy -run TestLiveStream -v -timeout 120s
//
// It only ever touches the forward it allocated itself, so it is safe to run
// while another scrcpy session is attached to the same device.
func TestLiveStream(t *testing.T) {
	serial := strings.TrimSpace(os.Getenv("PHONE_AGENT_LIVE_SERIAL"))
	if serial == "" {
		t.Skip("set PHONE_AGENT_LIVE_SERIAL to run against a real device")
	}
	adbPath := adbBinary()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := New(Options{Log: logger, Context: context.Background()})
	t.Cleanup(func() { _ = r.Close() })

	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	t.Logf("loopback listening on 127.0.0.1:%d", port)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	status, err := r.Start(ctx, serial, 60, 50)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { r.Stop() })

	forwardPort, scid := r.sessionIdentity()
	streamURL, _ := status.Get("streamUrl")
	t.Logf("stream up: %v", streamURL)
	t.Logf("status: %v", status.Keys())

	// Attach a client exactly the way the widget does.
	token := r.currentToken()
	conn, resp, br := dialStream(t, port, "/stream?token="+token, nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade returned %d", resp.StatusCode)
	}
	if err := conn.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	opcode, payload := readServerFrame(t, br)
	if opcode != opText {
		t.Fatalf("first frame opcode = %#x, want the ready message", opcode)
	}
	var ready map[string]any
	if err := json.Unmarshal(payload, &ready); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Logf("ready: state=%v frame=%vx%v codec=%v",
		ready["state"], ready["frameWidth"], ready["frameHeight"], ready["codec"])

	// Sustained motion for the whole measurement window.
	swipes, stopSwipes := context.WithCancel(context.Background())
	defer stopSwipes()
	go func() {
		for swipes.Err() == nil {
			cmd := exec.CommandContext(swipes, adbPath, "-s", serial,
				"shell", "input swipe 540 1800 540 400 200")
			_ = cmd.Run()
		}
	}()

	const window = 8 * time.Second
	startedReading := time.Now()
	deadline := startedReading.Add(window)
	var packets, keyFrames, configs, bytesRead int
	firstFrameAt := time.Time{}
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("read deadline: %v", err)
		}
		opcode, payload := readServerFrame(t, br)
		if opcode != opBinary {
			continue
		}
		isConfig, isKey, _, body, err := DecodeStreamPacket(payload)
		if err != nil {
			t.Fatalf("the widget would reject this packet: %v", err)
		}
		if firstFrameAt.IsZero() {
			firstFrameAt = time.Now()
		}
		bytesRead += len(body)
		switch {
		case isConfig:
			configs++
		case isKey:
			keyFrames++
			packets++
		default:
			packets++
		}
	}
	stopSwipes()

	elapsed := window.Seconds()
	rate := float64(packets) / elapsed
	stats := r.Stats()
	diag := r.Diagnostics()
	t.Logf("client received %d frames (%d key, %d config, %.1f KB) in %.1fs = %.1f packets/s; first frame after %v",
		packets, keyFrames, configs, float64(bytesRead)/1024, elapsed, rate, firstFrameAt.Sub(startedReading).Round(time.Millisecond))
	t.Logf("pump read %d packets (%.1f/s), %d frames (%.1f/s); relay dropped %d GOPs / %d packets, max queue %d B",
		diag.Packets, diag.PacketsPerSecond, diag.Frames, diag.FramesPerSecond,
		stats.DroppedGOPs, stats.DroppedPackets, stats.MaxQueuedBytes)

	if packets == 0 {
		t.Fatal("no H.264 frames reached the client at all")
	}
	// The regression being replaced rendered at about 1 FPS. Anything below 10
	// frames per second during sustained motion means the relay is stalling.
	if rate < 10 {
		t.Fatalf("only %.1f packets/s during sustained motion; the relay is stalling", rate)
	}
	if stats.DroppedGOPs != 0 {
		t.Errorf("a loopback client should never fall behind, but %d GOPs were dropped", stats.DroppedGOPs)
	}

	_ = conn.Close()

	// Codex production sandboxes reject loopback ws:// origins. Exercise the
	// standards-compliant MCP Apps tools/call transport against the same live
	// stream and require it to stay far above the JPEG fallback rate.
	const pullWindow = 5 * time.Second
	pullStarted := time.Now()
	pullDeadline := pullStarted.Add(pullWindow)
	pullFirstFrame := time.Time{}
	pullPackets := 0
	pullConfigs := 0
	for time.Now().Before(pullDeadline) {
		batch := r.PullPackets(context.Background(), 512<<10, 250)
		offset := 0
		for offset < len(batch.Data) {
			if len(batch.Data)-offset < WSHeaderLength {
				t.Fatalf("MCP batch ended with a truncated H.264 header")
			}
			length := int(binary.BigEndian.Uint32(batch.Data[offset+10 : offset+WSHeaderLength]))
			end := offset + WSHeaderLength + length
			if end > len(batch.Data) {
				t.Fatalf("MCP batch packet length exceeds the batch")
			}
			isConfig, _, _, _, err := DecodeStreamPacket(batch.Data[offset:end])
			if err != nil {
				t.Fatalf("MCP bridge delivered an invalid packet: %v", err)
			}
			if isConfig {
				pullConfigs++
			} else {
				if pullFirstFrame.IsZero() {
					pullFirstFrame = time.Now()
				}
				pullPackets++
			}
			offset = end
		}
	}
	pullRate := float64(pullPackets) / pullWindow.Seconds()
	pullStats := r.Stats()
	pullDiag := r.Diagnostics()
	t.Logf("MCP client received %d frames (%d config) in %.1fs = %.1f packets/s; first frame after %v",
		pullPackets, pullConfigs, pullWindow.Seconds(), pullRate,
		pullFirstFrame.Sub(pullStarted).Round(time.Millisecond))
	t.Logf("MCP pump rate %.1f packets/s / %.1f frames/s; relay dropped %d GOPs / %d packets, max queue %d B",
		pullDiag.PacketsPerSecond, pullDiag.FramesPerSecond,
		pullStats.DroppedGOPs, pullStats.DroppedPackets, pullStats.MaxQueuedBytes)
	if pullPackets == 0 || pullRate < 10 {
		t.Fatalf("MCP H.264 bridge delivered only %.1f packets/s", pullRate)
	}
	if pullStats.DroppedGOPs != 0 {
		t.Errorf("MCP H.264 bridge dropped %d GOPs", pullStats.DroppedGOPs)
	}

	r.Stop()

	// Nothing may survive the stop: not the forward, not the device process.
	if forwardPort != 0 {
		out := runADB(t, adbPath, "forward", "--list")
		if strings.Contains(out, "tcp:"+strconv.Itoa(forwardPort)) {
			t.Errorf("adb forward tcp:%d survived Stop:\n%s", forwardPort, out)
		} else {
			t.Logf("forward tcp:%d released", forwardPort)
		}
	}
	// The scid is unique to this session, so this cannot trip over a scrcpy
	// someone else is running on the same device. The bracketed first character
	// keeps the grep from matching the shell command that carries it.
	probe := fmt.Sprintf("ps -A -o ARGS= 2>/dev/null | grep 'sci[d]=%08x' || true", scid)
	if out := runADB(t, adbPath, "-s", serial, "shell", probe); strings.TrimSpace(out) != "" {
		t.Errorf("scrcpy-server (scid=%08x) survived Stop on the device:\n%s", scid, out)
	} else {
		t.Logf("device-side scrcpy-server (scid=%08x) is gone", scid)
	}
}

// sessionIdentity reads the live session's forward port and scrcpy session id
// for the live test's cleanup assertions.
func (r *Runtime) sessionIdentity() (forwardPort int, scid uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == nil {
		return 0, 0
	}
	return r.sess.forwardPort, r.sess.scid
}

// currentToken exposes the stream token to the live test, which has to build the
// same URL the widget would.
func (r *Runtime) currentToken() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token
}

func adbBinary() string {
	if p := os.Getenv("ADB_PATH"); p != "" {
		return p
	}
	return "adb"
}

func runADB(t *testing.T, adbPath string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, adbPath, args...).CombinedOutput()
	if err != nil {
		t.Logf("adb %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}
