package discovery

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"time"
)

const (
	readDeadlineInterval  = 250 * time.Millisecond
	announceReplyInterval = 20 * time.Millisecond
	announceReplyBurst    = 16
)

func RunResponder(ctx context.Context, discoveryPort int, audioPort int, name string, advertisedAddr netip.Addr, logf func(format string, args ...any)) error {
	if discoveryPort <= 0 || discoveryPort > 65535 {
		return fmt.Errorf("discovery port out of range: %d", discoveryPort)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	instanceID, err := newInstanceID()
	if err != nil {
		return err
	}
	announce, err := EncodeAnnounce(Announce{
		TCPPort:        audioPort,
		InstanceID:     instanceID,
		AdvertisedAddr: advertisedAddr,
		Name:           name,
	})
	if err != nil {
		return err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: discoveryPort})
	if err != nil {
		return fmt.Errorf("listen discovery udp :%d: %w", discoveryPort, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-runCtx.Done()
		_ = conn.Close()
	}()
	defer func() {
		cancel()
		_ = conn.Close()
		<-watcherDone
	}()

	logf("discovery listening on %s, replying with audio port %d as %q", conn.LocalAddr(), audioPort, name)

	limiter := newReplyRateLimiter(time.Now())
	return runResponderLoop(runCtx, conn, announce, &limiter, logf)
}

func newInstanceID() (InstanceID, error) {
	var id InstanceID
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("generate discovery instance id: %w", err)
	}
	return id, nil
}

func runResponderLoop(ctx context.Context, conn *net.UDPConn, announce []byte, limiter *replyRateLimiter, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if limiter == nil {
		newLimiter := newReplyRateLimiter(time.Now())
		limiter = &newLimiter
	}

	buf := make([]byte, MaxPacketBytes+1)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := conn.SetReadDeadline(time.Now().Add(readDeadlineInterval)); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("set discovery read deadline: %w", err)
		}

		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return fmt.Errorf("read discovery packet: %w", err)
		}

		msg, err := Decode(buf[:n])
		if err != nil {
			logf("ignoring malformed discovery packet from %s: %v", src, err)
			continue
		}
		if msg.Type != TypeQuery {
			continue
		}
		if !limiter.Allow(time.Now()) {
			continue
		}
		if _, err := conn.WriteToUDPAddrPort(announce, src); err != nil {
			logf("discovery announce reply to %s failed: %v", src, err)
		}
	}
}

type replyRateLimiter struct {
	tokens int
	last   time.Time
}

func newReplyRateLimiter(now time.Time) replyRateLimiter {
	return replyRateLimiter{tokens: announceReplyBurst, last: now}
}

func (limiter *replyRateLimiter) Allow(now time.Time) bool {
	if limiter.last.IsZero() {
		limiter.tokens = announceReplyBurst
		limiter.last = now
	}
	if elapsed := now.Sub(limiter.last); elapsed >= announceReplyInterval {
		limiter.tokens += int(elapsed / announceReplyInterval)
		if limiter.tokens > announceReplyBurst {
			limiter.tokens = announceReplyBurst
		}
		limiter.last = now
	}
	if limiter.tokens <= 0 {
		return false
	}
	limiter.tokens--
	return true
}
