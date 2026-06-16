package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"remote-au/internal/audio"
)

var errForcedWrite = errors.New("forced write error")

func TestRunSenderReconnectsTCPWriteErrorWithGrowingBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials int
	var sleeps []time.Duration
	err := RunSender(ctx, SenderOptions{
		Resolve: func(context.Context) (string, error) {
			return "receiver:47000", nil
		},
		Capture:               &streamingFakeCapture{format: audio.DefaultFormat()},
		Transport:             TransportTCP,
		WriteTimeout:          time.Second,
		ReconnectMinDelay:     10 * time.Millisecond,
		ReconnectMaxDelay:     80 * time.Millisecond,
		ReconnectHealthyAfter: time.Second,
		Logf:                  func(string, ...any) {},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return &scriptedConn{
				write: func(int, []byte) (int, error) {
					return 0, errForcedWrite
				},
			}, nil
		},
		sleep: func(_ context.Context, d time.Duration) bool {
			sleeps = append(sleeps, d)
			if len(sleeps) == 2 {
				cancel()
				return false
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("RunSender: %v", err)
	}
	if dials != 2 {
		t.Fatalf("dials=%d, want 2", dials)
	}
	wantSleeps := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if !reflect.DeepEqual(sleeps, wantSleeps) {
		t.Fatalf("sleeps=%v, want %v", sleeps, wantSleeps)
	}
}

func TestRunSenderResetsBackoffOnlyAfterHealthyAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := fakeClock{t: time.Unix(0, 0)}
	var dials int
	var sleeps []time.Duration
	err := RunSender(ctx, SenderOptions{
		Resolve: func(context.Context) (string, error) {
			return "receiver:47000", nil
		},
		Capture:               &streamingFakeCapture{format: audio.DefaultFormat()},
		Transport:             TransportTCP,
		WriteTimeout:          time.Second,
		ReconnectMinDelay:     10 * time.Millisecond,
		ReconnectMaxDelay:     80 * time.Millisecond,
		ReconnectHealthyAfter: 2 * time.Second,
		Logf:                  func(string, ...any) {},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dials++
			attempt := dials
			return &scriptedConn{
				write: func(writeNum int, b []byte) (int, error) {
					switch attempt {
					case 1, 2:
						clock.advance(time.Second)
						return 0, errForcedWrite
					case 3:
						if writeNum == 1 {
							return len(b), nil
						}
						clock.advance(3 * time.Second)
						return 0, errForcedWrite
					default:
						return 0, errForcedWrite
					}
				},
			}, nil
		},
		now: clock.now,
		sleep: func(_ context.Context, d time.Duration) bool {
			sleeps = append(sleeps, d)
			if len(sleeps) == 3 {
				cancel()
				return false
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("RunSender: %v", err)
	}
	if dials != 3 {
		t.Fatalf("dials=%d, want 3", dials)
	}
	wantSleeps := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 10 * time.Millisecond}
	if !reflect.DeepEqual(sleeps, wantSleeps) {
		t.Fatalf("sleeps=%v, want %v", sleeps, wantSleeps)
	}
}

func TestRunSenderRetriesResolverErrorsUntilAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var resolverCalls int
	var dials int
	var sleeps []time.Duration
	err := RunSender(ctx, SenderOptions{
		Resolve: func(context.Context) (string, error) {
			resolverCalls++
			if resolverCalls < 3 {
				return "", errors.New("no receiver yet")
			}
			return "receiver:47000", nil
		},
		Capture:               &streamingFakeCapture{format: audio.DefaultFormat()},
		Transport:             TransportTCP,
		WriteTimeout:          time.Second,
		ReconnectMinDelay:     10 * time.Millisecond,
		ReconnectMaxDelay:     80 * time.Millisecond,
		ReconnectHealthyAfter: time.Second,
		Logf:                  func(string, ...any) {},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return &scriptedConn{
				write: func(int, []byte) (int, error) {
					cancel()
					return 0, context.Canceled
				},
			}, nil
		},
		sleep: func(_ context.Context, d time.Duration) bool {
			sleeps = append(sleeps, d)
			return true
		},
	})
	if err != nil {
		t.Fatalf("RunSender: %v", err)
	}
	if resolverCalls != 3 {
		t.Fatalf("resolverCalls=%d, want 3", resolverCalls)
	}
	if dials != 1 {
		t.Fatalf("dials=%d, want 1", dials)
	}
	wantSleeps := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if !reflect.DeepEqual(sleeps, wantSleeps) {
		t.Fatalf("sleeps=%v, want %v", sleeps, wantSleeps)
	}
}

func TestRunSenderContextCancelStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var resolverCalls int
	err := RunSender(ctx, SenderOptions{
		Resolve: func(context.Context) (string, error) {
			resolverCalls++
			cancel()
			return "", errors.New("no receiver")
		},
		Capture:               &streamingFakeCapture{format: audio.DefaultFormat()},
		Transport:             TransportTCP,
		WriteTimeout:          time.Second,
		ReconnectMinDelay:     10 * time.Millisecond,
		ReconnectMaxDelay:     80 * time.Millisecond,
		ReconnectHealthyAfter: time.Second,
		Logf:                  func(string, ...any) {},
		sleep: func(context.Context, time.Duration) bool {
			t.Fatal("RunSender slept after context cancellation")
			return false
		},
	})
	if err != nil {
		t.Fatalf("RunSender: %v", err)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolverCalls=%d, want 1", resolverCalls)
	}
}

type streamingFakeCapture struct {
	format audio.Format
	next   byte
}

func (c *streamingFakeCapture) Format() audio.Format {
	return c.format
}

func (c *streamingFakeCapture) Read(dst []byte) int {
	for i := range dst {
		dst[i] = c.next
		c.next++
	}
	return len(dst)
}

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time {
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

type scriptedConn struct {
	write func(int, []byte) (int, error)
	wrote int
}

func (c *scriptedConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *scriptedConn) Write(b []byte) (int, error) {
	c.wrote++
	if c.write != nil {
		return c.write(c.wrote, b)
	}
	return len(b), nil
}

func (c *scriptedConn) Close() error {
	return nil
}

func (c *scriptedConn) LocalAddr() net.Addr {
	return fakeNetAddr("local")
}

func (c *scriptedConn) RemoteAddr() net.Addr {
	return fakeNetAddr("remote")
}

func (c *scriptedConn) SetDeadline(time.Time) error {
	return nil
}

func (c *scriptedConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *scriptedConn) SetWriteDeadline(time.Time) error {
	return nil
}

type fakeNetAddr string

func (a fakeNetAddr) Network() string {
	return "fake"
}

func (a fakeNetAddr) String() string {
	return string(a)
}
