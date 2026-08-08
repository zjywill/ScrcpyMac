package scrcpy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// maxClientFrameBytes bounds a single inbound WebSocket frame. The widget only
// ever sends close and ping frames, so anything large is either a bug or an
// attempt to make the relay allocate on demand.
const maxClientFrameBytes = 1 << 20

// errWSClosed is returned once a wsConn has been closed.
var errWSClosed = errors.New("WebSocket client is closed")

// closeFrameTimeout bounds the courtesy close frame. A peer that has stopped
// reading must not be able to hold shutdown open.
const closeFrameTimeout = 250 * time.Millisecond

// wsConn is one accepted loopback WebSocket connection.
//
// Writes are serialised by wmu because two goroutines can reach them: the
// client's writer goroutine draining its queue, and close sending the close
// frame. Reads happen only on the connection's own reader goroutine.
//
// closed is atomic rather than guarded by wmu on purpose. A stalled peer leaves
// the writer goroutine blocked inside Write while it holds wmu; if close had to
// take that mutex it would deadlock against the very goroutine it is trying to
// release. close therefore only *tries* for the lock, and always reaches
// conn.Close, which is what unblocks the pending write.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu    sync.Mutex
	closed atomic.Bool
}

func newWSConn(conn net.Conn, br *bufio.Reader) *wsConn {
	if br == nil {
		br = bufio.NewReaderSize(conn, 4096)
	}
	return &wsConn{conn: conn, br: br}
}

// writeFrame sends an already-framed message. The relay pre-frames every video
// packet exactly once and shares the bytes across clients.
func (w *wsConn) writeFrame(frame []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closed.Load() {
		return errWSClosed
	}
	_, err := w.conn.Write(frame)
	return err
}

// close sends a close frame, then tears the socket down. It is idempotent, safe
// from any goroutine, and never blocks for longer than closeFrameTimeout;
// closing the net.Conn is what unblocks both the reader and a stalled writer.
func (w *wsConn) close() error {
	if w.closed.Swap(true) {
		return nil
	}
	// Best effort. The peer may be gone, or the writer goroutine may be blocked
	// mid-frame holding wmu — in which case skipping the courtesy frame and
	// going straight to Close is exactly what releases it.
	if w.wmu.TryLock() {
		_ = w.conn.SetWriteDeadline(time.Now().Add(closeFrameTimeout))
		_, _ = w.conn.Write(wsFrame(opClose, nil))
		w.wmu.Unlock()
	}
	return w.conn.Close()
}

// readLoop consumes inbound frames until the peer closes, the socket dies, or a
// protocol violation is seen. It answers pings and ignores everything else,
// which is all the widget protocol needs: the stream is one-directional.
//
// It returns nil for an orderly close (a close frame or EOF) so callers can tell
// a normal disconnect from a failure.
func (w *wsConn) readLoop() error {
	for {
		opcode, payload, err := w.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch opcode {
		case opClose:
			return nil
		case opPing:
			if err := w.writeFrame(wsFrame(opPong, payload)); err != nil {
				return err
			}
		default:
			// Text, binary, continuation and pong frames are discarded: the
			// widget never sends them and the relay has no inbound protocol.
		}
	}
}

// readFrame reads one RFC 6455 frame and returns its opcode and unmasked
// payload. Fragmentation is not reassembled — each frame is handled on its own,
// which matches the Python and is sufficient for close/ping.
func (w *wsConn) readFrame() (opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(w.br, head[:]); err != nil {
		return 0, nil, err
	}
	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > maxClientFrameBytes {
		return 0, nil, errorf("oversized WebSocket frame from client: %d bytes", length)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
	}
	return opcode, payload, nil
}
