package scrcpy

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// kind classifies a test packet.
type kind int

const (
	kindDelta kind = iota
	kindKey
	kindConfig
)

// makePacket builds a packet whose first four payload bytes carry a sequence
// number, so a test can tell exactly which packets a client received and
// whether the ones it received are contiguous.
func makePacket(seq int, k kind, size int) *Packet {
	if size < 4 {
		size = 4
	}
	payload := make([]byte, size)
	binary.BigEndian.PutUint32(payload[:4], uint32(seq))
	return EncodeStreamPacket(k == kindConfig, k == kindKey, uint64(seq), payload)
}

// delivered is one frame a client actually received.
type delivered struct {
	seq    int
	key    bool
	config bool
	text   bool
}

// drain pops everything queued on a client and decodes it.
func drain(t *testing.T, c *Client) []delivered {
	t.Helper()
	var out []delivered
	for {
		q, ok := c.pop()
		if !ok {
			return out
		}
		opcode, body := wsBody(t, q.frame)
		if opcode == opText {
			out = append(out, delivered{text: true})
			continue
		}
		isConfig, isKey, _, payload, err := DecodeStreamPacket(body)
		if err != nil {
			t.Fatalf("client received an undecodable packet: %v", err)
		}
		out = append(out, delivered{
			seq:    int(binary.BigEndian.Uint32(payload[:4])),
			key:    isKey,
			config: isConfig,
		})
	}
}

// assertDecodable checks the property that matters for H.264: every delta frame
// a client receives must be preceded by a key frame, with no gap in between.
//
// A gap is exactly what the widget's own "drop a delta when decodeQueueSize > 4"
// produces, and what freezes the picture until the next IDR. The relay must
// therefore only ever drop from a key frame boundary to the next one.
func assertDecodable(t *testing.T, got []delivered) {
	t.Helper()
	haveKey := false
	expected := -1
	for i, d := range got {
		switch {
		case d.text || d.config:
			// Neither carries a picture, so neither affects the chain.
		case d.key:
			haveKey = true
			expected = d.seq + 1
		default:
			if !haveKey {
				t.Fatalf("delivered[%d]: delta frame %d arrived before any key frame", i, d.seq)
			}
			if d.seq != expected {
				t.Fatalf("delivered[%d]: delta frame %d follows a gap (expected %d); "+
					"the reference chain is broken", i, d.seq, expected)
			}
			expected = d.seq + 1
		}
	}
}

func TestDropPolicyDiscardsWholeGOPs(t *testing.T) {
	c := newClient("test", nil, nil)
	// Room for a little more than one 10-packet GOP, so the overflow path runs
	// deterministically and still leaves a whole GOP behind.
	c.maxBytes = 12 * 1024
	c.maxItems = 1 << 20

	// Three GOPs of one key frame plus nine deltas each, never drained.
	seq := 0
	c.enqueue(queued{frame: makePacket(seq, kindConfig, 64).Frame, video: true, config: true})
	seq++
	for gop := 0; gop < 3; gop++ {
		p := makePacket(seq, kindKey, 1024)
		c.enqueue(queued{frame: p.Frame, video: true, key: true})
		seq++
		for i := 0; i < 9; i++ {
			p := makePacket(seq, kindDelta, 1024)
			c.enqueue(queued{frame: p.Frame, video: true})
			seq++
		}
	}

	got := drain(t, c)
	assertDecodable(t, got)

	stats := c.Stats()
	if stats.DroppedGOPs == 0 {
		t.Fatal("the queue budget was exceeded but nothing was dropped")
	}
	if stats.WaitingKeyFrame {
		t.Fatal("with a newer key frame queued the client must resume from it, not wait for the next IDR")
	}
	frames := 0
	for _, d := range got {
		if !d.text && !d.config {
			frames++
		}
	}
	if frames == 0 {
		t.Fatal("everything was dropped; the client should still hold the newest GOP")
	}
	if stats.QueuedBytes != 0 {
		t.Errorf("after draining, QueuedBytes = %d, want 0", stats.QueuedBytes)
	}
}

