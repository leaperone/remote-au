package transport

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
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
	defaultMaxGapFrames     = protocol.MaxSampleRate * 3
	udpReadTimeout          = 250 * time.Millisecond
	udpIdleTimeout          = 2 * time.Second
)

type streamKey string

type udpSourceState struct {
	stream   *mixer.Stream
	tracker  frameTracker
	lastSeen time.Time
}

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
	staleFrames      atomic.Uint64
	nextStreamID     atomic.Uint64

	activeMu      sync.Mutex
	activeStreams map[streamKey]string
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
		activeStreams:    make(map[streamKey]string),
		conns:            make(map[net.Conn]struct{}),
	}, nil
}

func (r *Receiver) Run(ctx context.Context, addr string) error {
	if addr == "" {
		addr = ":47000"
	}

	lc := net.ListenConfig{}
	tcpLn, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	tcpAddr, ok := tcpLn.Addr().(*net.TCPAddr)
	if !ok {
		_ = tcpLn.Close()
		return fmt.Errorf("unexpected TCP listener address type %T", tcpLn.Addr())
	}

	udpAddr := udpListenAddrFromTCP(tcpAddr)
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcpLn.Close()
		return fmt.Errorf("listen udp %s: %w", udpAddr, err)
	}

	return r.RunListeners(ctx, tcpLn, udpConn)
}

func (r *Receiver) RunListener(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		return fmt.Errorf("listener is required")
	}
	return r.RunListeners(ctx, ln, nil)
}

func (r *Receiver) RunListeners(ctx context.Context, tcpLn net.Listener, udpConn *net.UDPConn) error {
	if tcpLn == nil {
		return fmt.Errorf("tcp listener is required")
	}

	// Per-run context so any exit (ctx cancel OR a fatal Accept error) tears
	// down sockets and active stream handlers, preventing goroutine/fd leaks
	// outside the normal cancellation path.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	r.logf("listening on tcp %s", tcpLn.Addr())
	if udpConn != nil {
		r.logf("listening on udp %s", udpConn.LocalAddr())
	}

	errCh := make(chan error, 2)
	reportErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
	}

	var loops sync.WaitGroup
	var handlers sync.WaitGroup
	defer func() {
		cancel()
		_ = tcpLn.Close()
		if udpConn != nil {
			_ = udpConn.Close()
		}
		r.closeActive()
		loops.Wait()
		handlers.Wait()
	}()
	go func() {
		<-runCtx.Done()
		_ = tcpLn.Close()
		if udpConn != nil {
			_ = udpConn.Close()
		}
		r.closeActive()
	}()

	pendingHandshakes := make(chan struct{}, maxPendingHandshakes)
	loops.Add(1)
	go func() {
		defer loops.Done()
		reportErr(r.runTCPAcceptLoop(runCtx, tcpLn, pendingHandshakes, &handlers))
	}()

	if udpConn != nil {
		loops.Add(1)
		go func() {
			defer loops.Done()
			reportErr(r.runUDPReceiveLoop(runCtx, udpConn))
		}()
	}

	select {
	case <-ctx.Done():
		cancel()
		return nil
	case err := <-errCh:
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func (r *Receiver) runTCPAcceptLoop(ctx context.Context, ln net.Listener, pendingHandshakes chan struct{}, handlers *sync.WaitGroup) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
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
			r.handleConnection(ctx, conn, func() {
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

func (r *Receiver) StaleCount() uint64 {
	return r.staleFrames.Load()
}

func (r *Receiver) Stats() stats.AggregateSnapshot {
	return r.mixer.Snapshot()
}

func (r *Receiver) handleConnection(ctx context.Context, conn net.Conn, releasePending func()) {
	remote := fmt.Sprint(conn.RemoteAddr())
	active := false
	var key streamKey
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
			r.unregisterStream(key, stream.ID())
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
	if err := r.validateTCPHandshake(hs); err != nil {
		r.logf("rejecting %s: %v", remote, err)
		return
	}
	var ok bool
	key = r.tcpStreamKey(remote)
	stream, ok = r.registerStream(key, hs.Name, remote)
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
	tracker := frameTracker{}
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
		if !r.acceptFrame(remote, &tracker, frame, int(hs.FrameSamples)) {
			continue
		}
		stream.Write(frame.Payload)
	}
}

func (r *Receiver) runUDPReceiveLoop(ctx context.Context, conn *net.UDPConn) error {
	sources := make(map[netip.AddrPort]*udpSourceState)
	defer func() {
		for src, state := range sources {
			r.unregisterStream(r.udpStreamKey(src), state.stream.ID())
		}
	}()

	buf := make([]byte, protocol.MaxUDPDatagramBytes+1)
	nextCleanup := time.Now().Add(udpReadTimeout)
	cleanupIfDue := func(now time.Time) {
		if now.Before(nextCleanup) {
			return
		}
		r.cleanupIdleUDPSources(sources, now)
		nextCleanup = now.Add(udpReadTimeout)
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := conn.SetReadDeadline(time.Now().Add(udpReadTimeout)); err != nil {
			return fmt.Errorf("set udp read deadline: %w", err)
		}

		n, src, err := conn.ReadFromUDPAddrPort(buf)
		now := time.Now()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				cleanupIfDue(now)
				continue
			}
			return fmt.Errorf("read udp datagram: %w", err)
		}
		cleanupIfDue(now)

		datagram, err := protocol.DecodeUDPDatagram(buf[:n])
		if err != nil {
			r.logf("dropping udp datagram from %s: %v", src, err)
			continue
		}

		switch datagram.Type {
		case protocol.DatagramTypeHello:
			r.handleUDPHello(sources, src, datagram.Handshake, now)
		case protocol.DatagramTypeAudio:
			r.handleUDPAudio(sources, src, datagram.Frame, now)
		default:
			r.logf("dropping udp datagram from %s: unsupported type %d", src, datagram.Type)
		}
	}
}

