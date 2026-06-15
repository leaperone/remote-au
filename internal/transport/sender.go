package transport

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/protocol"
)

const (
	defaultWriteTimeout = 2 * time.Second
	capturePollInterval = 2 * time.Millisecond
)

type Capture interface {
	Format() audio.Format
	Read(dst []byte) int
}

type SenderOptions struct {
	Address      string
	Capture      Capture
	Name         string
	WriteTimeout time.Duration
	Logf         func(format string, args ...any)
}

func RunSender(ctx context.Context, opts SenderOptions) error {
	if opts.Address == "" {
		return fmt.Errorf("sender address is required")
	}
	if opts.Capture == nil {
		return fmt.Errorf("sender capture is required")
	}
	if opts.Name == "" {
		opts.Name = "remote-au"
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	hs, err := handshakeFromFormat(opts.Capture.Format(), opts.Name)
	if err != nil {
		return err
	}

	var seq uint64
	var captureFrame uint64
	dialer := net.Dialer{Timeout: opts.WriteTimeout}

	if err := ctx.Err(); err != nil {
		return nil
	}

	opts.Logf("connecting to %s", opts.Address)
	conn, err := dialer.DialContext(ctx, "tcp", opts.Address)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	opts.Logf("connected to %s", conn.RemoteAddr())
	err = sendConnection(ctx, conn, opts.Capture, hs, opts.WriteTimeout, &seq, &captureFrame)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func sendConnection(ctx context.Context, conn net.Conn, capture Capture, hs protocol.Handshake, timeout time.Duration, seq, captureFrame *uint64) error {
	defer conn.Close()

	w := bufio.NewWriterSize(conn, 32*1024)
	if err := setWriteDeadline(conn, timeout); err != nil {
		return err
	}
	if err := protocol.WriteHandshake(w, hs); err != nil {
		return err
	}

	packetBytes, err := hs.ExpectedPayloadBytes()
	if err != nil {
		return err
	}
	bytesPerFrame := int(hs.Channels) * audio.BytesPerSampleS16
	packet := make([]byte, packetBytes)
	filled := 0
	packetCaptureFrame := *captureFrame

	for {
		if ctx.Err() != nil {
			return nil
		}

		if filled == 0 {
			packetCaptureFrame = *captureFrame
		}
		n := capture.Read(packet[filled:])
		if n > 0 {
			filled += n
			*captureFrame += uint64(n / bytesPerFrame)
			if filled < len(packet) {
				continue
			}

			frame := protocol.Frame{
				Seq:          *seq,
				CaptureFrame: packetCaptureFrame,
				Payload:      packet,
			}
			*seq = *seq + 1
			if err := setWriteDeadline(conn, timeout); err != nil {
				return err
			}
			if err := protocol.WriteFrame(w, hs, frame); err != nil {
				return err
			}
			filled = 0
			continue
		}

		if !sleepContext(ctx, capturePollInterval) {
			return nil
		}
	}
}

func handshakeFromFormat(format audio.Format, name string) (protocol.Handshake, error) {
	if err := format.Validate(); err != nil {
		return protocol.Handshake{}, err
	}
	if format.Rate > protocol.MaxSampleRate || format.Rate < protocol.MinSampleRate {
		return protocol.Handshake{}, fmt.Errorf("sample rate out of protocol range: %d", format.Rate)
	}
	if format.Channels > protocol.MaxChannels || format.Channels < protocol.MinChannels {
		return protocol.Handshake{}, fmt.Errorf("channel count out of protocol range: %d", format.Channels)
	}
	if format.FrameSamples > protocol.MaxFrameSamples || format.FrameSamples < protocol.MinFrameSamples {
		return protocol.Handshake{}, fmt.Errorf("frame sample count out of protocol range: %d", format.FrameSamples)
	}

	hs := protocol.Handshake{
		Version:      protocol.Version1,
		SampleRate:   uint32(format.Rate),
		Channels:     uint8(format.Channels),
		Format:       protocol.FormatS16LE,
		FrameSamples: uint16(format.FrameSamples),
		Name:         name,
	}
	if err := hs.Validate(); err != nil {
		return protocol.Handshake{}, err
	}
	return hs, nil
}

func setWriteDeadline(conn net.Conn, timeout time.Duration) error {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
