package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
	"remote-au/internal/protocol"
)

func TestUDPReceiverHelloCreatesStream(t *testing.T) {
	receiver, addr, stop := startTestUDPReceiver(t, ReceiverOptions{})
	defer stop()

	client := dialTestUDP(t, addr)
	defer client.Close()

	sendTestUDPHello(t, client, "udp-one")
	waitForCondition(t, time.Second, "active udp stream", func() bool {
		return receiver.Stats().ActiveStreams == 1
	})
}

func TestUDPReceiverUnknownAudioDropped(t *testing.T) {
	receiver, addr, stop := startTestUDPReceiver(t, ReceiverOptions{})
	defer stop()

	client := dialTestUDP(t, addr)
	defer client.Close()

	sendTestUDPAudio(t, client, 0, 0, testPCM(100, 200))
	time.Sleep(100 * time.Millisecond)
	if got := receiver.Stats().ActiveStreams; got != 0 {
		t.Fatalf("active streams = %d, want 0", got)
	}
}

func TestUDPReceiverInactivityTimeoutRemovesStream(t *testing.T) {
	receiver, addr, stop := startTestUDPReceiver(t, ReceiverOptions{})
	defer stop()

	client := dialTestUDP(t, addr)
	defer client.Close()

	sendTestUDPHello(t, client, "idle")
	waitForCondition(t, time.Second, "active udp stream", func() bool {
		return receiver.Stats().ActiveStreams == 1
	})
	waitForCondition(t, 4*time.Second, "inactive udp stream removal", func() bool {
		return receiver.Stats().ActiveStreams == 0
	})
}

func TestUDPReceiverSeqGapInsertsSilence(t *testing.T) {
	receiver, addr, stop := startTestUDPReceiver(t, ReceiverOptions{BufferFrames: 4})
	defer stop()

	client := dialTestUDP(t, addr)
	defer client.Close()

	sendTestUDPHello(t, client, "gap")
	waitForCondition(t, time.Second, "active udp stream", func() bool {
		return receiver.Stats().ActiveStreams == 1
	})
	sendTestUDPAudio(t, client, 0, 0, testPCM(100, 200))
	sendTestUDPAudio(t, client, 2, 4, testPCM(300, 400))

	waitForQueueFrames(t, receiver, 6)
	out := make([]byte, 6*testReceiverFormat().BytesPerFrame())
	receiver.Pull(out, 6)
	assertPCM(t, out, []int16{100, 200, 0, 0, 300, 400})
	if got := receiver.GapCount(); got != 1 {
		t.Fatalf("gap count = %d, want 1", got)
	}
}

func TestUDPReceiverStaleDuplicateDropped(t *testing.T) {
	receiver, addr, stop := startTestUDPReceiver(t, ReceiverOptions{BufferFrames: 4})
	defer stop()

	client := dialTestUDP(t, addr)
	defer client.Close()

	sendTestUDPHello(t, client, "stale")
	waitForCondition(t, time.Second, "active udp stream", func() bool {
		return receiver.Stats().ActiveStreams == 1
	})
	sendTestUDPAudio(t, client, 0, 0, testPCM(100, 200))
	sendTestUDPAudio(t, client, 0, 0, testPCM(900, 900))
	sendTestUDPAudio(t, client, 1, 2, testPCM(300, 400))

	waitForQueueFrames(t, receiver, 4)
	out := make([]byte, 4*testReceiverFormat().BytesPerFrame())
	receiver.Pull(out, 4)
	assertPCM(t, out, []int16{100, 200, 300, 400})
	if got := receiver.StaleCount(); got != 1 {
		t.Fatalf("stale count = %d, want 1", got)
	}
}

