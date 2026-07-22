package scrcpy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/adb"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
	"github.com/zjywill/scrcpyMac/phone-agent/internal/paths"
)

// Timings copied from the Python: 40 connect attempts, 0.8s per dial, 0.2s
// between attempts — about eight seconds of grace for scrcpy-server to come up.
const (
	handshakeAttempts = 40
	handshakeDial     = 800 * time.Millisecond
	handshakeBackoff  = 200 * time.Millisecond
	pushTimeout       = 60 * time.Second
	terminateGrace    = 2 * time.Second
	// videoReadBuffer keeps the pump to one syscall per several packets. It has
	// no latency cost: bufio.Reader returns as soon as any bytes are available.
	videoReadBuffer = 512 << 10
)

// session is one live scrcpy-server: the device-side process, the adb forward
// that reaches it, and the two sockets.
type session struct {
	serial      string
	adb         *adb.Client
	scid        uint32
	forwardPort int

	cmd    *exec.Cmd
	cancel context.CancelFunc
	exited chan struct{}

	video   net.Conn
	control net.Conn
	reader  *bufio.Reader

	meta *VideoMeta
}

// startSession pushes scrcpy-server, allocates a forward, spawns the server and
// completes the handshake. Everything it created is released on any failure, so
// a failed start leaves no forward, no process and no socket behind.
//
// ctx bounds the setup adb calls — it is the MCP request context, cancelled when
// the tool call returns. procCtx bounds the scrcpy-server process itself and
// must live for the whole session.
func startSession(
	ctx, procCtx context.Context,
	log *slog.Logger,
	client *adb.Client,
	serial string,
	maxFPS, resolutionPercent int,
) (sess *session, err error) {
	serverPath, err := paths.ScrcpyServer()
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}

	nativeWidth, nativeHeight, err := client.ScreenSize(ctx)
	if err != nil {
		return nil, err
	}
	maxSize := scaledMaxSize(nativeWidth, nativeHeight, resolutionPercent)

	if _, err := client.RunTimeout(ctx, pushTimeout, "push", serverPath, jarDevicePath); err != nil {
		return nil, err
	}

	scid, err := randomSCID()
	if err != nil {
		return nil, err
	}
	sess = &session{serial: serial, adb: client, scid: scid, exited: make(chan struct{})}
	defer func() {
		if err != nil {
			sess.close(log)
			sess = nil
		}
	}()

	socketName := fmt.Sprintf("localabstract:scrcpy_%08x", scid)
	forward, err := client.Run(ctx, "forward", "tcp:0", socketName)
	if err != nil {
		return nil, err
	}
	port, convErr := strconv.Atoi(strings.TrimSpace(forward.Stdout))
	if convErr != nil || port <= 0 {
		err = errorf("adb did not allocate a scrcpy forward port: %s", pyRepr(forward.Stdout))
		return nil, err
	}
	sess.forwardPort = port

	if err = sess.spawn(procCtx, log, spawnArgs(scid, maxSize, maxFPS)); err != nil {
		return nil, err
	}
	if err = sess.handshake(nativeWidth, nativeHeight, maxFPS, resolutionPercent); err != nil {
		return nil, err
	}
	return sess, nil
}

// scaledMaxSize is scrcpy's max_size: the long edge scaled by the requested
// percentage, floored at 320. round() here is Python's banker's rounding, which
// is what jsonresult.PyRoundInt implements.
func scaledMaxSize(nativeWidth, nativeHeight, resolutionPercent int) int {
	longEdge := nativeWidth
	if nativeHeight > longEdge {
		longEdge = nativeHeight
	}
	size := jsonresult.PyRoundInt(float64(longEdge) * float64(resolutionPercent) / 100)
	if size < 320 {
		return 320
	}
	return size
}

// spawnArgs builds the adb argument list for scrcpy-server. Kept separate so a
// test can assert the option set without a device.
//
// The option set is frozen: video_codec=h264 because the widget's WebCodecs
// decoder is configured from an AVC SPS, audio=false because the plugin has no
// audio path, tunnel_forward=true because the client dials the forward rather
// than the server dialling back, and cleanup=false so a crash cannot leave
// scrcpy's settings restoration half-applied on the device.
func spawnArgs(scid uint32, maxSize, maxFPS int) []string {
	return []string{
		"shell",
		"CLASSPATH=" + jarDevicePath,
		"app_process",
		"/",
		"com.genymobile.scrcpy.Server",
		paths.ScrcpyVersion,
		fmt.Sprintf("scid=%08x", scid),
		"log_level=info",
		"tunnel_forward=true",
		"video=true",
		"audio=false",
		"control=true",
		"video_codec=h264",
		fmt.Sprintf("max_size=%d", maxSize),
		fmt.Sprintf("max_fps=%d", maxFPS),
		"video_bit_rate=4000000",
		"clipboard_autosync=false",
		"cleanup=false",
	}
}

