package scrcpy

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Queue and replay budgets.
//
// The queue budget has to hold more than one GOP or GOP-granular dropping is
// useless: scrcpy sets KEY_I_FRAME_INTERVAL to 10 seconds, so at the 4 Mbit/s
// this plugin requests one GOP is roughly 5 MB. 8 MiB gives a client about 1.5
// GOPs of slack — enough that discarding the oldest GOP still leaves a key frame
// to resume from, and small enough that a wedged client is noticed rather than
// buffered forever.
//
// The replay budget bounds the current-GOP buffer a newly connected client is
// replayed. Beyond it the buffer is abandoned and new clients wait for the next
// key frame instead, because replaying a partial GOP would hand the decoder
// deltas whose reference pictures it never received.
const (
	defaultQueueBytes  = 8 << 20
	defaultQueueItems  = 4096
	defaultReplayBytes = 8 << 20
)

// queued is one pending outbound WebSocket frame.
//
// video distinguishes H.264 packets, which the drop policy may discard, from
// control text ("ready", "config", "error"), which it never may. config marks
// the SPS/PPS packet: it is a video packet but carries no picture, so it is
// exempt from both the drop policy and the key-frame gate.
type queued struct {
	frame  []byte
	video  bool
	key    bool
	config bool
}

func (q queued) droppable() bool { return q.video && !q.config }

// ClientStats is a snapshot of one client's relay counters.
type ClientStats struct {
	// Sent counts frames handed to the socket.
	Sent uint64
	// SentBytes counts wire bytes handed to the socket, application headers
	// and WebSocket framing included.
	SentBytes uint64
	// DroppedGOPs counts how many times the queue overflowed and a whole GOP
	// was discarded.
	DroppedGOPs uint64
	// DroppedPackets counts individual access units discarded, either as part
	// of a dropped GOP or by the key-frame gate that follows one.
	DroppedPackets uint64
	// Queued is the current queue depth in frames.
	Queued int
	// QueuedBytes is the current queue depth in bytes.
	QueuedBytes int
	// WaitingKeyFrame reports that the client is gated until the next key
	// frame, i.e. its picture is frozen and will resync at the next IDR.
	WaitingKeyFrame bool
}

// Client is one connected widget: a socket, a bounded queue and a writer
// goroutine.
//
// The packet pump only ever appends to the queue under c.mu — a few pointer
// writes, no I/O, no logging — and never touches the socket. All blocking work
// happens on the writer goroutine, so a client that stops reading can never slow
// the pump, the device encoder, or any other client.
type Client struct {
	conn *wsConn
	log  *slog.Logger

	id          string
	connectedAt time.Time

	maxBytes int
	maxItems int

	mu       sync.Mutex
	queue    []queued
	head     int
	bytes    int
	needKey  bool
	closed   bool
	pendWarn int
	stats    ClientStats

	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newClient(id string, conn *wsConn, log *slog.Logger) *Client {
	return &Client{
		conn:        conn,
		log:         log,
		id:          id,
		connectedAt: time.Now(),
		maxBytes:    defaultQueueBytes,
		maxItems:    defaultQueueItems,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
}

// start launches the writer goroutine.
func (c *Client) start() {
	c.wg.Add(1)
	go c.writeLoop()
}

// ID is the client's short identifier, stable for its lifetime.
func (c *Client) ID() string { return c.id }

// ConnectedAt is when the WebSocket upgrade completed.
func (c *Client) ConnectedAt() time.Time { return c.connectedAt }

// QueueCapacity is the queue depth at which the drop policy starts discarding
// GOPs.
func (c *Client) QueueCapacity() int { return c.maxItems }

// Stats returns a snapshot of the client's counters.
func (c *Client) Stats() ClientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Queued = len(c.queue) - c.head
	s.QueuedBytes = c.bytes
	s.WaitingKeyFrame = c.needKey
	return s
}

// requireKeyFrame gates the client until the next key frame. Used when a client
// connects with no replayable GOP available.
func (c *Client) requireKeyFrame() {
	c.mu.Lock()
	c.needKey = true
	c.mu.Unlock()
}

// enqueue appends one frame, applying the key-frame gate and the GOP drop
// policy. It never blocks on I/O and never blocks on another client.
func (c *Client) enqueue(q queued) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}

	if q.droppable() {
		if c.needKey && !q.key {
			// Still resynchronising: a delta frame here would reference
			// pictures this client never received.
			c.stats.DroppedPackets++
			c.mu.Unlock()
			return
		}
		if q.key {
			c.needKey = false
		}
	}

	c.compactLocked()
	c.queue = append(c.queue, q)
	c.bytes += len(q.frame)

	for (c.bytes > c.maxBytes || len(c.queue)-c.head > c.maxItems) && c.dropOldestGOPLocked() {
	}
	c.mu.Unlock()

	c.signal()
}

