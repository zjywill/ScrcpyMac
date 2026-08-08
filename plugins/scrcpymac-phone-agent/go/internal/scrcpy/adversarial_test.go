package scrcpy

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// TestFuzzDropPolicyNeverHandsOutAnOrphanDelta is the invariant the whole relay
// design rests on: a client must never be handed a delta frame whose reference
// chain it did not receive. Randomised GOP structures plus randomised drain
// rates explore the drop policy far past the hand-written cases.
func TestFuzzDropPolicyNeverHandsOutAnOrphanDelta(t *testing.T) {
	rng := rand.New(rand.NewSource(20260722))

	for iter := 0; iter < 200; iter++ {
		client := newClient("fuzz", nil, nil)
		// Tiny budgets so drops happen constantly.
		client.maxBytes = 400 + rng.Intn(2000)
		client.maxItems = 2 + rng.Intn(8)
		// Hub.Add gates every client that has no replayable GOP; model that.
		client.requireKeyFrame()

		var delivered []queued
		haveKey := false

		for step := 0; step < 400; step++ {
			var q queued
			switch {
			case rng.Intn(40) == 0:
				q = queued{frame: make([]byte, 40), video: true, config: true}
			case rng.Intn(10) == 0:
				q = queued{frame: make([]byte, 100+rng.Intn(400)), video: true, key: true}
			case rng.Intn(30) == 0:
				q = queued{frame: []byte("text")} // control message
			default:
				q = queued{frame: make([]byte, 50+rng.Intn(600)), video: true}
			}
			client.enqueue(q)

			// Drain a random prefix, as a writer goroutine would.
			for drains := rng.Intn(4); drains > 0; drains-- {
				got, ok := client.pop()
				if !ok {
					break
				}
				delivered = append(delivered, got)
				if !got.video || got.config {
					continue
				}
				if got.key {
					haveKey = true
					continue
				}
				if !haveKey {
					t.Fatalf("iter %d step %d: delta frame delivered with no preceding key frame "+
						"(maxBytes=%d maxItems=%d, delivered=%d)",
						iter, step, client.maxBytes, client.maxItems, len(delivered))
				}
			}
		}

		// Drain whatever is left with the same invariant.
		for {
			got, ok := client.pop()
			if !ok {
				break
			}
			if !got.video || got.config {
				continue
			}
			if got.key {
				haveKey = true
				continue
			}
			if !haveKey {
				t.Fatalf("iter %d: delta frame delivered from the tail with no preceding key frame", iter)
			}
		}

		if client.bytes != 0 {
			t.Fatalf("iter %d: byte accounting drifted, queue is empty but bytes=%d", iter, client.bytes)
		}
	}
}

// TestEnqueueByteAccountingStaysExact guards the queue-length bookkeeping the
// drop policy branches on: a drifting c.bytes either makes the relay drop for no
// reason or stops it dropping when it must.
func TestEnqueueByteAccountingStaysExact(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	client := newClient("acct", nil, nil)
	client.maxBytes = 5000
	client.maxItems = 16

	for step := 0; step < 5000; step++ {
		if rng.Intn(3) == 0 {
			if _, ok := client.pop(); ok {
				continue
			}
		}
		client.enqueue(queued{
			frame: make([]byte, 1+rng.Intn(900)),
			video: true,
			key:   rng.Intn(8) == 0,
		})

		client.mu.Lock()
		want := 0
		for _, q := range client.queue[client.head:] {
			want += len(q.frame)
		}
		got := client.bytes
		depth := len(client.queue) - client.head
		client.mu.Unlock()

		if got != want {
			t.Fatalf("step %d: c.bytes=%d, sum of queued frames=%d", step, got, want)
		}
		if depth > client.maxItems+1 {
			t.Fatalf("step %d: queue depth %d exceeds maxItems %d", step, depth, client.maxItems)
		}
	}
}