// spawn starts `adb shell app_process ...`.
//
// Its stdout and stderr are drained into the logger. Wiring either to os.Stdout
// would corrupt the MCP transport on the very first scrcpy log line.
func (s *session) spawn(procCtx context.Context, log *slog.Logger, args []string) error {
	ctx, cancel := context.WithCancel(procCtx)
	s.cancel = cancel

	argv := s.adb.Argv(args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = terminateGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errorf("could not capture scrcpy-server output: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return errorf("could not capture scrcpy-server output: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return errorf("could not start scrcpy-server: %v", err)
	}
	s.cmd = cmd

	go drainPipe(stdout, log, "scrcpy-server stdout")
	go drainPipe(stderr, log, "scrcpy-server stderr")
	go func() {
		_ = cmd.Wait()
		close(s.exited)
	}()
	return nil
}

// drainPipe forwards a scrcpy-server pipe to the log, line by line, at debug
// level.
//
// Draining is not optional and it must not stop early. bufio.Scanner gives up on
// a token longer than its buffer (a long Java stack trace line, a device that
// logs a whole dump), and if this goroutine returned there, the deferred Close
// would break the pipe under a device-side process that is still writing —
// killing the stream because of a log line. So a scanner failure falls back to
// discarding the rest: the log is lossy, the stream is not.
func drainPipe(r io.ReadCloser, log *slog.Logger, name string) {
	defer func() { _ = r.Close() }()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 64<<10)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" && log != nil {
			log.Debug(name, "line", line)
		}
	}
	if err := scanner.Err(); err != nil {
		if log != nil {
			log.Debug("scrcpy-server output is no longer being logged", "pipe", name, "err", err)
		}
		_, _ = io.Copy(io.Discard, r)
	}
}

// handshake connects the two sockets and reads scrcpy's video header.
//
// With tunnel_forward=true the server accepts the video socket first and the
// control socket second (audio is disabled, so there is no audio socket in
// between). The first byte on the video socket is a dummy 0x00 that lets the
// client tell "connected to the server" from "connected to a stale forward";
// then come 64 bytes of device name and the 12-byte video header.
func (s *session) handshake(nativeWidth, nativeHeight, maxFPS, resolutionPercent int) error {
	var lastErr error
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.forwardPort))

	for attempt := 0; attempt < handshakeAttempts; attempt++ {
		var video, control net.Conn
		err := func() error {
			select {
			case <-s.exited:
				return errorf("scrcpy-server exited before handshake (%d)", s.exitCode())
			default:
			}

			var dialErr error
			if video, dialErr = net.DialTimeout("tcp", address, handshakeDial); dialErr != nil {
				return dialErr
			}
			if control, dialErr = net.DialTimeout("tcp", address, handshakeDial); dialErr != nil {
				return dialErr
			}
			setNoDelay(video)
			setNoDelay(control)

			if err := video.SetReadDeadline(time.Now().Add(handshakeDial)); err != nil {
				return err
			}
			var dummy [1]byte
			if _, err := io.ReadFull(video, dummy[:]); err != nil {
				return readError(err)
			}
			if dummy[0] != 0 {
				return errorf("unexpected scrcpy handshake byte: %s", hex.EncodeToString(dummy[:]))
			}

			metadata := make([]byte, DeviceNameLength+12)
			if _, err := io.ReadFull(video, metadata); err != nil {
				return readError(err)
			}
			codecID := binary.BigEndian.Uint32(metadata[DeviceNameLength : DeviceNameLength+4])
			if codecID != H264CodecID {
				return errorf("scrcpy-server returned unsupported codec 0x%08x", codecID)
			}
			if err := video.SetReadDeadline(time.Time{}); err != nil {
				return err
			}

			s.video = video
			s.control = control
			s.reader = bufio.NewReaderSize(video, videoReadBuffer)
			s.meta = &VideoMeta{
				Serial:            s.serial,
				DeviceName:        decodeDeviceName(metadata[:DeviceNameLength], s.serial),
				CodecID:           codecID,
				Width:             int(binary.BigEndian.Uint32(metadata[DeviceNameLength+4 : DeviceNameLength+8])),
				Height:            int(binary.BigEndian.Uint32(metadata[DeviceNameLength+8 : DeviceNameLength+12])),
				NativeWidth:       nativeWidth,
				NativeHeight:      nativeHeight,
				MaxFPS:            maxFPS,
				ResolutionPercent: resolutionPercent,
			}
			return nil
		}()
		if err == nil {
			return nil
		}

		lastErr = err
		if video != nil {
			_ = video.Close()
		}
		if control != nil {
			_ = control.Close()
		}
		time.Sleep(handshakeBackoff)
	}
	return errorf("could not connect to plugin scrcpy-server: %v", lastErr)
}