func (r *Receiver) handleUDPHello(sources map[netip.AddrPort]*udpSourceState, src netip.AddrPort, hs protocol.Handshake, now time.Time) {
	remote := src.String()
	if err := r.validateFormat(hs); err != nil {
		r.logf("rejecting udp %s: %v", remote, err)
		return
	}

	if state := sources[src]; state != nil {
		state.lastSeen = now
		return
	}

	key := r.udpStreamKey(src)
	stream, ok := r.registerStream(key, hs.Name, remote)
	if !ok {
		r.logf("rejecting udp %s: maximum active streams reached (%d)", remote, r.maxStreams)
		return
	}
	sources[src] = &udpSourceState{
		stream:   stream,
		lastSeen: now,
	}

	name := stats.SafeDisplayName(hs.Name)
	if name == "" {
		name = "(unnamed)"
	}
	r.logf("udp stream started from %s: %s, %d Hz, %d channel(s)", remote, name, hs.SampleRate, hs.Channels)
}

func (r *Receiver) handleUDPAudio(sources map[netip.AddrPort]*udpSourceState, src netip.AddrPort, frame protocol.Frame, now time.Time) {
	state := sources[src]
	if state == nil {
		r.logf("dropping udp audio from unknown source %s", src)
		return
	}

	payloadLen := len(frame.Payload)
	bytesPerFrame := r.format.BytesPerFrame()
	if bytesPerFrame <= 0 || payloadLen%bytesPerFrame != 0 {
		r.logf("dropping udp audio from %s: payload length %d is not frame-aligned to %d byte frame(s)", src, payloadLen, bytesPerFrame)
		return
	}

	state.lastSeen = now
	packetFrames := payloadLen / bytesPerFrame
	ok, gapFrames := r.acceptTrackedFrame(src.String(), &state.tracker, frame, packetFrames, "udp ")
	if !ok {
		return
	}
	if gapFrames > 0 {
		r.writeSilence(state.stream, gapFrames)
	}
	state.stream.Write(frame.Payload)
}

