package scrcpy

import (
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StreamPath is the only path the loopback listener serves. The widget builds
// its URL from status().streamUrl, which always points here.
const StreamPath = "/stream"

// wsMagic is the RFC 6455 GUID mixed into Sec-WebSocket-Accept.
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// handshakeTimeout bounds how long a connection may take to send its request
// headers, matching the Python's 5-second socket timeout.
const handshakeTimeout = 5 * time.Second

// loopback is the 127.0.0.1 WebSocket listener the widget connects to.
//
// It is bound once, early, and kept for the process lifetime: its port is baked
// into the widget resource's CSP connectDomains when the MCP server is built, so
// rebinding later would publish a port the widget is no longer allowed to reach.
type loopback struct {
	ln   net.Listener
	srv  *http.Server
	port int

	closeOnce sync.Once
	done      chan struct{}
}

// startLoopback binds 127.0.0.1:0 and serves the upgrade endpoint.
func (r *Runtime) startLoopback() (*loopback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errorf("plugin loopback stream is unavailable: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, errorf("plugin loopback stream is unavailable")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", r.serveStream)

	lb := &loopback{
		ln:   ln,
		port: addr.Port,
		done: make(chan struct{}),
		srv: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: handshakeTimeout,
			// The widget is the only client and it is on the loopback
			// interface; nothing here should ever be slow.
			ReadTimeout: handshakeTimeout,
		},
	}
	go func() {
		defer close(lb.done)
		if err := lb.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.logger().Warn("loopback stream listener stopped", "err", err)
		}
	}()
	return lb, nil
}

// close stops the listener and waits for Serve to return. Hijacked connections
// are owned by the hub, not by http.Server, so the hub must be closed too.
func (lb *loopback) close() {
	if lb == nil {
		return
	}
	lb.closeOnce.Do(func() { _ = lb.srv.Close() })
	select {
	case <-lb.done:
	case <-time.After(2 * time.Second):
	}
}

// serveStream performs the WebSocket upgrade for GET /stream?token=...
//
// The checks mirror the Python exactly: method, path, a non-empty
// Sec-WebSocket-Key, an Upgrade: websocket header, and a token that matches the
// current session's. Anything else is 403 — including a wrong path, because the
// listener serves nothing but the stream.
func (r *Runtime) serveStream(w http.ResponseWriter, req *http.Request) {
	// Admission first, so Close waits for a handler that is already in flight
	// instead of racing it: http.Server.Close does not track hijacked
	// connections, and this one is about to hijack.
	if !r.enterHandler() {
		w.Header().Set("Connection", "close")
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}
	defer r.exitHandler()

	key := req.Header.Get("Sec-WebSocket-Key")
	if req.Method != http.MethodGet ||
		req.URL.Path != StreamPath ||
		key == "" ||
		!strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") ||
		!r.AcceptsStreamToken(req.URL.Query().Get("token")) {
		w.Header().Set("Connection", "close")
		http.Error(w, "", http.StatusForbidden)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hijacker.Hijack()
	if err != nil {
		r.logger().Warn("could not hijack the loopback connection", "err", err)
		return
	}

	// The stream is latency-critical and the frames are small; Nagle would
	// batch them behind the peer's delayed ACK.
	setNoDelay(conn)
	// http.Server's read deadline applies to the handshake only. Clear both
	// deadlines now that the connection belongs to the relay.
	_ = conn.SetDeadline(time.Time{})

	accept := acceptKey(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(response)); err != nil {
		_ = conn.Close()
		return
	}

	r.serveClient(newWSConn(conn, brw.Reader))
}

// acceptKey computes Sec-WebSocket-Accept.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsMagic))
	return base64.StdEncoding.EncodeToString(sum[:])
}
