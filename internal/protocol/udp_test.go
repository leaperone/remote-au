package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestUDPHelloRoundTrip(t *testing.T) {
	in := Handshake{
		Version:      Version1,
		SampleRate:   48000,
		Channels:     2,
		Format:       FormatS16LE,
		FrameSamples: 240,
		Name:         "sender",
	}

	packet, err := AppendUDPHello(nil, in)
	if err != nil {
		t.Fatalf("AppendUDPHello: %v", err)
	}
	out, err := DecodeUDPDatagram(packet)
	if err != nil {
		t.Fatalf("DecodeUDPDatagram: %v", err)
	}
	if out.Type != DatagramTypeHello {
		t.Fatalf("type = %d, want %d", out.Type, DatagramTypeHello)
	}
	if out.Handshake != in {
		t.Fatalf("handshake mismatch: got %#v want %#v", out.Handshake, in)
	}
}

func TestUDPAudioRoundTrip(t *testing.T) {
	payload := []byte("abcdefghijklmnop")
	in := Frame{
		Seq:          7,
		CaptureFrame: 16,
		Payload:      payload,
	}

	packet, err := AppendUDPAudio(nil, in)
	if err != nil {
		t.Fatalf("AppendUDPAudio: %v", err)
	}
	out, err := DecodeUDPDatagram(packet)
	if err != nil {
		t.Fatalf("DecodeUDPDatagram: %v", err)
	}
	if out.Type != DatagramTypeAudio {
		t.Fatalf("type = %d, want %d", out.Type, DatagramTypeAudio)
	}
	if out.Frame.Type != FrameTypeAudio || out.Frame.Seq != in.Seq || out.Frame.CaptureFrame != in.CaptureFrame {
		t.Fatalf("frame metadata mismatch: %#v", out.Frame)
	}
	if !bytes.Equal(out.Frame.Payload, payload) {
		t.Fatalf("payload mismatch: got %q want %q", out.Frame.Payload, payload)
	}
	if len(out.Frame.Payload) > 0 && &out.Frame.Payload[0] != &packet[24] {
		t.Fatalf("payload should alias the datagram buffer")
	}
}

func TestDecodeUDPDatagramRejectsOversizedPacket(t *testing.T) {
	_, err := DecodeUDPDatagram(make([]byte, MaxUDPDatagramBytes+1))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected oversized error, got %v", err)
	}
}

func TestDecodeUDPDatagramRejectsTruncatedHeaderAndFields(t *testing.T) {
	hello, err := AppendUDPHello(nil, Handshake{
		Version:      Version1,
		SampleRate:   48000,
		Channels:     2,
		Format:       FormatS16LE,
		FrameSamples: 240,
		Name:         "sender",
	})
	if err != nil {
		t.Fatalf("AppendUDPHello: %v", err)
	}
	audio, err := AppendUDPAudio(nil, Frame{
		Seq:          3,
		CaptureFrame: 240,
		Payload:      []byte("abcd"),
	})
	if err != nil {
		t.Fatalf("AppendUDPAudio: %v", err)
	}

	for n := 0; n < len(hello); n++ {
		if _, err := DecodeUDPDatagram(hello[:n]); err == nil {
			t.Fatalf("DecodeUDPDatagram accepted truncated hello length %d", n)
		}
	}
	for n := 0; n < len(audio); n++ {
		if _, err := DecodeUDPDatagram(audio[:n]); err == nil {
			t.Fatalf("DecodeUDPDatagram accepted truncated audio length %d", n)
		}
	}
}

func TestDecodeUDPDatagramRejectsBadMagicVersionAndType(t *testing.T) {
	packet, err := AppendUDPHello(nil, Handshake{
		Version:      Version1,
		SampleRate:   48000,
		Channels:     2,
		Format:       FormatS16LE,
		FrameSamples: 240,
	})
	if err != nil {
		t.Fatalf("AppendUDPHello: %v", err)
	}

	badMagic := append([]byte(nil), packet...)
	badMagic[0] = 'X'
	if _, err := DecodeUDPDatagram(badMagic); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("expected magic error, got %v", err)
	}

	badVersion := append([]byte(nil), packet...)
	badVersion[4] = Version1 + 1
	if _, err := DecodeUDPDatagram(badVersion); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected version error, got %v", err)
	}

	badType := append([]byte(nil), packet...)
	badType[5] = 99
	if _, err := DecodeUDPDatagram(badType); err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("expected type error, got %v", err)
	}
}

func TestDecodeUDPDatagramRejectsOversizedHelloNameBeforeAlloc(t *testing.T) {
	packet := append([]byte{'R', 'A', 'U', 'U', Version1, DatagramTypeHello}, make([]byte, 11)...)
	packet[6] = 0
	binary.BigEndian.PutUint32(packet[7:11], 48000)
	packet[11] = 2
	packet[12] = FormatS16LE
	binary.BigEndian.PutUint16(packet[13:15], 240)
	binary.BigEndian.PutUint16(packet[15:17], MaxNameLen+1)

	_, err := DecodeUDPDatagram(packet)
	if err == nil || !strings.Contains(err.Error(), "name too long") {
		t.Fatalf("expected name length error, got %v", err)
	}
}

func TestDecodeUDPDatagramRejectsPayloadLengthMismatch(t *testing.T) {
	packet, err := AppendUDPAudio(nil, Frame{Payload: []byte("abcd")})
	if err != nil {
		t.Fatalf("AppendUDPAudio: %v", err)
	}

	short := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(short[22:24], 5)
	if _, err := DecodeUDPDatagram(short); err == nil || !strings.Contains(err.Error(), "length mismatch") {
		t.Fatalf("expected short payload mismatch, got %v", err)
	}

	trailing := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(trailing[22:24], 3)
	if _, err := DecodeUDPDatagram(trailing); err == nil || !strings.Contains(err.Error(), "length mismatch") {
		t.Fatalf("expected trailing payload mismatch, got %v", err)
	}
}

func TestDecodeUDPDatagramRejectsZeroPayload(t *testing.T) {
	if _, err := AppendUDPAudio(nil, Frame{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected append empty payload error, got %v", err)
	}

	packet := append([]byte{'R', 'A', 'U', 'U', Version1, DatagramTypeAudio}, make([]byte, 18)...)
	if _, err := DecodeUDPDatagram(packet); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected decode empty payload error, got %v", err)
	}
}
