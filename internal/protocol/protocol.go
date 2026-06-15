package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	Version1 = 1

	FormatS16LE = 1

	SupportedFlags uint8 = 0

	FrameTypeAudio = 1

	MaxNameLen      = 255
	MaxPayloadBytes = 256 * 1024

	MinSampleRate   = 8000
	MaxSampleRate   = 192000
	MinChannels     = 1
	MaxChannels     = 8
	MinFrameSamples = 1
	MaxFrameSamples = 4096
)

var magic = [4]byte{'R', 'A', 'U', '1'}

type Handshake struct {
	Version      uint8
	Flags        uint8
	SampleRate   uint32
	Channels     uint8
	Format       uint8
	FrameSamples uint16
	Name         string
}

type Frame struct {
	Type         uint8
	Seq          uint64
	CaptureFrame uint64
	Payload      []byte
}

func WriteHandshake(w *bufio.Writer, h Handshake) error {
	if h.Version == 0 {
		h.Version = Version1
	}
	if err := h.Validate(); err != nil {
		return err
	}

	var header [16]byte
	copy(header[:4], magic[:])
	header[4] = h.Version
	header[5] = h.Flags
	binary.BigEndian.PutUint32(header[6:10], h.SampleRate)
	header[10] = h.Channels
	header[11] = h.Format
	binary.BigEndian.PutUint16(header[12:14], h.FrameSamples)
	binary.BigEndian.PutUint16(header[14:16], uint16(len(h.Name)))

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("write handshake header: %w", err)
	}
	if _, err := w.WriteString(h.Name); err != nil {
		return fmt.Errorf("write handshake name: %w", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush handshake: %w", err)
	}
	return nil
}

func ReadHandshake(r *bufio.Reader) (Handshake, error) {
	var header [16]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Handshake{}, fmt.Errorf("read handshake header: %w", err)
	}
	if !bytes.Equal(header[:4], magic[:]) {
		return Handshake{}, fmt.Errorf("invalid handshake magic")
	}

	nameLen := binary.BigEndian.Uint16(header[14:16])
	if nameLen > MaxNameLen {
		return Handshake{}, fmt.Errorf("handshake name too long: %d > %d", nameLen, MaxNameLen)
	}

	name := make([]byte, int(nameLen))
	if len(name) > 0 {
		if _, err := io.ReadFull(r, name); err != nil {
			return Handshake{}, fmt.Errorf("read handshake name: %w", err)
		}
	}

	h := Handshake{
		Version:      header[4],
		Flags:        header[5],
		SampleRate:   binary.BigEndian.Uint32(header[6:10]),
		Channels:     header[10],
		Format:       header[11],
		FrameSamples: binary.BigEndian.Uint16(header[12:14]),
		Name:         string(name),
	}
	if err := h.Validate(); err != nil {
		return Handshake{}, err
	}
	return h, nil
}

func WriteFrame(w *bufio.Writer, h Handshake, f Frame) error {
	expected, err := h.ExpectedPayloadBytes()
	if err != nil {
		return err
	}
	if f.Type == 0 {
		f.Type = FrameTypeAudio
	}
	if f.Type != FrameTypeAudio {
		return fmt.Errorf("unsupported frame type: %d", f.Type)
	}
	if len(f.Payload) != expected {
		return fmt.Errorf("invalid audio payload length: got %d, want %d", len(f.Payload), expected)
	}

	var header [21]byte
	header[0] = f.Type
	binary.BigEndian.PutUint64(header[1:9], f.Seq)
	binary.BigEndian.PutUint64(header[9:17], f.CaptureFrame)
	binary.BigEndian.PutUint32(header[17:21], uint32(len(f.Payload)))

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	if _, err := w.Write(f.Payload); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush frame: %w", err)
	}
	return nil
}

func ReadFrame(r *bufio.Reader, h Handshake, payloadBuf []byte) (Frame, []byte, error) {
	expected, err := h.ExpectedPayloadBytes()
	if err != nil {
		return Frame{}, payloadBuf, err
	}

	var header [21]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, payloadBuf, fmt.Errorf("read frame header: %w", err)
	}
	if header[0] != FrameTypeAudio {
		return Frame{}, payloadBuf, fmt.Errorf("unsupported frame type: %d", header[0])
	}

	payloadLen := binary.BigEndian.Uint32(header[17:21])
	if payloadLen > MaxPayloadBytes {
		return Frame{}, payloadBuf, fmt.Errorf("audio payload too large: %d > %d", payloadLen, MaxPayloadBytes)
	}
	if int(payloadLen) != expected {
		return Frame{}, payloadBuf, fmt.Errorf("invalid audio payload length: got %d, want %d", payloadLen, expected)
	}

	if cap(payloadBuf) < int(payloadLen) {
		payloadBuf = make([]byte, int(payloadLen))
	}
	payload := payloadBuf[:int(payloadLen)]
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, payloadBuf, fmt.Errorf("read frame payload: %w", err)
	}

	return Frame{
		Type:         header[0],
		Seq:          binary.BigEndian.Uint64(header[1:9]),
		CaptureFrame: binary.BigEndian.Uint64(header[9:17]),
		Payload:      payload,
	}, payloadBuf, nil
}

func (h Handshake) Validate() error {
	if h.Version != Version1 {
		return fmt.Errorf("unsupported protocol version: %d", h.Version)
	}
	if unsupported := h.Flags &^ SupportedFlags; unsupported != 0 {
		return fmt.Errorf("unsupported handshake flags: 0x%02x", unsupported)
	}
	if h.Format != FormatS16LE {
		return fmt.Errorf("unsupported audio format: %d", h.Format)
	}
	if h.SampleRate < MinSampleRate || h.SampleRate > MaxSampleRate {
		return fmt.Errorf("sample rate out of range: %d", h.SampleRate)
	}
	if h.Channels < MinChannels || h.Channels > MaxChannels {
		return fmt.Errorf("channel count out of range: %d", h.Channels)
	}
	if h.FrameSamples < MinFrameSamples || h.FrameSamples > MaxFrameSamples {
		return fmt.Errorf("frame sample count out of range: %d", h.FrameSamples)
	}
	if len(h.Name) > MaxNameLen {
		return fmt.Errorf("handshake name too long: %d > %d", len(h.Name), MaxNameLen)
	}
	if _, err := h.ExpectedPayloadBytes(); err != nil {
		return err
	}
	return nil
}

func (h Handshake) ExpectedPayloadBytes() (int, error) {
	samples := uint64(h.FrameSamples) * uint64(h.Channels)
	maxInt := uint64(int(^uint(0) >> 1))
	if samples > maxInt/2 {
		return 0, fmt.Errorf("audio payload length overflows int")
	}
	bytes := samples * 2
	if bytes > MaxPayloadBytes {
		return 0, fmt.Errorf("audio payload exceeds maximum: %d > %d", bytes, MaxPayloadBytes)
	}
	return int(bytes), nil
}
