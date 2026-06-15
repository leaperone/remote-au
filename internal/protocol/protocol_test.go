package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestHandshakeRoundTrip(t *testing.T) {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)

	in := Handshake{
		Version:      Version1,
		SampleRate:   48000,
		Channels:     2,
		Format:       FormatS16LE,
		FrameSamples: 480,
		Name:         "sender",
	}
	if err := WriteHandshake(w, in); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	out, err := ReadHandshake(bufio.NewReader(&b))
	if err != nil {
		t.Fatalf("ReadHandshake: %v", err)
	}
	if out != in {
		t.Fatalf("handshake mismatch: got %#v want %#v", out, in)
	}
}

func TestReadHandshakeRejectsOversizedNameBeforeAlloc(t *testing.T) {
	var b bytes.Buffer
	header := make([]byte, 16)
	copy(header[:4], magic[:])
	header[4] = Version1
	binary.BigEndian.PutUint32(header[6:10], 48000)
	header[10] = 2
	header[11] = FormatS16LE
	binary.BigEndian.PutUint16(header[12:14], 480)
	binary.BigEndian.PutUint16(header[14:16], MaxNameLen+1)
	b.Write(header)

	_, err := ReadHandshake(bufio.NewReader(&b))
	if err == nil || !strings.Contains(err.Error(), "name too long") {
		t.Fatalf("expected name length error, got %v", err)
	}
}

func TestReadHandshakeRejectsUnsupportedFlags(t *testing.T) {
	h := Handshake{
		Version:      Version1,
		Flags:        0x01,
		SampleRate:   48000,
		Channels:     2,
		Format:       FormatS16LE,
		FrameSamples: 480,
	}
	if err := h.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported handshake flags") {
		t.Fatalf("expected unsupported flags validation error, got %v", err)
	}

	var b bytes.Buffer
	header := make([]byte, 16)
	copy(header[:4], magic[:])
	header[4] = Version1
	header[5] = 0x01
	binary.BigEndian.PutUint32(header[6:10], 48000)
	header[10] = 2
	header[11] = FormatS16LE
	binary.BigEndian.PutUint16(header[12:14], 480)
	b.Write(header)

	_, err := ReadHandshake(bufio.NewReader(&b))
	if err == nil || !strings.Contains(err.Error(), "unsupported handshake flags") {
		t.Fatalf("expected unsupported flags read error, got %v", err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	h := Handshake{
		Version:      Version1,
		SampleRate:   48000,
		Channels:     2,
		Format:       FormatS16LE,
		FrameSamples: 4,
	}
	payload := []byte("abcdefghijklmnop")

	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	if err := WriteFrame(w, h, Frame{Seq: 7, CaptureFrame: 16, Payload: payload}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	frame, scratch, err := ReadFrame(bufio.NewReader(&b), h, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if frame.Seq != 7 || frame.CaptureFrame != 16 || string(frame.Payload) != string(payload) {
		t.Fatalf("frame mismatch: %#v", frame)
	}
	if len(scratch) != len(payload) {
		t.Fatalf("scratch length = %d, want %d", len(scratch), len(payload))
	}
}

func TestReadFrameRejectsInvalidPayloadLengthBeforeRead(t *testing.T) {
	h := Handshake{
		Version:      Version1,
		SampleRate:   48000,
		Channels:     2,
		Format:       FormatS16LE,
		FrameSamples: 480,
	}

	var b bytes.Buffer
	header := make([]byte, 21)
	header[0] = FrameTypeAudio
	binary.BigEndian.PutUint32(header[17:21], MaxPayloadBytes+1)
	b.Write(header)

	_, _, err := ReadFrame(bufio.NewReader(&b), h, nil)
	if err == nil || !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("expected payload length error, got %v", err)
	}
}
