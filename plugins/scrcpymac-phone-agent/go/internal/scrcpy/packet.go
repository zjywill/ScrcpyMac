package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// WebSocket opcodes. Only these four are ever produced by the relay.
const (
	opText   byte = 0x1
	opBinary byte = 0x2
	opClose  byte = 0x8
	opPing   byte = 0x9
	opPong   byte = 0xA
)

// maxPayloadBytes bounds one H.264 access unit. scrcpy at 4 Mbit/s never comes
// close; the limit exists so a desynchronised socket cannot make the pump
// allocate gigabytes before it notices.
const maxPayloadBytes = 32 << 20

// Packet is one H.264 access unit, already framed for the wire.
//
// Frame is the COMPLETE WebSocket binary frame — RFC 6455 header, then the
// 14-byte application header, then the payload. It is built once per packet and
// shared, read-only, by every connected client: server-to-client frames are
// unmasked, so the bytes are identical for all of them. Nothing may mutate it
// after EncodeStreamPacket returns.
type Packet struct {
	// Config marks a codec-configuration packet (SPS/PPS). It carries no
	// picture and is never dropped.
	Config bool
	// Key marks an IDR access unit, i.e. the start of a GOP.
	Key bool
	// PTS is the presentation timestamp in microseconds, flag bits removed.
	PTS uint64
	// Size is the H.264 payload length, without either header.
	Size int
	// Frame is the ready-to-send WebSocket frame.
	Frame []byte
}

// ParseVideoHeader decodes scrcpy's 12-byte packet header.
//
//	bit 63 of the pts field  CONFIG
//	bit 62 of the pts field  KEYFRAME
//	bits 61..0               pts, microseconds
//	trailing uint32          payload length
func ParseVideoHeader(header []byte) (isConfig, isKey bool, pts uint64, size uint32, err error) {
	if len(header) < FrameHeaderLength {
		return false, false, 0, 0, errorf("short read from scrcpy-server")
	}
	field := binary.BigEndian.Uint64(header[:8])
	size = binary.BigEndian.Uint32(header[8:FrameHeaderLength])
	return field&packetFlagConfig != 0, field&packetFlagKeyFrame != 0, field & packetPTSMask, size, nil
}

// EncodeStreamPacket builds the WebSocket frame for one access unit.
//
// The application header layout is frozen by ui/src/main.ts:parseWsPacket:
//
//	offset 0   uint8   version, always 1
//	offset 1   uint8   flags: bit0 CONFIG, bit1 KEYFRAME
//	offset 2   uint64  pts, big endian
//	offset 10  uint32  payload length, big endian
//	offset 14  payload
func EncodeStreamPacket(isConfig, isKey bool, pts uint64, payload []byte) *Packet {
	var flags byte
	if isConfig {
		flags |= WSFlagConfig
	}
	if isKey {
		flags |= WSFlagKeyFrame
	}

	body := WSHeaderLength + len(payload)
	frame := make([]byte, 0, wsHeaderLen(body)+body)
	frame = appendWSHeader(frame, opBinary, body)
	frame = append(frame, WSPacketVersion, flags)
	frame = binary.BigEndian.AppendUint64(frame, pts)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)

	return &Packet{Config: isConfig, Key: isKey, PTS: pts, Size: len(payload), Frame: frame}
}

// ErrShortPacket is returned by DecodeStreamPacket for a truncated or malformed
// application packet. It mirrors the two errors parseWsPacket throws.
var ErrShortPacket = errors.New("truncated ScrcpyMac H.264 packet")

// DecodeStreamPacket is the inverse of the application header written by
// EncodeStreamPacket. The relay never needs it; it exists so tests can assert
// the exact bytes the widget will parse, and it is written against the widget's
// parseWsPacket rather than against the encoder.
func DecodeStreamPacket(body []byte) (isConfig, isKey bool, pts uint64, payload []byte, err error) {
	if len(body) < WSHeaderLength {
		return false, false, 0, nil, ErrShortPacket
	}
	if body[0] != WSPacketVersion {
		return false, false, 0, nil, fmt.Errorf("unsupported stream version %d", body[0])
	}
	flags := body[1]
	pts = binary.BigEndian.Uint64(body[2:10])
	length := binary.BigEndian.Uint32(body[10:WSHeaderLength])
	if int(length) != len(body)-WSHeaderLength {
		return false, false, 0, nil, fmt.Errorf("packet length mismatch: header says %d, frame carries %d",
			length, len(body)-WSHeaderLength)
	}
	return flags&WSFlagConfig != 0, flags&WSFlagKeyFrame != 0, pts, body[WSHeaderLength:], nil
}

// wsHeaderLen is the RFC 6455 header size for an unmasked frame of n bytes.
func wsHeaderLen(n int) int {
	switch {
	case n < 126:
		return 2
	case n <= 0xFFFF:
		return 4
	default:
		return 10
	}
}

// appendWSHeader appends an unmasked FIN frame header for n payload bytes.
// Server-to-client frames are never masked.
func appendWSHeader(dst []byte, opcode byte, n int) []byte {
	dst = append(dst, 0x80|(opcode&0x0F))
	switch {
	case n < 126:
		return append(dst, byte(n))
	case n <= 0xFFFF:
		dst = append(dst, 126)
		return binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, 127)
		return binary.BigEndian.AppendUint64(dst, uint64(n))
	}
}

// wsFrame builds a complete unmasked frame.
func wsFrame(opcode byte, payload []byte) []byte {
	frame := make([]byte, 0, wsHeaderLen(len(payload))+len(payload))
	frame = appendWSHeader(frame, opcode, len(payload))
	return append(frame, payload...)
}

// splitAnnexB returns the NAL units of an Annex-B byte stream, start codes
// removed. Empty units are dropped, matching the Python.
func splitAnnexB(data []byte) [][]byte {
	type start struct{ offset, prefix int }
	var starts []start

	for i := 0; i+3 <= len(data); {
		switch {
		case i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1:
			starts = append(starts, start{i, 4})
			i += 4
		case data[i] == 0 && data[i+1] == 0 && data[i+2] == 1:
			starts = append(starts, start{i, 3})
			i += 3
		default:
			i++
		}
	}

	units := make([][]byte, 0, len(starts))
	for i, s := range starts {
		end := len(data)
		if i+1 < len(starts) {
			end = starts[i+1].offset
		}
		if unit := data[s.offset+s.prefix : end]; len(unit) > 0 {
			units = append(units, unit)
		}
	}
	return units
}

// codecFromConfig derives the WebCodecs codec string from the SPS in a
// configuration packet: "avc1." then profile_idc, constraint flags and level_idc
// as uppercase hex. It falls back to DefaultCodec when no usable SPS is found,
// which is what the widget assumes anyway.
func codecFromConfig(data []byte) string {
	for _, unit := range splitAnnexB(data) {
		if len(unit) >= 4 && unit[0]&0x1F == 7 {
			return fmt.Sprintf("avc1.%02X%02X%02X", unit[1], unit[2], unit[3])
		}
	}
	return DefaultCodec
}
