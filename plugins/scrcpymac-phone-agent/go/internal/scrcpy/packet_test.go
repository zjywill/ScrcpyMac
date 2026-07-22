package scrcpy

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// wsBody strips the RFC 6455 header from a frame produced by this package and
// returns the opcode and the application payload. It deliberately re-derives the
// header size from the wire bytes rather than trusting wsHeaderLen, so a broken
// encoder cannot make the test agree with itself.
func wsBody(t *testing.T, frame []byte) (opcode byte, body []byte) {
	t.Helper()
	if len(frame) < 2 {
		t.Fatalf("frame is %d bytes, too short for a header", len(frame))
	}
	if frame[0]&0x80 == 0 {
		t.Fatalf("FIN is not set: first byte %#02x", frame[0])
	}
	if frame[1]&0x80 != 0 {
		t.Fatal("server-to-client frames must not be masked")
	}
	opcode = frame[0] & 0x0F

	switch n := frame[1] & 0x7F; {
	case n < 126:
		return opcode, frame[2:]
	case n == 126:
		want := int(binary.BigEndian.Uint16(frame[2:4]))
		if got := len(frame) - 4; got != want {
			t.Fatalf("16-bit length says %d, frame carries %d", want, got)
		}
		return opcode, frame[4:]
	default:
		want := int(binary.BigEndian.Uint64(frame[2:10]))
		if got := len(frame) - 10; got != want {
			t.Fatalf("64-bit length says %d, frame carries %d", want, got)
		}
		return opcode, frame[10:]
	}
}

func TestParseVideoHeader(t *testing.T) {
	tests := []struct {
		name     string
		field    uint64
		size     uint32
		isConfig bool
		isKey    bool
		pts      uint64
	}{
		{"config", packetFlagConfig, 42, true, false, 0},
		{"keyframe", packetFlagKeyFrame | 1234, 4096, false, true, 1234},
		{"delta", 999, 512, false, false, 999},
		{"both flags", packetFlagConfig | packetFlagKeyFrame | 7, 1, true, true, 7},
		{"max pts", packetPTSMask, 0, false, false, packetPTSMask},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := make([]byte, FrameHeaderLength)
			binary.BigEndian.PutUint64(header[:8], tt.field)
			binary.BigEndian.PutUint32(header[8:], tt.size)

			isConfig, isKey, pts, size, err := ParseVideoHeader(header)
			if err != nil {
				t.Fatalf("ParseVideoHeader: %v", err)
			}
			if isConfig != tt.isConfig || isKey != tt.isKey || pts != tt.pts || size != tt.size {
				t.Fatalf("got config=%v key=%v pts=%d size=%d, want config=%v key=%v pts=%d size=%d",
					isConfig, isKey, pts, size, tt.isConfig, tt.isKey, tt.pts, tt.size)
			}
		})
	}
}

func TestParseVideoHeaderRejectsShortHeader(t *testing.T) {
	if _, _, _, _, err := ParseVideoHeader(make([]byte, FrameHeaderLength-1)); err == nil {
		t.Fatal("a short header must be rejected")
	}
}

// The 14-byte application header is parsed by ui/src/main.ts:parseWsPacket and
// cannot change. This pins the exact bytes against the equivalent
// struct.pack(">BBQI", 1, flags, pts, length) from the Python.
func TestStreamPacketHeaderBytesAreFrozen(t *testing.T) {
	packet := EncodeStreamPacket(true, true, 123456789, []byte{1, 2, 3, 4, 5})
	opcode, body := wsBody(t, packet.Frame)
	if opcode != opBinary {
		t.Fatalf("opcode = %#x, want binary", opcode)
	}

	const wantHeader = "010300000000075bcd1500000005"
	if got := hex.EncodeToString(body[:WSHeaderLength]); got != wantHeader {
		t.Fatalf("application header = %s, want %s", got, wantHeader)
	}
	if got := hex.EncodeToString(body[WSHeaderLength:]); got != "0102030405" {
		t.Fatalf("payload = %s", got)
	}
}