// TestConcurrentClientsBroadcastAndTeardown drives the whole loopback relay the
// way a flapping widget would: clients attaching and vanishing (some of them
// without ever reading a byte) while the pump broadcasts and the session is
// stopped underneath them. Run under -race it is the only test that exercises
// the pump/writer/reader/teardown lock interleavings together.
//
// It asserts the two properties shutdown has to guarantee: no goroutine is left
// behind, and the listening port is released.
func TestConcurrentClientsBroadcastAndTeardown(t *testing.T) {
	before := goroutineCount()

	r := newTestRuntime(t)
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 100, Height: 200}, "tok")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// The packet pump: never stops, never blocks, whatever the clients do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for seq := 0; ; seq++ {
			select {
			case <-stop:
				return
			default:
			}
			k := kindDelta
			if seq%20 == 0 {
				k = kindKey
			}
			r.hub.Broadcast(makePacket(seq, k, 4096))
			r.hub.BroadcastText(textFrame(nil))
		}
	}()

	// Clients that connect, read nothing, and disappear.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for round := 0; round < 6; round++ {
				select {
				case <-stop:
					return
				default:
				}
				conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
				if err != nil {
					return
				}
				_, _ = fmt.Fprintf(conn, "GET /stream?token=tok HTTP/1.1\r\nHost: 127.0.0.1\r\n"+
					"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
					"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
				time.Sleep(time.Duration(5+i) * time.Millisecond)
				_ = conn.Close()
			}
		}(i)
	}

	// Diagnostics readers, i.e. concurrent phone_stream_status calls.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.Diagnostics()
			_ = r.Status()
			_ = r.Stats()
		}
	}()

	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the loopback listener still accepts connections after Close")
	}
	assertNoGoroutineLeak(t, before)
}

// TestPumpIsNeverBlockedByAWedgedClient pins the property the redesign exists
// for: a client that reads nothing must not slow the broadcast down. The Python
// called sendall() on the pump thread, so one stalled widget backpressured the
// device encoder.
func TestPumpIsNeverBlockedByAWedgedClient(t *testing.T) {
	r := newTestRuntime(t)
	port, err := r.EnsureLoopback()
	if err != nil {
		t.Fatalf("EnsureLoopback: %v", err)
	}
	fakeStreaming(t, r, &VideoMeta{Serial: "S", Width: 100, Height: 200}, "tok")

	conn, resp, _ := dialStream(t, port, "/stream?token=tok", nil)
	if resp.StatusCode != 101 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	defer func() { _ = conn.Close() }()
	// Deliberately never read.

	// Built up front so the measurement is Broadcast, not allocation. 3000 x
	// 64 KiB is ~190 MB of nominal traffic, far past the ~8 MiB the client's
	// queue may hold, so the drop policy runs throughout.
	const packets = 3000
	frames := make([]*Packet, packets)
	for i := range frames {
		k := kindDelta
		if i%30 == 0 {
			k = kindKey
		}
		frames[i] = makePacket(i, k, 64<<10)
	}

	var worst time.Duration
	start := time.Now()
	for _, p := range frames {
		t0 := time.Now()
		r.hub.Broadcast(p)
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	total := time.Since(start)
	t.Logf("%d broadcasts past a wedged client: total %v, worst single %v", packets, total, worst)

	// Generous by two orders of magnitude — measured at ~1.5 ms total. The
	// failure this guards against is a socket write creeping back onto the pump,
	// which would put a whole TCP send buffer's worth of latency on every packet.
	if total > time.Second {
		t.Errorf("broadcasting %d packets past a non-reading client took %v; the pump is "+
			"blocking on the client (worst single broadcast %v)", packets, total, worst)
	}
	if worst > 100*time.Millisecond {
		t.Errorf("a single broadcast took %v; the pump is blocking on the client", worst)
	}
	if stats := r.Stats(); stats.DroppedPackets == 0 {
		t.Error("nothing was dropped, so the queue bound never engaged and this test " +
			"proved nothing")
	}
}

// TestDrainPipeKeepsDrainingPastAnOverlongLine is the "a log line must never
// kill the stream" guard.
//
// bufio.Scanner gives up on a token longer than its buffer. If drainPipe
// returned there, its deferred Close would break the pipe under a device-side
// process that is still writing, and scrcpy-server would die of a stack trace.
func TestDrainPipeKeepsDrainingPastAnOverlongLine(t *testing.T) {
	pr, pw := io.Pipe()

	done := make(chan struct{})
	go func() {
		drainPipe(pr, nil, "test")
		close(done)
	}()

	// One line far past the 64 KiB scanner limit, then more output behind it.
	if _, err := pw.Write(bytes.Repeat([]byte("x"), 300<<10)); err != nil {
		t.Fatalf("write long line: %v", err)
	}
	for i := 0; i < 200; i++ {
		if _, err := fmt.Fprintf(pw, "line %d after the overlong one\n", i); err != nil {
			t.Fatalf("write %d: the drainer stopped reading and broke the pipe: %v", i, err)
		}
	}
	_ = pw.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drainPipe did not return after the pipe closed")
	}
}