// After a drop that leaves no key frame to resume from, the client must be gated
// until the next IDR rather than fed deltas it cannot decode.
func TestDropPolicyGatesUntilTheNextKeyFrame(t *testing.T) {
	c := newClient("test", nil, nil)
	c.maxBytes = 2 * 1024
	c.maxItems = 1 << 20

	seq := 0
	c.enqueue(queued{frame: makePacket(seq, kindKey, 1024).Frame, video: true, key: true})
	seq++
	// Deltas keep arriving with no new key frame; the queue can only hold two.
	for i := 0; i < 20; i++ {
		c.enqueue(queued{frame: makePacket(seq, kindDelta, 1024).Frame, video: true})
		seq++
	}

	if !c.Stats().WaitingKeyFrame {
		t.Fatal("with no key frame left to resume from, the client must wait for the next IDR")
	}

	// Deltas are dropped while gated...
	before := c.Stats().DroppedPackets
	c.enqueue(queued{frame: makePacket(seq, kindDelta, 16).Frame, video: true})
	seq++
	if c.Stats().DroppedPackets != before+1 {
		t.Fatal("a delta arriving while gated must be dropped, not queued")
	}

	// ...and the gate lifts on the next key frame.
	keySeq := seq
	c.enqueue(queued{frame: makePacket(keySeq, kindKey, 16).Frame, video: true, key: true})
	seq++
	c.enqueue(queued{frame: makePacket(seq, kindDelta, 16).Frame, video: true})

	if c.Stats().WaitingKeyFrame {
		t.Fatal("a key frame must lift the gate")
	}
	got := drain(t, c)
	assertDecodable(t, got)
	if len(got) == 0 || !got[0].key || got[0].seq != keySeq {
		t.Fatalf("after resync the first delivered packet must be key frame %d, got %+v", keySeq, got)
	}
}

// Control text ("ready", "config", "error") carries no picture and must survive
// every drop, or the widget never learns that the stream failed.
func TestTextFramesSurviveDrops(t *testing.T) {
	c := newClient("test", nil, nil)
	c.maxBytes = 3 * 1024
	c.maxItems = 1 << 20

	c.enqueue(queued{frame: wsFrame(opText, []byte(`{"type":"ready"}`))})
	c.enqueue(queued{frame: makePacket(0, kindKey, 1024).Frame, video: true, key: true})
	for i := 1; i < 10; i++ {
		c.enqueue(queued{frame: makePacket(i, kindDelta, 1024).Frame, video: true})
	}
	c.enqueue(queued{frame: wsFrame(opText, []byte(`{"type":"error"}`))})

	got := drain(t, c)
	texts := 0
	for _, d := range got {
		if d.text {
			texts++
		}
	}
	if texts != 2 {
		t.Fatalf("got %d text frames, want 2 — text must never be dropped", texts)
	}
	if c.Stats().DroppedGOPs == 0 {
		t.Fatal("the test did not exercise the drop path")
	}
}

func TestHubReplaysTheWholeGOPToANewClient(t *testing.T) {
	hub := newHub()
	hub.Broadcast(makePacket(0, kindConfig, 32))
	hub.Broadcast(makePacket(1, kindKey, 64))
	hub.Broadcast(makePacket(2, kindDelta, 64))
	hub.Broadcast(makePacket(3, kindDelta, 64))

	c := newClient("test", nil, nil)
	if !hub.Add(c, wsFrame(opText, []byte(`{"type":"ready"}`))) {
		t.Fatal("Add refused a client on an open hub")
	}
	// Live packets continue after the replay and must follow it seamlessly.
	hub.Broadcast(makePacket(4, kindDelta, 64))

	got := drain(t, c)
	if len(got) != 6 {
		t.Fatalf("got %d frames, want ready + config + key + 3 deltas: %+v", len(got), got)
	}
	if !got[0].text {
		t.Error("the ready message must come first")
	}
	if !got[1].config {
		t.Error("the codec configuration must come before any picture")
	}
	if !got[2].key || got[2].seq != 1 {
		t.Errorf("the replay must start at the GOP's key frame, got %+v", got[2])
	}
	assertDecodable(t, got)
}

