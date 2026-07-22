package scrcpy

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// maskedFrame builds a client-to-server frame. RFC 6455 requires client frames
// to be masked, so this is what the widget's browser WebSocket actually sends.
func maskedFrame(t *testing.T, opcode byte, payload []byte) []byte {
	t.Helper()
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}

	frame := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}

// readServerFrame reads one unmasked server-to-client frame.
func readServerFrame(t *testing.T, br *bufio.Reader) (opcode byte, payload []byte) {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	if head[1]&0x80 != 0 {
		t.Fatal("the server must not mask its frames")
	}
	opcode = head[0] & 0x0F

	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			t.Fatalf("read 16-bit length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			t.Fatalf("read 64-bit length: %v", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return opcode, payload
}

// dialStream performs the WebSocket handshake against the loopback listener and
// returns the raw connection plus the HTTP status.
func dialStream(t *testing.T, port int, path string, headers map[string]string) (net.Conn, *http.Response, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	request := map[string]string{
		"Host":                  fmt.Sprintf("127.0.0.1:%d", port),
		"Upgrade":               "websocket",
		"Connection":            "Upgrade",
		"Sec-WebSocket-Key":     "dGhlIHNhbXBsZSBub25jZQ==",
		"Sec-WebSocket-Version": "13",
	}
	for k, v := range headers {
		if v == "" {
			delete(request, k)
			continue
		}
		request[k] = v
	}

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	for k, v := range request {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return conn, resp, br
}

func TestLoopbackUpgradeAndStream(t *testing.T) {
	r := newTestRuntime(t)
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	meta := &VideoMeta{Serial: "TESTSERIAL", DeviceName: "Test", Width: 540, Height: 1140,
		NativeWidth: 1080, NativeHeight: 2280, MaxFPS: 60, ResolutionPercent: 50}
	fakeStreaming(t, r, meta, "token-abc")

	// The relay already has a GOP recorded, so the client must be replayed it.
	r.hub.Broadcast(makePacket(0, kindConfig, 8))
	r.hub.Broadcast(makePacket(1, kindKey, 16))
	r.hub.Broadcast(makePacket(2, kindDelta, 16))

	_, resp, br := dialStream(t, port, "/stream?token=token-abc", nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	// The RFC 6455 worked example: this key must produce this accept value.
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("Sec-WebSocket-Accept = %q", got)
	}

	// 1. the ready message, carrying the status payload.
	opcode, payload := readServerFrame(t, br)
	if opcode != opText {
		t.Fatalf("first frame opcode = %#x, want text", opcode)
	}
	var ready map[string]any
	if err := json.Unmarshal(payload, &ready); err != nil {
		t.Fatalf("ready is not JSON: %v (%s)", err, payload)
	}
	if ready["type"] != "ready" || ready["state"] != StateStreaming || ready["backend"] != "plugin-h264" {
		t.Fatalf("ready payload = %v", ready)
	}
	if url, _ := ready["streamUrl"].(string); !strings.Contains(url, "token=token-abc") {
		t.Fatalf("ready.streamUrl = %q", url)
	}

	// 2. the replayed configuration, then 3. the GOP from its key frame.
	for i, want := range []struct {
		config bool
		key    bool
		seq    int
	}{{true, false, 0}, {false, true, 1}, {false, false, 2}} {
		opcode, payload := readServerFrame(t, br)
		if opcode != opBinary {
			t.Fatalf("replay frame %d opcode = %#x, want binary", i, opcode)
		}
		isConfig, isKey, _, body, err := DecodeStreamPacket(payload)
		if err != nil {
			t.Fatalf("replay frame %d: %v", i, err)
		}
		if isConfig != want.config || isKey != want.key || int(binary.BigEndian.Uint32(body[:4])) != want.seq {
			t.Fatalf("replay frame %d = config:%v key:%v seq:%d, want %+v",
				i, isConfig, isKey, binary.BigEndian.Uint32(body[:4]), want)
		}
	}

	// A live packet reaches the client without a gap.
	r.hub.Broadcast(makePacket(3, kindDelta, 16))
	opcode, payload = readServerFrame(t, br)
	if opcode != opBinary {
		t.Fatalf("live frame opcode = %#x", opcode)
	}
	if _, _, _, body, err := DecodeStreamPacket(payload); err != nil {
		t.Fatalf("live frame: %v", err)
	} else if seq := binary.BigEndian.Uint32(body[:4]); seq != 3 {
		t.Fatalf("live frame seq = %d, want 3", seq)
	}
}

func TestLoopbackAnswersPingAndClosesOnClose(t *testing.T) {
	r := newTestRuntime(t)
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 100, Height: 200}, "tok")

	conn, resp, br := dialStream(t, port, "/stream?token=tok", nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if opcode, _ := readServerFrame(t, br); opcode != opText {
		t.Fatalf("expected the ready message first, got opcode %#x", opcode)
	}

	if _, err := conn.Write(maskedFrame(t, opPing, []byte("hello"))); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	opcode, payload := readServerFrame(t, br)
	if opcode != opPong || string(payload) != "hello" {
		t.Fatalf("ping answered with opcode %#x payload %q", opcode, payload)
	}

	if _, err := conn.Write(maskedFrame(t, opClose, nil)); err != nil {
		t.Fatalf("write close: %v", err)
	}
	// The server answers the close and drops the client.
	if opcode, _ := readServerFrame(t, br); opcode != opClose {
		t.Fatalf("close answered with opcode %#x", opcode)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.hub.Clients()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the client was not removed from the hub after it closed")
}

func TestLoopbackRejectsBadRequests(t *testing.T) {
	r := newTestRuntime(t)
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 10, Height: 20}, "good-token")

	tests := []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{"wrong token", "/stream?token=wrong", nil},
		{"no token", "/stream", nil},
		{"empty token", "/stream?token=", nil},
		{"wrong path", "/other?token=good-token", nil},
		{"no upgrade header", "/stream?token=good-token", map[string]string{"Upgrade": ""}},
		{"no websocket key", "/stream?token=good-token", map[string]string{"Sec-WebSocket-Key": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, resp, _ := dialStream(t, port, tt.path, tt.headers)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// A token is only ever valid while a stream is actually running, so a stale
// widget cannot attach to a runtime that has stopped.
func TestLoopbackRejectsWhenNotStreaming(t *testing.T) {
	r := newTestRuntime(t)
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 10, Height: 20}, "tok")
	r.Stop()

	_, resp, _ := dialStream(t, port, "/stream?token=tok", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 once the stream has stopped", resp.StatusCode)
	}
}

func TestAcceptKeyMatchesRFC6455(t *testing.T) {
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("acceptKey = %q", got)
	}
}

func TestWSConnRejectsOversizedClientFrames(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()

	ws := newWSConn(serverSide, nil)
	done := make(chan error, 1)
	go func() { done <- ws.readLoop() }()

	// Announce a 1 GB frame without sending it.
	header := []byte{0x82, 0x80 | 127}
	header = binary.BigEndian.AppendUint64(header, 1<<30)
	if _, err := clientSide.Write(header); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an oversized frame must be reported as an error, not silently accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop did not reject the oversized frame")
	}
	_ = ws.close()
}