func TestUDPReceiverMaxStreamsSharedRegistry(t *testing.T) {
	receiver, addr, stop := startTestUDPReceiver(t, ReceiverOptions{MaxStreams: 1})
	defer stop()

	first := dialTestUDP(t, addr)
	defer first.Close()
	second := dialTestUDP(t, addr)
	defer second.Close()

	sendTestUDPHello(t, first, "first")
	waitForCondition(t, time.Second, "first udp stream", func() bool {
		return receiver.Stats().ActiveStreams == 1
	})
	sendTestUDPHello(t, second, "second")
	time.Sleep(100 * time.Millisecond)

	snapshot := receiver.Stats()
	if snapshot.ActiveStreams != 1 {
		t.Fatalf("active streams = %d, want 1", snapshot.ActiveStreams)
	}
	if len(snapshot.Streams) != 1 || snapshot.Streams[0].Name != "first" {
		t.Fatalf("streams = %#v, want only first stream", snapshot.Streams)
	}
}

func TestUDPReceiverIdleCleanupRunsDuringContinuousUnknownTraffic(t *testing.T) {
	const maxStreams = 2
	receiver, addr, stop := startTestUDPReceiver(t, ReceiverOptions{MaxStreams: maxStreams})
	defer stop()

	for i := 0; i < maxStreams; i++ {
		client := dialTestUDP(t, addr)
		defer client.Close()
		sendTestUDPHello(t, client, "idle")
	}
	waitForCondition(t, time.Second, "all udp stream slots occupied", func() bool {
		return receiver.Stats().ActiveStreams == maxStreams
	})

	noise := dialTestUDP(t, addr)
	defer noise.Close()
	noisePacket, err := protocol.AppendUDPAudio(nil, protocol.Frame{
		Seq:          99,
		CaptureFrame: 99,
		Payload:      testPCM(1, 2),
	})
	if err != nil {
		t.Fatalf("AppendUDPAudio noise: %v", err)
	}
	stopNoise := make(chan struct{})
	noiseDone := make(chan struct{})
	go func() {
		defer close(noiseDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopNoise:
				return
			case <-ticker.C:
				_, _ = noise.Write(noisePacket)
			}
		}
	}()
	defer func() {
		close(stopNoise)
		<-noiseDone
	}()

	waitForCondition(t, udpIdleTimeout+2*time.Second, "idle udp cleanup under continuous unrelated traffic", func() bool {
		return receiver.Stats().ActiveStreams == 0
	})

	fresh := dialTestUDP(t, addr)
	defer fresh.Close()
	sendTestUDPHello(t, fresh, "fresh")
	waitForCondition(t, time.Second, "fresh udp stream after idle cleanup", func() bool {
		snapshot := receiver.Stats()
		return snapshot.ActiveStreams == 1 && len(snapshot.Streams) == 1 && snapshot.Streams[0].Name == "fresh"
	})
}

func TestReceiverRunBindsUDPToTCPSelectedPort(t *testing.T) {
	addr := reserveTestTCPAddr(t)

	receiver, err := NewReceiver(ReceiverOptions{
		Format:       testReceiverFormat(),
		BufferFrames: 4,
		Logger:       logging.Nop(),
	})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- receiver.Run(ctx, addr)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Receiver.Run: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("Receiver.Run did not stop")
		}
	}()

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	client := dialTestUDP(t, udpAddr)
	defer client.Close()

	helloPacket := testUDPHelloPacket(t, "run-udp")
	deadline := time.Now().Add(time.Second)
	var lastWriteErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			skipIfNetworkPermissionDenied(t, err)
			if err != nil {
				t.Fatalf("Receiver.Run exited during startup: %v", err)
			}
			t.Fatal("Receiver.Run exited during startup")
		default:
		}
		if receiver.Stats().ActiveStreams == 1 {
			return
		}
		if _, err := client.Write(helloPacket); err != nil {
			skipIfNetworkPermissionDenied(t, err)
			lastWriteErr = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if receiver.Stats().ActiveStreams == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastWriteErr != nil {
		t.Fatalf("timed out waiting for udp stream through Receiver.Run; last udp write error: %v", lastWriteErr)
	}
	t.Fatalf("timed out waiting for udp stream through Receiver.Run")
}