func (r *Receiver) cleanupIdleUDPSources(sources map[netip.AddrPort]*udpSourceState, now time.Time) {
	for src, state := range sources {
		if now.Sub(state.lastSeen) <= udpIdleTimeout {
			continue
		}
		r.logf("udp stream from %s timed out", src)
		r.unregisterStream(r.udpStreamKey(src), state.stream.ID())
		delete(sources, src)
	}
}

func (r *Receiver) writeSilence(stream *mixer.Stream, frames int) {
	if frames <= 0 {
		return
	}
	bytesPerFrame := r.format.BytesPerFrame()
	if bytesPerFrame <= 0 {
		return
	}
	chunkFrames := max(1, protocol.MaxUDPAudioPayloadBytes/bytesPerFrame)
	silence := make([]byte, chunkFrames*bytesPerFrame)
	for frames > 0 {
		n := min(frames, chunkFrames)
		stream.Write(silence[:n*bytesPerFrame])
		frames -= n
	}
}

func (r *Receiver) acceptFrame(remote string, tracker *frameTracker, frame protocol.Frame, packetFrames int) bool {
	ok, _ := r.acceptTrackedFrame(remote, tracker, frame, packetFrames, "")
	return ok
}

func (r *Receiver) acceptTrackedFrame(remote string, tracker *frameTracker, frame protocol.Frame, packetFrames int, logPrefix string) (bool, int) {
	result := tracker.Accept(frame.Seq, frame.CaptureFrame, packetFrames)
	if result.Stale {
		r.staleFrames.Add(1)
		r.logf("dropping stale %sframe from %s: seq=%d, expected=%d", logPrefix, remote, frame.Seq, result.ExpectedSeq)
		return false, 0
	}
	if result.MissingPackets > 0 {
		r.gaps.Add(result.MissingPackets)
		r.logf("%ssequence gap from %s: missing %d packet(s), expected seq=%d, got seq=%d", logPrefix, remote, result.MissingPackets, result.ExpectedSeq, frame.Seq)
	}

	if result.CaptureGapFrames > 0 {
		r.logf("%scapture frame gap from %s: missing %d frame(s), expected captureFrame=%d, got captureFrame=%d", logPrefix, remote, result.CaptureGapFrames, result.ExpectedCaptureFrame, frame.CaptureFrame)
	} else if result.CaptureMovedBackward {
		r.logf("%scapture frame moved backward from %s: expected captureFrame=%d, got captureFrame=%d", logPrefix, remote, result.ExpectedCaptureFrame, frame.CaptureFrame)
	}

	return true, result.GapFrames
}

type frameTracker struct {
	seen                 bool
	expectedSeq          uint64
	expectedCaptureFrame uint64
	maxGapFrames         int
}

type frameAcceptResult struct {
	ExpectedSeq          uint64
	ExpectedCaptureFrame uint64
	MissingPackets       uint64
	CaptureGapFrames     uint64
	GapFrames            int
	Stale                bool
	CaptureMovedBackward bool
}

func (t *frameTracker) Accept(seq, captureFrame uint64, packetFrames int) frameAcceptResult {
	if !t.seen {
		t.seen = true
		t.expectedSeq = seq + 1
		t.expectedCaptureFrame = advanceCaptureFrame(captureFrame, packetFrames)
		return frameAcceptResult{}
	}

	result := frameAcceptResult{
		ExpectedSeq:          t.expectedSeq,
		ExpectedCaptureFrame: t.expectedCaptureFrame,
	}
	if seq < t.expectedSeq {
		result.Stale = true
		return result
	}
	if seq > t.expectedSeq {
		result.MissingPackets = seq - t.expectedSeq
	}

	if captureFrame > t.expectedCaptureFrame {
		result.CaptureGapFrames = captureFrame - t.expectedCaptureFrame
		result.GapFrames = t.capGapFrames(result.CaptureGapFrames)
	} else if captureFrame < t.expectedCaptureFrame {
		result.CaptureMovedBackward = true
	}
	if result.GapFrames == 0 && result.MissingPackets > 0 {
		result.GapFrames = t.capMissingPacketFrames(result.MissingPackets, packetFrames)
	}

	t.expectedSeq = seq + 1
	t.expectedCaptureFrame = advanceCaptureFrame(captureFrame, packetFrames)
	return result
}

