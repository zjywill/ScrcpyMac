package scrcpy

import (
	"fmt"
	"time"
)

// Diagnostics is the relay's own account of what it is doing.
//
// It exists because the stream-status payload is frozen contract and cannot grow
// keys, yet a ~1 FPS regression is impossible to diagnose without knowing
// whether the device is producing packets, whether a client's queue is backed
// up, and whether whole GOPs are being discarded. The MCP layer maps this onto
// its own diagnostics tool.
type Diagnostics struct {
	// State mirrors the status payload: idle, starting, streaming or error.
	State string
	// Backend is "plugin-h264" while streaming, "adb" otherwise.
	Backend string
	// Serial is the streaming device, "" when idle.
	Serial string
	// Codec is the negotiated codec string, e.g. "avc1.42E01E".
	Codec string
	// DeviceWidth and DeviceHeight are the panel's native pixels.
	DeviceWidth, DeviceHeight int
	// FrameWidth and FrameHeight are the encoded frame size.
	FrameWidth, FrameHeight int

	// UptimeS is seconds since the session started, 0 when idle.
	UptimeS float64

	// Packets counts everything read off the device socket; Frames excludes
	// SPS/PPS configuration packets; KeyFrames counts IDRs, i.e. the GOP
	// boundaries a lagging client can resync on.
	Packets, Frames, KeyFrames int64
	// Bytes is the total H.264 payload read from the device socket.
	Bytes int64

	// PacketsPerSecond and FramesPerSecond are measured over the pump's recent
	// window: the device's real output rate, independent of any client.
	PacketsPerSecond, FramesPerSecond float64

	// DroppedGOPs and DroppedPackets total the relay's discards across every
	// client. Non-zero means at least one client could not keep up.
	DroppedGOPs, DroppedPackets int64

	// LastError is the most recent relay error, "" when healthy.
	LastError string

	// Clients is one entry per attached WebSocket client. Never nil.
	Clients []ClientDiagnostics
}

// ClientDiagnostics is one attached widget.
type ClientDiagnostics struct {
	// ID identifies the client in logs and diagnostics.
	ID string
	// ConnectedS is seconds since this client attached.
	ConnectedS float64
	// QueueDepth is how many frames are waiting to be written to this client,
	// and QueueCapacity the bound at which the relay starts dropping GOPs. A
	// depth that sits near capacity is the signature of a slow consumer.
	QueueDepth, QueueCapacity int
	// PacketsSent and BytesSent are what actually reached this client.
	PacketsSent, BytesSent int64
	// DroppedGOPs and DroppedPackets are what this client missed.
	DroppedGOPs, DroppedPackets int64
	// WaitingForKeyFrame is true while the client is resyncing after a drop and
	// is receiving nothing until the next IDR.
	WaitingForKeyFrame bool
}

// Diagnostics snapshots the relay.
//
// It takes the runtime lock and each client's lock briefly and performs no I/O,
// so it can never block on the packet pump or on a client's socket.
func (r *Runtime) Diagnostics() Diagnostics {
	r.mu.Lock()
	d := Diagnostics{
		State:            r.state,
		Backend:          backendName(r.state),
		Codec:            r.codec,
		Packets:          int64(r.packetCount),
		Frames:           int64(r.frameCount),
		KeyFrames:        int64(r.keyFrameCount),
		Bytes:            r.byteCount,
		PacketsPerSecond: r.packets.rate(),
		FramesPerSecond:  r.frames.rate(),
		LastError:        r.errMsg,
		Clients:          []ClientDiagnostics{},
	}
	if meta := r.meta; meta != nil {
		d.Serial = meta.Serial
		d.DeviceWidth = meta.NativeWidth
		d.DeviceHeight = meta.NativeHeight
		d.FrameWidth = meta.Width
		d.FrameHeight = meta.Height
	}
	if r.state == StateStreaming && !r.startedAt.IsZero() {
		d.UptimeS = time.Since(r.startedAt).Seconds()
	}
	r.mu.Unlock()

	for _, client := range r.hub.Clients() {
		stats := client.Stats()
		d.Clients = append(d.Clients, ClientDiagnostics{
			ID:                 client.ID(),
			ConnectedS:         time.Since(client.ConnectedAt()).Seconds(),
			QueueDepth:         stats.Queued,
			QueueCapacity:      client.QueueCapacity(),
			PacketsSent:        int64(stats.Sent),
			BytesSent:          int64(stats.SentBytes),
			DroppedGOPs:        int64(stats.DroppedGOPs),
			DroppedPackets:     int64(stats.DroppedPackets),
			WaitingForKeyFrame: stats.WaitingKeyFrame,
		})
		d.DroppedGOPs += int64(stats.DroppedGOPs)
		d.DroppedPackets += int64(stats.DroppedPackets)
	}
	return d
}

// nextClientID mints a short, stable identifier for a newly attached client.
func (r *Runtime) nextClientID() string {
	r.mu.Lock()
	r.clientSeq++
	seq := r.clientSeq
	r.mu.Unlock()
	return fmt.Sprintf("c%d", seq)
}
