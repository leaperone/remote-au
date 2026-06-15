package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/protocol"
)

const (
	defaultWriteTimeout = 2 * time.Second
	capturePollInterval = 2 * time.Millisecond
	helloInterval       = time.Second
)

type Capture interface {
	Format() audio.Format
	Read(dst []byte) int
}

type SenderTransport string

const (
	TransportUDP SenderTransport = "udp"
	TransportTCP SenderTransport = "tcp"
)

type SenderOptions struct {
	Address      string
	Capture      Capture
	Name         string
	Transport    SenderTransport
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
	if opts.Transport == "" {
		opts.Transport = TransportUDP
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

	switch opts.Transport {
	case TransportUDP:
		return runUDPSender(ctx, opts, hs)
	case TransportTCP:
		return runTCPSender(ctx, opts, hs)
	default:
		return fmt.Errorf("unsupported sender transport %q (want udp or tcp)", opts.Transport)
	}
}

func runTCPSender(ctx context.Context, opts SenderOptions, hs protocol.Handshake) error {
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

func runUDPSender(ctx context.Context, opts SenderOptions, hs protocol.Handshake) error {
	dialer := net.Dialer{Timeout: opts.WriteTimeout}
	if err := ctx.Err(); err != nil {
		return nil
	}

	opts.Logf("connecting to %s over udp", opts.Address)
	conn, err := dialer.DialContext(ctx, "udp", opts.Address)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer conn.Close()
	opts.Logf("connected to %s over udp", conn.RemoteAddr())

	if err := writeUDPHello(conn, hs, opts.WriteTimeout); err != nil {
		cont, err := handleUDPWriteError(ctx, opts, "udp hello", err)
		if !cont {
			return err
		}
	}
	helloTicker := time.NewTicker(helloInterval)
	defer helloTicker.Stop()

	bytesPerFrame := opts.Capture.Format().BytesPerFrame()
	if bytesPerFrame <= 0 {
		return fmt.Errorf("invalid capture format: %d bytes per frame", bytesPerFrame)
	}
	chunkFrames := max(1, protocol.MaxUDPAudioPayloadBytes/bytesPerFrame)
	chunkBytes := chunkFrames * bytesPerFrame
	packet := make([]byte, chunkBytes)
	filled := 0
	var seq uint64
	var captureFrame uint64
	packetCaptureFrame := captureFrame

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-helloTicker.C:
			if err := writeUDPHello(conn, hs, opts.WriteTimeout); err != nil {
				cont, err := handleUDPWriteError(ctx, opts, "udp hello", err)
				if !cont {
					return err
				}
			}
		default:
		}

		if filled == 0 {
			packetCaptureFrame = captureFrame
		}
		n := opts.Capture.Read(packet[filled:])
		if n > 0 {
			filled += n
			captureFrame += uint64(n / bytesPerFrame)
			if filled < len(packet) {
				continue
			}

			frame := protocol.Frame{
				Seq:          seq,
				CaptureFrame: packetCaptureFrame,
				Payload:      packet,
			}
			seq++
			if err := writeUDPAudio(conn, frame, opts.WriteTimeout); err != nil {
				cont, err := handleUDPWriteError(ctx, opts, "udp audio", err)
				if !cont {
					return err
				}
			}
			filled = 0
			continue
		}

		if !sleepContext(ctx, capturePollInterval) {
			return nil
		}
	}
}

func handleUDPWriteError(ctx context.Context, opts SenderOptions, what string, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, nil
	}
	if !isTransientUDPWriteError(err) {
		return false, err
	}
	opts.Logf("%s write failed transiently: %v", what, err)
	return true, nil
}

func isTransientUDPWriteError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ENETDOWN) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EAGAIN)
}

func writeUDPHello(conn net.Conn, hs protocol.Handshake, timeout time.Duration) error {
	packet, err := protocol.AppendUDPHello(nil, hs)
	if err != nil {
		return err
	}
	if err := setWriteDeadline(conn, timeout); err != nil {
		return err
	}
	_, err = conn.Write(packet)
	return err
}

func writeUDPAudio(conn net.Conn, frame protocol.Frame, timeout time.Duration) error {
	packet, err := protocol.AppendUDPAudio(nil, frame)
	if err != nil {
		return err
	}
	if err := setWriteDeadline(conn, timeout); err != nil {
		return err
	}
	_, err = conn.Write(packet)
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
