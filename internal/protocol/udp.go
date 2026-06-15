package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const (
	DatagramTypeHello = 1
	DatagramTypeAudio = 2

	MaxUDPDatagramBytes     = 1500
	MaxUDPAudioPayloadBytes = 960
)

var udpMagic = [4]byte{'R', 'A', 'U', 'U'}

// UDPDatagram is the decoded form of one UDP packet. For audio datagrams,
// Frame.Payload aliases the packet passed to DecodeUDPDatagram; callers must
// copy it before reusing that receive buffer unless the consumer copies it
// synchronously.
type UDPDatagram struct {
	Type      uint8
	Handshake Handshake
	Frame     Frame
}

// AppendUDPHello appends a HELLO datagram to dst and returns the extended
// buffer. The envelope version is the Handshake version.
func AppendUDPHello(dst []byte, h Handshake) ([]byte, error) {
	if h.Version == 0 {
		h.Version = Version1
	}
	if err := h.Validate(); err != nil {
		return dst, err
	}

	packetLen := 6 + 1 + 4 + 1 + 1 + 2 + 2 + len(h.Name)
	if packetLen > MaxUDPDatagramBytes {
		return dst, fmt.Errorf("udp hello datagram too large: %d > %d", packetLen, MaxUDPDatagramBytes)
	}

	dst = appendUDPEnvelope(dst, h.Version, DatagramTypeHello)
	dst = append(dst, h.Flags)
	dst = binary.BigEndian.AppendUint32(dst, h.SampleRate)
	dst = append(dst, h.Channels, h.Format)
	dst = binary.BigEndian.AppendUint16(dst, h.FrameSamples)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(h.Name)))
	dst = append(dst, h.Name...)
	return dst, nil
}

// AppendUDPAudio appends an AUDIO datagram to dst and returns the extended
// buffer. The PCM payload is S16LE and is capped below the Ethernet MTU.
func AppendUDPAudio(dst []byte, f Frame) ([]byte, error) {
	if f.Type == 0 {
		f.Type = FrameTypeAudio
	}
	if f.Type != FrameTypeAudio {
		return dst, fmt.Errorf("unsupported frame type: %d", f.Type)
	}
	if len(f.Payload) == 0 {
		return dst, fmt.Errorf("udp audio payload is empty")
	}
	if len(f.Payload) > MaxUDPAudioPayloadBytes {
		return dst, fmt.Errorf("udp audio payload too large: %d > %d", len(f.Payload), MaxUDPAudioPayloadBytes)
	}

	packetLen := 6 + 8 + 8 + 2 + len(f.Payload)
	if packetLen > MaxUDPDatagramBytes {
		return dst, fmt.Errorf("udp audio datagram too large: %d > %d", packetLen, MaxUDPDatagramBytes)
	}

	dst = appendUDPEnvelope(dst, Version1, DatagramTypeAudio)
	dst = binary.BigEndian.AppendUint64(dst, f.Seq)
	dst = binary.BigEndian.AppendUint64(dst, f.CaptureFrame)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(f.Payload)))
	dst = append(dst, f.Payload...)
	return dst, nil
}

func appendUDPEnvelope(dst []byte, version, datagramType uint8) []byte {
	dst = append(dst, udpMagic[:]...)
	dst = append(dst, version, datagramType)
	return dst
}

func DecodeUDPDatagram(packet []byte) (UDPDatagram, error) {
	if len(packet) > MaxUDPDatagramBytes {
		return UDPDatagram{}, fmt.Errorf("udp datagram too large: %d > %d", len(packet), MaxUDPDatagramBytes)
	}
	if len(packet) < 6 {
		return UDPDatagram{}, fmt.Errorf("udp datagram header truncated: %d < 6", len(packet))
	}
	if !bytes.Equal(packet[:4], udpMagic[:]) {
		return UDPDatagram{}, fmt.Errorf("invalid udp datagram magic")
	}

	version := packet[4]
	if version != Version1 {
		return UDPDatagram{}, fmt.Errorf("unsupported udp datagram version: %d", version)
	}

	switch typ := packet[5]; typ {
	case DatagramTypeHello:
		hs, err := decodeUDPHello(version, packet[6:])
		if err != nil {
			return UDPDatagram{}, err
		}
		return UDPDatagram{Type: typ, Handshake: hs}, nil
	case DatagramTypeAudio:
		frame, err := decodeUDPAudio(packet[6:])
		if err != nil {
			return UDPDatagram{}, err
		}
		return UDPDatagram{Type: typ, Frame: frame}, nil
	default:
		return UDPDatagram{}, fmt.Errorf("unsupported udp datagram type: %d", typ)
	}
}

func decodeUDPHello(version uint8, body []byte) (Handshake, error) {
	const fixedLen = 1 + 4 + 1 + 1 + 2 + 2
	if len(body) < fixedLen {
		return Handshake{}, fmt.Errorf("udp hello truncated: %d < %d", len(body), fixedLen)
	}

	nameLen := binary.BigEndian.Uint16(body[9:11])
	if nameLen > MaxNameLen {
		return Handshake{}, fmt.Errorf("udp hello name too long: %d > %d", nameLen, MaxNameLen)
	}
	if len(body[11:]) != int(nameLen) {
		return Handshake{}, fmt.Errorf("udp hello name length mismatch: got %d byte(s), want %d", len(body[11:]), nameLen)
	}

	hs := Handshake{
		Version:      version,
		Flags:        body[0],
		SampleRate:   binary.BigEndian.Uint32(body[1:5]),
		Channels:     body[5],
		Format:       body[6],
		FrameSamples: binary.BigEndian.Uint16(body[7:9]),
		Name:         string(body[11:]),
	}
	if err := hs.Validate(); err != nil {
		return Handshake{}, err
	}
	return hs, nil
}

func decodeUDPAudio(body []byte) (Frame, error) {
	const fixedLen = 8 + 8 + 2
	if len(body) < fixedLen {
		return Frame{}, fmt.Errorf("udp audio truncated: %d < %d", len(body), fixedLen)
	}

	payloadLen := binary.BigEndian.Uint16(body[16:18])
	if payloadLen == 0 {
		return Frame{}, fmt.Errorf("udp audio payload is empty")
	}
	if payloadLen > MaxUDPAudioPayloadBytes {
		return Frame{}, fmt.Errorf("udp audio payload too large: %d > %d", payloadLen, MaxUDPAudioPayloadBytes)
	}
	payload := body[18:]
	if len(payload) != int(payloadLen) {
		return Frame{}, fmt.Errorf("udp audio payload length mismatch: got %d byte(s), want %d", len(payload), payloadLen)
	}

	return Frame{
		Type:         FrameTypeAudio,
		Seq:          binary.BigEndian.Uint64(body[0:8]),
		CaptureFrame: binary.BigEndian.Uint64(body[8:16]),
		Payload:      payload,
	}, nil
}