// compactLocked slides a partially drained queue back to the front so the
// backing array is reused instead of growing without bound.
func (c *Client) compactLocked() {
	if c.head < 32 || c.head*2 < len(c.queue) {
		return
	}
	n := copy(c.queue, c.queue[c.head:])
	clear(c.queue[n:])
	c.queue = c.queue[:n]
	c.head = 0
}

// dropOldestGOPLocked discards the oldest complete GOP and reports whether
// anything was dropped.
//
// This is the heart of the redesign. Dropping an isolated delta frame — what the
// widget does today at ui/src/main.ts:573 — breaks the H.264 reference chain and
// freezes the picture until the next key frame. Instead everything from the head
// up to the next queued key frame is discarded as a unit, so the queue resumes
// exactly at a GOP boundary. When no newer key frame is queued there is no
// boundary to resume at, so every droppable frame goes and needKey gates the
// client until the device sends the next IDR.
//
// Control frames (the "ready"/"config"/"error" text messages) are always kept:
// they carry no picture and the widget needs them.
func (c *Client) dropOldestGOPLocked() bool {
	live := c.queue[c.head:]
	if len(live) == 0 {
		return false
	}

	// The boundary is the earliest queued key frame that actually has droppable
	// frames in front of it. Requiring "in front of it" matters: the head is
	// normally itself a key frame, and stopping at that one would drop nothing
	// and spin. Requiring a key frame matters because the queue must resume on
	// a GOP boundary — a delta whose reference frames were dropped is exactly
	// the frozen-picture failure this policy exists to avoid.
	end := len(live)
	resync := false
	droppableBefore := 0
	for i, q := range live {
		if q.droppable() && q.key && droppableBefore > 0 {
			end = i
			resync = true
			break
		}
		if q.droppable() {
			droppableBefore++
		}
	}

	kept := make([]queued, 0, len(live))
	dropped := 0
	for i, q := range live {
		if i < end && q.droppable() {
			c.bytes -= len(q.frame)
			dropped++
			continue
		}
		kept = append(kept, q)
	}
	if dropped == 0 {
		return false
	}

	c.queue = kept
	c.head = 0
	c.stats.DroppedGOPs++
	c.stats.DroppedPackets += uint64(dropped)
	if !resync {
		c.needKey = true
	}
	// Logging is deferred to the writer goroutine: enqueue runs on the packet
	// pump, under both the hub and the client mutex, and a blocked stderr must
	// never be able to stall the device encoder.
	c.pendWarn += dropped
	return true
}

// pop removes the oldest queued frame.
func (c *Client) pop() (queued, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.head >= len(c.queue) {
		return queued{}, false
	}
	q := c.queue[c.head]
	c.queue[c.head] = queued{}
	c.head++
	c.bytes -= len(q.frame)
	c.stats.Sent++
	c.stats.SentBytes += uint64(len(q.frame))
	if c.head == len(c.queue) {
		c.queue = c.queue[:0]
		c.head = 0
	}
	return q, true
}

// takeWarning returns and clears the deferred drop warning.
func (c *Client) takeWarning() (dropped int, waiting bool, queuedBytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped, c.pendWarn = c.pendWarn, 0
	return dropped, c.needKey, c.bytes
}

func (c *Client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// writeLoop drains the queue onto the socket. It is the only goroutine that
// performs a blocking write, which is what keeps the pump free.
func (c *Client) writeLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
		}
		for {
			q, ok := c.pop()
			if !ok {
				break
			}
			if err := c.conn.writeFrame(q.frame); err != nil {
				// The peer is gone. Closing here unblocks the reader
				// goroutine, which performs the hub removal.
				c.markClosed()
				_ = c.conn.close()
				return
			}
		}
		c.reportDrops()
	}
}

// reportDrops logs backpressure outside every lock.
func (c *Client) reportDrops() {
	dropped, waiting, queuedBytes := c.takeWarning()
	if dropped == 0 || c.log == nil {
		return
	}
	c.log.Warn("h264 client fell behind; dropped whole GOPs to stay live",
		"packets", dropped, "queuedBytes", queuedBytes, "awaitingKeyFrame", waiting)
}

// markClosed drops the queue and stops accepting frames.
func (c *Client) markClosed() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		clear(c.queue)
		c.queue = c.queue[:0]
		c.head = 0
		c.bytes = 0
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.done) })
}

// shutdown stops the writer, closes the socket and waits for the writer to
// finish. It is idempotent and safe from any goroutine.
func (c *Client) shutdown() {
	c.closeOnce.Do(func() { close(c.done) })
	_ = c.conn.close()
	c.wg.Wait()
	c.markClosed()
}

