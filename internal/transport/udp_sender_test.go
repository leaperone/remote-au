package transport

import (
	"context"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/protocol"
)

func TestUDPSenderSendsHelloFirstTickerAndChunkedAudio(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	skipIfNetworkPermissionDenied(t, err)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer server.Close()

	format := audio.DefaultFormat()
	capture := &fakeCapture{
		format: format,
		data:   make([]byte, protocol.MaxUDPAudioPayloadBytes*2),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSender(ctx, SenderOptions{
			Address:      server.LocalAddr().String(),
			Capture:      capture,
			Name:         "sender",
			Transport:    TransportUDP,
			WriteTimeout: 200 * time.Millisecond,
			Logf:         func(string, ...any) {},
		})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RunSender: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("RunSender did not stop")
		}
	}()

	first := readUDPDatagram(t, server, time.Second)
	if first.Type != protocol.DatagramTypeHello {
		t.Fatalf("first datagram type = %d, want HELLO", first.Type)
	}
	if first.Handshake.Name != "sender" {
		t.Fatalf("hello name = %q, want sender", first.Handshake.Name)
	}

	audio0 := readUDPDatagram(t, server, time.Second)
	assertUDPAudioDatagram(t, audio0, 0, 0, protocol.MaxUDPAudioPayloadBytes)
	audio1 := readUDPDatagram(t, server, time.Second)
	framesPerChunk := uint64(protocol.MaxUDPAudioPayloadBytes / format.BytesPerFrame())
	assertUDPAudioDatagram(t, audio1, 1, framesPerChunk, protocol.MaxUDPAudioPayloadBytes)

	tickerHello := readUntilUDPDatagramType(t, server, protocol.DatagramTypeHello, 1500*time.Millisecond)
	if tickerHello.Handshake.Name != "sender" {
		t.Fatalf("ticker hello name = %q, want sender", tickerHello.Handshake.Name)
	}
}

func TestUDPWriteErrorClassification(t *testing.T) {
	portUnreachable := &net.OpError{Op: "write", Net: "udp", Err: syscall.ECONNREFUSED}
	if isTransientUDPWriteError(portUnreachable) {
		t.Fatalf("ECONNREFUSED write error classified as transient")
	}
	if isTransientUDPWriteError(net.ErrClosed) {
		t.Fatalf("net.ErrClosed classified as transient")
	}
	wouldBlock := &net.OpError{Op: "write", Net: "udp", Err: syscall.EAGAIN}
	if !isTransientUDPWriteError(wouldBlock) {
		t.Fatalf("EAGAIN write error classified as unrecoverable")
	}
	temporary := temporaryUDPWriteError{}
	if !isTransientUDPWriteError(temporary) {
		t.Fatalf("temporary write error classified as unrecoverable")
	}
	addrNotAvailable := &net.OpError{Op: "write", Net: "udp", Err: syscall.EADDRNOTAVAIL}
	if isTransientUDPWriteError(addrNotAvailable) {
		t.Fatalf("EADDRNOTAVAIL write error classified as transient")
	}
}

type temporaryUDPWriteError struct{}

func (temporaryUDPWriteError) Error() string {
	return "temporary"
}

func (temporaryUDPWriteError) Timeout() bool {
	return false
}

func (temporaryUDPWriteError) Temporary() bool {
	return true
}

type fakeCapture struct {
	mu     sync.Mutex
	format audio.Format
	data   []byte
}

func (c *fakeCapture) Format() audio.Format {
	return c.format
}

func (c *fakeCapture) Read(dst []byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) == 0 {
		return 0
	}
	n := copy(dst, c.data)
	c.data = c.data[n:]
	return n
}

func readUDPDatagram(t *testing.T, conn *net.UDPConn, timeout time.Duration) protocol.UDPDatagram {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, protocol.MaxUDPDatagramBytes+1)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP: %v", err)
	}
	datagram, err := protocol.DecodeUDPDatagram(buf[:n])
	if err != nil {
		t.Fatalf("DecodeUDPDatagram: %v", err)
	}
	return datagram
}

func readUntilUDPDatagramType(t *testing.T, conn *net.UDPConn, typ uint8, timeout time.Duration) protocol.UDPDatagram {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		datagram := readUDPDatagram(t, conn, time.Until(deadline))
		if datagram.Type == typ {
			return datagram
		}
	}
	t.Fatalf("timed out waiting for datagram type %d", typ)
	return protocol.UDPDatagram{}
}

func assertUDPAudioDatagram(t *testing.T, datagram protocol.UDPDatagram, seq, captureFrame uint64, maxPayload int) {
	t.Helper()
	if datagram.Type != protocol.DatagramTypeAudio {
		t.Fatalf("datagram type = %d, want AUDIO", datagram.Type)
	}
	if datagram.Frame.Seq != seq {
		t.Fatalf("seq = %d, want %d", datagram.Frame.Seq, seq)
	}
	if datagram.Frame.CaptureFrame != captureFrame {
		t.Fatalf("captureFrame = %d, want %d", datagram.Frame.CaptureFrame, captureFrame)
	}
	if got := len(datagram.Frame.Payload); got == 0 || got > maxPayload {
		t.Fatalf("payload length = %d, want 1..%d", got, maxPayload)
	}
}