func (t *frameTracker) capMissingPacketFrames(missingPackets uint64, packetFrames int) int {
	if missingPackets == 0 || packetFrames <= 0 {
		return 0
	}
	framesPerPacket := uint64(packetFrames)
	if missingPackets > ^uint64(0)/framesPerPacket {
		return t.capGapFrames(^uint64(0))
	}
	return t.capGapFrames(missingPackets * framesPerPacket)
}

func (t *frameTracker) capGapFrames(gap uint64) int {
	maxGapFrames := t.maxGapFrames
	if maxGapFrames <= 0 {
		maxGapFrames = defaultMaxGapFrames
	}
	if gap > uint64(maxGapFrames) {
		return maxGapFrames
	}
	return int(gap)
}

func advanceCaptureFrame(captureFrame uint64, packetFrames int) uint64 {
	if packetFrames <= 0 {
		return captureFrame
	}
	frames := uint64(packetFrames)
	if ^uint64(0)-captureFrame < frames {
		return ^uint64(0)
	}
	return captureFrame + frames
}

func (r *Receiver) validateFormat(hs protocol.Handshake) error {
	if err := hs.Validate(); err != nil {
		return err
	}
	if int(hs.SampleRate) != r.format.Rate {
		return fmt.Errorf("sample rate mismatch: sender %d, receiver %d", hs.SampleRate, r.format.Rate)
	}
	if int(hs.Channels) != r.format.Channels {
		return fmt.Errorf("channel mismatch: sender %d, receiver %d", hs.Channels, r.format.Channels)
	}
	if hs.Format != protocol.FormatS16LE {
		return fmt.Errorf("format mismatch: sender %d, receiver %d", hs.Format, protocol.FormatS16LE)
	}
	return nil
}

func (r *Receiver) validateTCPHandshake(hs protocol.Handshake) error {
	if err := r.validateFormat(hs); err != nil {
		return err
	}
	if int(hs.FrameSamples) != r.format.FrameSamples {
		return fmt.Errorf("frame sample mismatch: sender %d, receiver %d", hs.FrameSamples, r.format.FrameSamples)
	}
	return nil
}

func (r *Receiver) tcpStreamKey(remote string) streamKey {
	return streamKey(fmt.Sprintf("tcp:%s#%d", remote, r.nextStreamID.Add(1)))
}

func (r *Receiver) udpStreamKey(src netip.AddrPort) streamKey {
	return streamKey("udp:" + src.String())
}

func (r *Receiver) registerStream(key streamKey, name, remote string) (*mixer.Stream, bool) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if _, exists := r.activeStreams[key]; exists {
		return nil, false
	}
	if len(r.activeStreams) >= r.maxStreams {
		return nil, false
	}
	id := string(key)
	stream, err := r.mixer.AddStream(id, name, remote)
	if err != nil {
		r.logf("register stream failed from %s: %v", remote, err)
		return nil, false
	}
	r.activeStreams[key] = id
	return stream, true
}

func (r *Receiver) unregisterStream(key streamKey, id string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if r.activeStreams[key] == id {
		delete(r.activeStreams, key)
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
	opts.HighWatermarkFrames = max(targetFrames, framesForMillis(format.Rate, 80))
	return opts
}

func udpListenAddrFromTCP(addr *net.TCPAddr) *net.UDPAddr {
	if addr == nil {
		return &net.UDPAddr{}
	}
	return &net.UDPAddr{IP: addr.IP, Port: addr.Port, Zone: addr.Zone}
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
