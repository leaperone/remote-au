package discovery

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"remote-au/internal/logging"
)

const (
	readDeadlineInterval  = 250 * time.Millisecond
	announceReplyInterval = 20 * time.Millisecond
	announceReplyBurst    = 16
)

func RunResponder(ctx context.Context, discoveryPort int, audioPort int, name string, advertisedAddr netip.Addr, logger logging.Logger) error {
	conn, _, err := ListenFirst([]int{discoveryPort})
	if err != nil {
		return err
	}
	return RunResponderOnConn(ctx, conn, audioPort, name, advertisedAddr, logger)
}

func RunResponderOnConn(ctx context.Context, conn *net.UDPConn, audioPort int, name string, advertisedAddr netip.Addr, logger logging.Logger) error {
	if conn == nil {
		return errors.New("discovery responder conn is nil")
	}
	// Close the injected conn exactly once: both the ctx watcher and the final
	// cleanup may reach it, and Go's net.UDPConn double-close would otherwise
	// return a (harmless but noisy) already-closed error.
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	defer closeConn()
	if logger == nil {
		logger = logging.Nop()
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

	runCtx, cancel := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-runCtx.Done()
		closeConn()
	}()
	defer func() {
		cancel()
		<-watcherDone
	}()

	logger.Infof("discovery listening on %s, replying with audio port %d as %q", conn.LocalAddr(), audioPort, name)

	limiter := newReplyRateLimiter(time.Now())
	return runResponderLoop(runCtx, conn, announce, &limiter, logger)
}

func newInstanceID() (InstanceID, error) {
	var id InstanceID
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("generate discovery instance id: %w", err)
	}
	return id, nil
}

func runResponderLoop(ctx context.Context, conn *net.UDPConn, announce []byte, limiter *replyRateLimiter, logger logging.Logger) error {
	if logger == nil {
		logger = logging.Nop()
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
			logger.Debugf("ignoring malformed discovery packet from %s: %v", src, err)
			continue
		}
		if msg.Type != TypeQuery {
			continue
		}
		if !limiter.Allow(time.Now()) {
			continue
		}
		if _, err := conn.WriteToUDPAddrPort(announce, src); err != nil {
			logger.Warnf("discovery announce reply to %s failed: %v", src, err)
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