// Hub fans one H.264 stream out to every connected widget.
//
// It also owns the replay state: the last configuration packet and the current
// GOP. A client that connects mid-stream is replayed the configuration and then
// the whole GOP from its key frame, so its decoder starts with a complete
// reference chain. The Python replayed the configuration plus the last key frame
// only, which left the client decoding deltas against pictures it never saw.
type Hub struct {
	mu           sync.Mutex
	clients      map[*Client]struct{}
	config       *queued
	gop          []queued
	gopBytes     int
	gopAbandoned bool
	maxGOPBytes  int
	closed       bool

	broadcast atomic.Uint64
}

func newHub() *Hub {
	return &Hub{
		clients:     make(map[*Client]struct{}),
		maxGOPBytes: defaultReplayBytes,
	}
}

// Add registers a client and primes it: the ready message first, then the codec
// configuration, then the current GOP.
//
// Registration and replay happen under the same lock as Broadcast, so a packet
// can never slip between the replay snapshot and the client becoming visible —
// which would leave a hole in the client's reference chain.
func (h *Hub) Add(c *Client, ready []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	if len(ready) > 0 {
		c.enqueue(queued{frame: ready})
	}
	if h.config != nil {
		c.enqueue(*h.config)
	}
	if len(h.gop) > 0 {
		for _, q := range h.gop {
			c.enqueue(q)
		}
	} else {
		// No replayable GOP: gate the client so it starts at the next IDR
		// rather than on a delta frame.
		c.requireKeyFrame()
	}
	h.clients[c] = struct{}{}
	return true
}

// Remove deregisters a client.
func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// Broadcast records a packet for replay and queues it on every client. It never
// blocks: every client enqueue is a slice append under that client's own mutex.
func (h *Hub) Broadcast(p *Packet) {
	q := queued{frame: p.Frame, video: true, key: p.Key, config: p.Config}
	h.mu.Lock()
	h.record(q)
	for c := range h.clients {
		c.enqueue(q)
	}
	h.mu.Unlock()
	h.broadcast.Add(1)
}

// BroadcastText queues an already-framed text message on every client.
func (h *Hub) BroadcastText(frame []byte) {
	h.mu.Lock()
	for c := range h.clients {
		c.enqueue(queued{frame: frame})
	}
	h.mu.Unlock()
}

// record maintains the replay buffer. Callers hold h.mu.
func (h *Hub) record(q queued) {
	switch {
	case q.config:
		cfg := q
		h.config = &cfg
	case q.key:
		h.gop = append(h.gop[:0], q)
		h.gopBytes = len(q.frame)
		h.gopAbandoned = false
	case h.gopAbandoned || len(h.gop) == 0:
		// Nothing to append to: either the GOP grew past the replay budget or
		// no key frame has arrived yet.
	case h.gopBytes+len(q.frame) > h.maxGOPBytes:
		h.gop = h.gop[:0]
		h.gopBytes = 0
		h.gopAbandoned = true
	default:
		h.gop = append(h.gop, q)
		h.gopBytes += len(q.frame)
	}
}

// ResetReplay discards the replay state. Called when a session ends so a client
// connecting to the next session is never primed with the previous one's SPS.
func (h *Hub) ResetReplay() {
	h.mu.Lock()
	h.config = nil
	h.gop = h.gop[:0]
	h.gopBytes = 0
	h.gopAbandoned = false
	h.mu.Unlock()
}

// Clients returns the current client set.
func (h *Hub) Clients() []*Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

// CloseClients disconnects every client and waits for their writer goroutines,
// leaving the hub usable for the next session.
func (h *Hub) CloseClients() {
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.clients = make(map[*Client]struct{})
	h.mu.Unlock()

	for _, c := range clients {
		c.shutdown()
	}
}

// Close disconnects every client and refuses further registrations.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.CloseClients()
	h.ResetReplay()
}

// HubStats is the relay's aggregate drop accounting. It is deliberately not part
// of the MCP status payload — that key set is frozen contract — so it is exposed
// here for diagnostics, tests and the stderr log.
type HubStats struct {
	Clients        int
	Broadcast      uint64
	Sent           uint64
	DroppedGOPs    uint64
	DroppedPackets uint64
	MaxQueuedBytes int
}

// Stats aggregates the connected clients' counters.
func (h *Hub) Stats() HubStats {
	stats := HubStats{Broadcast: h.broadcast.Load()}
	for _, c := range h.Clients() {
		cs := c.Stats()
		stats.Clients++
		stats.Sent += cs.Sent
		stats.DroppedGOPs += cs.DroppedGOPs
		stats.DroppedPackets += cs.DroppedPackets
		if cs.QueuedBytes > stats.MaxQueuedBytes {
			stats.MaxQueuedBytes = cs.QueuedBytes
		}
	}
	return stats
}
