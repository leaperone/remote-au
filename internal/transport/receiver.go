package transport

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/protocol"
)

const (
	defaultReadTimeout      = 5 * time.Second
	defaultHandshakeTimeout = 2 * time.Second
	defaultBufferMillis     = 80
	maxPendingHandshakes    = 16
)

type ReceiverOptions struct {
	Format       audio.Format
	BufferFrames int
	ReadTimeout  time.Duration
	Logf         func(format string, args ...any)
}

type Receiver struct {
	format           audio.Format
	buffer           *pcmBuffer
	readTimeout      time.Duration
	handshakeTimeout time.Duration
	logf             func(format string, args ...any)
	gaps             atomic.Uint64

	activeMu   sync.Mutex
	activeConn net.Conn
}

func NewReceiver(opts ReceiverOptions) (*Receiver, error) {
	if opts.Format == (audio.Format{}) {
		opts.Format = audio.DefaultFormat()
	}
	if err := opts.Format.Validate(); err != nil {
		return nil, err
	}
	if opts.BufferFrames <= 0 {
		opts.BufferFrames = defaultReceiverBufferFrames(opts.Format)
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	bytesPerFrame := opts.Format.BytesPerFrame()
	return &Receiver{
		format:           opts.Format,
		buffer:           newPCMBuffer(opts.BufferFrames*bytesPerFrame, bytesPerFrame),
		readTimeout:      opts.ReadTimeout,
		handshakeTimeout: defaultHandshakeTimeout,
		logf:             opts.Logf,
	}, nil
}

func (r *Receiver) Run(ctx context.Context, addr string) error {
	if addr == "" {
		addr = ":47000"
	}

	// Per-Run context so any exit (ctx cancel OR a fatal Accept error) tears
	// down the listener and any active stream handler, preventing goroutine/fd
	// leaks outside the normal cancellation path.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	lc := net.ListenConfig{}
	ln, err := lc.Listen(runCtx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	r.logf("listening on %s", ln.Addr())
	go func() {
		<-runCtx.Done()
		_ = ln.Close()
		r.closeActive()
	}()

	pendingHandshakes := make(chan struct{}, maxPendingHandshakes)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}

		select {
		case pendingHandshakes <- struct{}{}:
		default:
			r.logf("too many pending handshakes, rejecting %s", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}

		go r.handleConnection(runCtx, conn, func() {
			<-pendingHandshakes
		})
	}
}

func (r *Receiver) Pull(out []byte, frameCount uint32) {
	bytesPerFrame := r.format.BytesPerFrame()
	want := int(frameCount) * bytesPerFrame
	if want > len(out) {
		want = len(out) - len(out)%bytesPerFrame
	}
	if want <= 0 {
		clear(out)
		return
	}

	clear(out[:want])
	n := r.buffer.TryRead(out[:want])
	if n < want {
		clear(out[n:want])
	}
	if want < len(out) {
		clear(out[want:])
	}
}

func (r *Receiver) GapCount() uint64 {
	return r.gaps.Load()
}

func (r *Receiver) handleConnection(ctx context.Context, conn net.Conn, releasePending func()) {
	remote := fmt.Sprint(conn.RemoteAddr())
	active := false
	pending := true
	releasePendingOnce := func() {
		if pending {
			pending = false
			releasePending()
		}
	}
	defer func() {
		releasePendingOnce()
		if active {
			r.clearActive(conn)
		}
		_ = conn.Close()
		if active {
			r.logf("connection from %s closed", remote)
		}
	}()

	reader := bufio.NewReaderSize(conn, 32*1024)
	if err := setReadDeadline(conn, r.handshakeTimeout); err != nil {
		r.logf("connection setup failed from %s: %v", remote, err)
		return
	}

	hs, err := protocol.ReadHandshake(reader)
	if err != nil {
		r.logf("handshake failed from %s: %v", remote, err)
		return
	}
	if err := r.validateHandshake(hs); err != nil {
		r.logf("rejecting %s: %v", remote, err)
		return
	}
	if !r.setActive(conn) {
		r.logf("rejecting %s: another stream is active", remote)
		return
	}
	active = true
	releasePendingOnce()

	r.buffer.Reset()
	name := hs.Name
	if name == "" {
		name = "(unnamed)"
	}
	r.logf("stream started from %s: %s, %d Hz, %d channel(s), %d-frame packets", remote, name, hs.SampleRate, hs.Channels, hs.FrameSamples)

	payloadBuf := make([]byte, 0, r.format.PacketBytes())
	seenFrame := false
	var expectedSeq uint64
	var expectedCaptureFrame uint64
	for {
		if ctx.Err() != nil {
			return
		}
		if err := setReadDeadline(conn, r.readTimeout); err != nil {
			r.logf("read deadline failed: %v", err)
			return
		}

		var frame protocol.Frame
		frame, payloadBuf, err = protocol.ReadFrame(reader, hs, payloadBuf)
		if err != nil {
			r.logf("read frame failed from %s: %v", remote, err)
			return
		}
		if !r.acceptFrame(remote, hs, frame, &seenFrame, &expectedSeq, &expectedCaptureFrame) {
			continue
		}
		r.buffer.Write(frame.Payload)
	}
}

func (r *Receiver) acceptFrame(remote string, hs protocol.Handshake, frame protocol.Frame, seen *bool, expectedSeq, expectedCaptureFrame *uint64) bool {
	if !*seen {
		*seen = true
		*expectedSeq = frame.Seq + 1
		*expectedCaptureFrame = frame.CaptureFrame + uint64(hs.FrameSamples)
		return true
	}

	if frame.Seq < *expectedSeq {
		r.logf("dropping stale frame from %s: seq=%d, expected=%d", remote, frame.Seq, *expectedSeq)
		return false
	}
	if frame.Seq > *expectedSeq {
		missing := frame.Seq - *expectedSeq
		r.gaps.Add(missing)
		r.logf("sequence gap from %s: missing %d packet(s), expected seq=%d, got seq=%d", remote, missing, *expectedSeq, frame.Seq)
	}

	if frame.CaptureFrame > *expectedCaptureFrame {
		r.logf("capture frame gap from %s: missing %d frame(s), expected captureFrame=%d, got captureFrame=%d", remote, frame.CaptureFrame-*expectedCaptureFrame, *expectedCaptureFrame, frame.CaptureFrame)
	} else if frame.CaptureFrame < *expectedCaptureFrame {
		r.logf("capture frame moved backward from %s: expected captureFrame=%d, got captureFrame=%d", remote, *expectedCaptureFrame, frame.CaptureFrame)
	}

	*expectedSeq = frame.Seq + 1
	*expectedCaptureFrame = frame.CaptureFrame + uint64(hs.FrameSamples)
	return true
}

func (r *Receiver) validateHandshake(hs protocol.Handshake) error {
	if err := hs.Validate(); err != nil {
		return err
	}
	if int(hs.SampleRate) != r.format.Rate {
		return fmt.Errorf("sample rate mismatch: sender %d, receiver %d", hs.SampleRate, r.format.Rate)
	}
	if int(hs.Channels) != r.format.Channels {
		return fmt.Errorf("channel mismatch: sender %d, receiver %d", hs.Channels, r.format.Channels)
	}
	if int(hs.FrameSamples) != r.format.FrameSamples {
		return fmt.Errorf("frame sample mismatch: sender %d, receiver %d", hs.FrameSamples, r.format.FrameSamples)
	}
	return nil
}

func defaultReceiverBufferFrames(format audio.Format) int {
	frames := format.Rate * defaultBufferMillis / 1000
	if frames < format.FrameSamples {
		frames = format.FrameSamples
	}
	return frames
}

func (r *Receiver) setActive(conn net.Conn) bool {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeConn != nil {
		return false
	}
	r.activeConn = conn
	return true
}

func (r *Receiver) clearActive(conn net.Conn) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeConn == conn {
		r.activeConn = nil
	}
}

func (r *Receiver) closeActive() {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeConn != nil {
		_ = r.activeConn.Close()
		r.activeConn = nil
	}
}

func setReadDeadline(conn net.Conn, timeout time.Duration) error {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	return nil
}