func startTestUDPReceiver(t *testing.T, opts ReceiverOptions) (*Receiver, *net.UDPAddr, func()) {
	t.Helper()

	format := testReceiverFormat()
	opts.Format = format
	if opts.BufferFrames == 0 {
		opts.BufferFrames = 4
	}
	opts.Logger = logging.Nop()
	receiver, err := NewReceiver(opts)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	skipIfNetworkPermissionDenied(t, err)
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	tcpAddr := tcpLn.Addr().(*net.TCPAddr)
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: tcpAddr.IP, Port: tcpAddr.Port})
	if err != nil {
		_ = tcpLn.Close()
		skipIfNetworkPermissionDenied(t, err)
		t.Fatalf("listen udp: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- receiver.RunListeners(ctx, tcpLn, udpConn)
	}()

	stop := func() {
		cancel()
		_ = tcpLn.Close()
		_ = udpConn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RunListeners: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("RunListeners did not stop")
		}
	}
	return receiver, udpConn.LocalAddr().(*net.UDPAddr), stop
}

func reserveTestTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	skipIfNetworkPermissionDenied(t, err)
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close tcp reservation: %v", err)
	}
	return addr
}

func testReceiverFormat() audio.Format {
	return audio.Format{Rate: 8000, Channels: 1, FrameSamples: 4}
}

func dialTestUDP(t *testing.T, addr *net.UDPAddr) *net.UDPConn {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, addr)
	skipIfNetworkPermissionDenied(t, err)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	return conn
}

func skipIfNetworkPermissionDenied(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, os.ErrPermission) {
		t.Skipf("local network sockets are not permitted in this environment: %v", err)
	}
}

func sendTestUDPHello(t *testing.T, conn *net.UDPConn, name string) {
	t.Helper()
	packet := testUDPHelloPacket(t, name)
	if _, err := conn.Write(packet); err != nil {
		t.Fatalf("write udp hello: %v", err)
	}
}

func testUDPHelloPacket(t *testing.T, name string) []byte {
	t.Helper()
	hs := protocol.Handshake{
		Version:      protocol.Version1,
		SampleRate:   uint32(testReceiverFormat().Rate),
		Channels:     uint8(testReceiverFormat().Channels),
		Format:       protocol.FormatS16LE,
		FrameSamples: 17,
		Name:         name,
	}
	packet, err := protocol.AppendUDPHello(nil, hs)
	if err != nil {
		t.Fatalf("AppendUDPHello: %v", err)
	}
	return packet
}

func sendTestUDPAudio(t *testing.T, conn *net.UDPConn, seq, captureFrame uint64, payload []byte) {
	t.Helper()
	packet, err := protocol.AppendUDPAudio(nil, protocol.Frame{
		Seq:          seq,
		CaptureFrame: captureFrame,
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("AppendUDPAudio: %v", err)
	}
	if _, err := conn.Write(packet); err != nil {
		t.Fatalf("write udp audio: %v", err)
	}
}

func testPCM(samples ...int16) []byte {
	pcm := make([]byte, len(samples)*audio.BytesPerSampleS16)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	return pcm
}

func assertPCM(t *testing.T, got []byte, want []int16) {
	t.Helper()
	if len(got) != len(want)*audio.BytesPerSampleS16 {
		t.Fatalf("pcm length = %d, want %d", len(got), len(want)*audio.BytesPerSampleS16)
	}
	for i, sample := range want {
		gotSample := int16(binary.LittleEndian.Uint16(got[i*2:]))
		if gotSample != sample {
			t.Fatalf("sample[%d] = %d, want %d", i, gotSample, sample)
		}
	}
}

func waitForQueueFrames(t *testing.T, receiver *Receiver, frames int) {
	t.Helper()
	waitForCondition(t, time.Second, "queued audio frames", func() bool {
		snapshot := receiver.Stats()
		return len(snapshot.Streams) == 1 && snapshot.Streams[0].QueueSize >= frames
	})
}

func waitForCondition(t *testing.T, timeout time.Duration, name string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", name)
}