// A client that connects before any key frame has arrived has nothing to replay
// and must wait, rather than being handed deltas against a picture it lacks.
func TestHubGatesAClientThatConnectsBeforeAKeyFrame(t *testing.T) {
	hub := newHub()
	hub.Broadcast(makePacket(0, kindConfig, 32))

	c := newClient("test", nil, nil)
	hub.Add(c, nil)
	if !c.Stats().WaitingKeyFrame {
		t.Fatal("a client with no replayable GOP must be gated")
	}

	hub.Broadcast(makePacket(1, kindDelta, 32))
	hub.Broadcast(makePacket(2, kindKey, 32))
	hub.Broadcast(makePacket(3, kindDelta, 32))

	got := drain(t, c)
	assertDecodable(t, got)
	if len(got) != 3 {
		t.Fatalf("got %d frames, want config + key + delta: %+v", len(got), got)
	}
}

// Past the replay budget the GOP buffer is abandoned: replaying half a GOP would
// hand the decoder deltas whose references it never received.
func TestHubAbandonsAnOversizedGOP(t *testing.T) {
	hub := newHub()
	hub.maxGOPBytes = 4 * 1024

	hub.Broadcast(makePacket(0, kindConfig, 32))
	hub.Broadcast(makePacket(1, kindKey, 1024))
	for i := 2; i < 12; i++ {
		hub.Broadcast(makePacket(i, kindDelta, 1024))
	}

	c := newClient("test", nil, nil)
	hub.Add(c, nil)
	if !c.Stats().WaitingKeyFrame {
		t.Fatal("with the GOP buffer abandoned, a new client must wait for the next key frame")
	}

	hub.Broadcast(makePacket(12, kindKey, 64))
	got := drain(t, c)
	assertDecodable(t, got)
}

// The whole point of the redesign: a client that never reads must not slow the
// packet pump down. The Python called sendall() on the pump thread, so this
// scenario stalled the device encoder.
func TestSlowClientNeverBlocksTheBroadcast(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientSide.Close()
	})

	hub := newHub()
	c := newClient("slow", newWSConn(serverSide, nil), nil)
	c.start()
	hub.Add(c, nil)
	t.Cleanup(func() {
		hub.Remove(c)
		c.shutdown()
	})

	// clientSide is never read, so the writer goroutine blocks on its very
	// first write and stays blocked for the whole test.
	const packets = 4000
	payload := 8 << 10

	start := time.Now()
	for i := 0; i < packets; i++ {
		k := kindDelta
		if i%300 == 0 {
			k = kindKey
		}
		hub.Broadcast(makePacket(i, k, payload))
	}
	elapsed := time.Since(start)

	// 4000 * 8 KiB is 32 MiB of video against an 8 MiB budget, all of it
	// against a socket that accepts nothing. Anything beyond a second means
	// the pump is blocking on the consumer.
	if elapsed > 2*time.Second {
		t.Fatalf("broadcasting %d packets to a stalled client took %v; the pump is blocking", packets, elapsed)
	}

	stats := c.Stats()
	if stats.DroppedGOPs == 0 {
		t.Fatal("a stalled client must have caused GOP drops")
	}
	if stats.QueuedBytes > c.maxBytes {
		t.Fatalf("queue grew to %d bytes, past the %d budget", stats.QueuedBytes, c.maxBytes)
	}
}

func TestHubCloseReleasesClients(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()

	hub := newHub()
	c := newClient("test", newWSConn(serverSide, nil), nil)
	c.start()
	hub.Add(c, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hub.Close did not return; a writer goroutine is stuck")
	}

	if n := len(hub.Clients()); n != 0 {
		t.Fatalf("%d clients survived Close", n)
	}
	if hub.Add(newClient("late", nil, nil), nil) {
		t.Fatal("a closed hub must refuse new clients")
	}
}