// exitCode reports the device-side server's exit status once it has exited.
func (s *session) exitCode() int {
	if s.cmd != nil && s.cmd.ProcessState != nil {
		return s.cmd.ProcessState.ExitCode()
	}
	return -1
}

// close releases everything the session owns, in the order that gives the
// device-side process the best chance to exit on its own: sockets first (losing
// them is scrcpy-server's shutdown signal), then the local adb shell, then a
// targeted kill for the rare orphan, then the forward.
//
// Every step is best effort and independent — one failure must not strand the
// rest, because whatever is left behind outlives the plugin's own process.
func (s *session) close(log *slog.Logger) {
	if s == nil {
		return
	}

	if s.video != nil {
		_ = s.video.Close()
		s.video = nil
	}
	if s.control != nil {
		_ = s.control.Close()
		s.control = nil
	}

	spawned := s.cmd != nil
	if spawned {
		s.terminate()
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}

	// Belt and braces, and only when something was actually spawned. Killing
	// the local `adb shell` usually takes the device-side app_process with it,
	// but "usually" leaves an orphan holding the display encoder. scid is
	// unique to this session, so this pkill can never touch a scrcpy that
	// something else started.
	if spawned && s.adb != nil && s.scid != 0 {
		_, _ = s.adb.RunNoCheck(context.Background(), adb.TeardownTimeout,
			"shell", fmt.Sprintf("pkill -f scid=%08x", s.scid))
	}

	if s.adb != nil && s.forwardPort != 0 {
		if _, err := s.adb.RunNoCheck(context.Background(), adb.TeardownTimeout,
			"forward", "--remove", fmt.Sprintf("tcp:%d", s.forwardPort)); err != nil && log != nil {
			log.Warn("could not remove the scrcpy adb forward", "port", s.forwardPort, "err", err)
		}
		s.forwardPort = 0
	}
}

// terminate ends the local adb shell: SIGTERM, two seconds, then SIGKILL — the
// same escalation as Python's Popen.terminate/kill pair.
func (s *session) terminate() {
	select {
	case <-s.exited:
		return
	default:
	}

	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-s.exited:
		return
	case <-time.After(terminateGrace):
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	select {
	case <-s.exited:
	case <-time.After(terminateGrace):
	}
}

// readError maps a truncated socket read onto the Python's message, which the
// widget shows as "H.264 stream ended: short read from scrcpy-server".
func readError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errorf("short read from scrcpy-server")
	}
	return err
}

// decodeDeviceName reads the NUL-terminated device name, falling back to the
// serial when it is empty. Invalid UTF-8 is replaced rather than rejected,
// matching Python's errors="replace".
func decodeDeviceName(raw []byte, serial string) string {
	for i, b := range raw {
		if b == 0 {
			raw = raw[:i]
			break
		}
	}
	name := string(raw)
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "�")
	}
	if name == "" {
		return serial
	}
	return name
}

// setNoDelay disables Nagle. Without it the kernel batches small NAL units
// behind the peer's delayed ACK, which is exactly the wrong trade for a live
// stream. Go enables it by default on TCP conns; saying so explicitly keeps the
// guarantee from depending on a runtime default.
func setNoDelay(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
}

// randomSCID picks scrcpy's session id, in the Python's range [1, 0x7FFFFFFE].
func randomSCID() (uint32, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(0x7FFFFFFE))
	if err != nil {
		return 0, errorf("could not generate a scrcpy session id: %v", err)
	}
	return uint32(n.Uint64()) + 1, nil
}