func TestStreamPacketRoundTrip(t *testing.T) {
	payload := make([]byte, 300)
	for i := range payload {
		payload[i] = byte(i)
	}

	for _, tt := range []struct {
		name            string
		isConfig, isKey bool
		pts             uint64
		payload         []byte
	}{
		{"config", true, false, 0, payload[:40]},
		{"keyframe", false, true, 1 << 40, payload},
		{"delta", false, false, 16_666, payload[:1]},
		{"empty payload", false, false, 0, nil},
		{"64-bit length", false, true, 5, make([]byte, 70_000)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			packet := EncodeStreamPacket(tt.isConfig, tt.isKey, tt.pts, tt.payload)
			if packet.Size != len(tt.payload) {
				t.Fatalf("Size = %d, want %d", packet.Size, len(tt.payload))
			}
			_, body := wsBody(t, packet.Frame)

			isConfig, isKey, pts, decoded, err := DecodeStreamPacket(body)
			if err != nil {
				t.Fatalf("DecodeStreamPacket: %v", err)
			}
			if isConfig != tt.isConfig || isKey != tt.isKey || pts != tt.pts {
				t.Fatalf("got config=%v key=%v pts=%d, want config=%v key=%v pts=%d",
					isConfig, isKey, pts, tt.isConfig, tt.isKey, tt.pts)
			}
			if len(decoded) != len(tt.payload) {
				t.Fatalf("payload length = %d, want %d", len(decoded), len(tt.payload))
			}
			for i := range decoded {
				if decoded[i] != tt.payload[i] {
					t.Fatalf("payload differs at %d", i)
				}
			}
		})
	}
}

func TestDecodeStreamPacketRejectsBadInput(t *testing.T) {
	good := EncodeStreamPacket(false, true, 1, []byte{9, 9, 9})
	_, body := wsBody(t, good.Frame)

	if _, _, _, _, err := DecodeStreamPacket(body[:WSHeaderLength-1]); err == nil {
		t.Error("a truncated header must be rejected")
	}

	wrongVersion := append([]byte(nil), body...)
	wrongVersion[0] = 2
	if _, _, _, _, err := DecodeStreamPacket(wrongVersion); err == nil {
		t.Error("an unknown version must be rejected, as the widget does")
	}

	shortPayload := append([]byte(nil), body...)
	if _, _, _, _, err := DecodeStreamPacket(shortPayload[:len(shortPayload)-1]); err == nil {
		t.Error("a length mismatch must be rejected, as the widget does")
	}
}

func TestWSHeaderLengthBoundaries(t *testing.T) {
	for _, tt := range []struct {
		size int
		want int
	}{{0, 2}, {125, 2}, {126, 4}, {0xFFFF, 4}, {0x10000, 10}} {
		if got := wsHeaderLen(tt.size); got != tt.want {
			t.Errorf("wsHeaderLen(%d) = %d, want %d", tt.size, got, tt.want)
		}
		frame := wsFrame(opBinary, make([]byte, tt.size))
		if len(frame) != tt.want+tt.size {
			t.Errorf("wsFrame(%d) is %d bytes, want %d", tt.size, len(frame), tt.want+tt.size)
		}
	}
}

func TestSplitAnnexB(t *testing.T) {
	data := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xC0, 0x1E, // SPS with a 4-byte start code
		0, 0, 1, 0x68, 0xCE, // PPS with a 3-byte start code
		0, 0, 0, 1, // trailing start code with no payload: dropped
	}
	units := splitAnnexB(data)
	if len(units) != 2 {
		t.Fatalf("got %d NAL units, want 2: %v", len(units), units)
	}
	if units[0][0] != 0x67 || units[1][0] != 0x68 {
		t.Fatalf("wrong NAL units: %x / %x", units[0], units[1])
	}
}

func TestCodecFromConfig(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 0x42, 0xC0, 0x1F, 0xAA}
	if got := codecFromConfig(sps); got != "avc1.42C01F" {
		t.Fatalf("codecFromConfig = %q, want avc1.42C01F", got)
	}
	if got := codecFromConfig([]byte{0, 0, 0, 1, 0x68, 0xCE}); got != DefaultCodec {
		t.Fatalf("without an SPS the codec must fall back to %q, got %q", DefaultCodec, got)
	}
	if got := codecFromConfig(nil); got != DefaultCodec {
		t.Fatalf("empty config must fall back to %q, got %q", DefaultCodec, got)
	}
}
