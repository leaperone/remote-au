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
	"remote-au/internal/mixer"
	"remote-au/internal/protocol"
	"remote-au/internal/stats"
)

const (
	defaultReadTimeout      = 5 * time.Second
	defaultHandshakeTimeout = 2 * time.Second
	maxPendingHandshakes    = 16
	defaultMaxStreams       = 8
)

type ReceiverOptions struct {
	Format       audio.Format
	BufferFrames int
	ReadTimeout  time.Duration
	MaxStreams   int
	Logf         func(format string, args ...any)
}

type Receiver struct {
	format           audio.Format
	mixer            *mixer.Mixer
	maxStreams       int
	readTimeout      time.Duration
	handshakeTimeout time.Duration
	logf             func(format string, args ...any)
	gaps             atomic.Uint64
	nextStreamID     atomic.Uint64

	activeMu      sync.Mutex
	activeStreams map[net.Conn]string
	conns         map[net.Conn]struct{}
}

func NewReceiver(opts ReceiverOptions) (*Receiver, error) {
	if opts.Format == (audio.Format{}) {
		opts.Format = audio.DefaultFormat()
	}
	if err := opts.Format.Validate(); err != nil {
		return nil, err
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = defaultMaxStreams
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	mix, err := mixer.New(mixer.Options{
		Format: opts.Format,
		Jitter: receiverJitterOptions(opts.Format, opts.BufferFrames),
	})
	if err != nil {
		return nil, err
	}

	return &Receiver{
		format:           opts.Format,
		mixer:            mix,
		maxStreams:       opts.MaxStreams,
		readTimeout:      opts.ReadTimeout,
		handshakeTimeout: defaultHandshakeTimeout,
		logf:             opts.Logf,
		activeStreams:    make(map[net.Conn]string),
		conns:            make(map[net.Conn]struct{}),
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

	r.logf("listening on %s", ln.Addr())
	var handlers sync.WaitGroup
	defer func() {
		cancel()
		_ = ln.Close()
		r.closeActive()
		handlers.Wait()
	}()
	go func() {
		<-runCtx.Done()
		_ = ln.Close()
		r.closeActive()
	}()

	pendingHandshakes := make(chan struct{}, maxPendingHandshakes)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if runCtx.Err() != nil || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		r.trackConn(conn)

		select {
		case pendingHandshakes <- struct{}{}:
		default:
			r.logf("too many pending handshakes, rejecting %s", conn.RemoteAddr())
			r.untrackConn(conn)
			_ = conn.Close()
			continue
		}

		handlers.Add(1)
		go func(conn net.Conn) {
			defer handlers.Done()
			r.handleConnection(runCtx, conn, func() {
				<-pendingHandshakes
			})
		}(conn)
	}
}

func (r *Receiver) Pull(out []byte, frameCount uint32) {
	r.mixer.Read(out, frameCount)
}

func (r *Receiver) GapCount() uint64 {
	return r.gaps.Load()
}

func (r *Receiver) Stats() stats.AggregateSnapshot {
	return r.mixer.Snapshot()
}

func (r *Receiver) handleConnection(ctx context.Context, conn net.Conn, releasePending func()) {
	remote := fmt.Sprint(conn.RemoteAddr())
	active := false
	var stream *mixer.Stream
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
			r.unregisterStream(conn, stream.ID())
		}
		r.untrackConn(conn)
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
	var ok bool
	stream, ok = r.registerStream(conn, hs.Name, remote)
	if !ok {
		r.logf("rejecting %s: maximum active streams reached (%d)", remote, r.maxStreams)
		return
	}
	active = true
	releasePendingOnce()

	name := hs.Name
	name = stats.SafeDisplayName(name)
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
		stream.Write(frame.Payload)
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

func (r *Receiver) registerStream(conn net.Conn, name, remote string) (*mixer.Stream, bool) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if len(r.activeStreams) >= r.maxStreams {
		return nil, false
	}
	id := fmt.Sprintf("%s#%d", remote, r.nextStreamID.Add(1))
	stream, err := r.mixer.AddStream(id, name, remote)
	if err != nil {
		r.logf("register stream failed from %s: %v", remote, err)
		return nil, false
	}
	r.activeStreams[conn] = id
	return stream, true
}

func (r *Receiver) unregisterStream(conn net.Conn, id string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeStreams[conn] == id {
		delete(r.activeStreams, conn)
		r.mixer.RemoveStream(id)
	}
}

func (r *Receiver) closeActive() {
	r.activeMu.Lock()
	conns := make([]net.Conn, 0, len(r.conns))
	for conn := range r.conns {
		conns = append(conns, conn)
	}
	r.activeMu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (r *Receiver) trackConn(conn net.Conn) {
	r.activeMu.Lock()
	r.conns[conn] = struct{}{}
	r.activeMu.Unlock()
}

func (r *Receiver) untrackConn(conn net.Conn) {
	r.activeMu.Lock()
	delete(r.conns, conn)
	r.activeMu.Unlock()
}

func receiverJitterOptions(format audio.Format, targetFrames int) mixer.JitterBufferOptions {
	opts := mixer.JitterBufferOptions{Format: format}
	if targetFrames <= 0 {
		return opts
	}
	opts.TargetFrames = targetFrames
	opts.LowWatermarkFrames = min(targetFrames, framesForMillis(format.Rate, 50))
	opts.HighWatermarkFrames = max(targetFrames, framesForMillis(format.Rate, 80))
	return opts
}

func framesForMillis(rate, millis int) int {
	frames := rate * millis / 1000
	if frames < 1 {
		return 1
	}
	return frames
}

func setReadDeadline(conn net.Conn, timeout time.Duration) error {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	return nil
}
