package stats

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxDisplayNameRunes = 80

// AudioStats mirrors the queue-oriented fields exposed by AudioRelay's native
// audio stats. Counts are in PCM frames for this raw S16LE transport.
type AudioStats struct {
	QueueSize            int
	MaxQueueSize         int
	DiscardedBufferCount uint64
	UnderrunCount        uint64
	PlayoutLatency       time.Duration
}

type StreamSnapshot struct {
	ID         string
	Name       string
	RemoteAddr string
	AudioStats
}

type AggregateSnapshot struct {
	ActiveStreams int
	Streams       []StreamSnapshot
	AudioStats
}

func NewAggregate(streams []StreamSnapshot) AggregateSnapshot {
	s := AggregateSnapshot{
		ActiveStreams: len(streams),
		Streams:       streams,
	}
	for _, stream := range streams {
		s.QueueSize += stream.QueueSize
		s.MaxQueueSize += stream.MaxQueueSize
		s.DiscardedBufferCount += stream.DiscardedBufferCount
		s.UnderrunCount += stream.UnderrunCount
		if stream.PlayoutLatency > s.PlayoutLatency {
			s.PlayoutLatency = stream.PlayoutLatency
		}
	}
	return s
}

func FormatVerbose(s AggregateSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "activeStreams=%d", s.ActiveStreams)
	if s.ActiveStreams > 0 {
		fmt.Fprintf(&b, " queueSize=%d/%d discarded=%d underrun=%d latency=%.1fms",
			s.QueueSize,
			s.MaxQueueSize,
			s.DiscardedBufferCount,
			s.UnderrunCount,
			float64(s.PlayoutLatency)/float64(time.Millisecond),
		)
	}
	b.WriteByte('\n')
	for _, stream := range s.Streams {
		name := SafeDisplayName(stream.Name)
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "  %s %s queueSize=%d/%d discarded=%d underrun=%d latency=%.1fms\n",
			name,
			stream.RemoteAddr,
			stream.QueueSize,
			stream.MaxQueueSize,
			stream.DiscardedBufferCount,
			stream.UnderrunCount,
			float64(stream.PlayoutLatency)/float64(time.Millisecond),
		)
	}
	return b.String()
}

func SafeDisplayName(name string) string {
	var b strings.Builder
	count := 0
	for _, r := range name {
		if count >= maxDisplayNameRunes {
			b.WriteString("...")
			break
		}
		count++
		if r == 0x7f || r == 0x1b || !unicode.IsPrint(r) {
			q := strconv.QuoteToASCII(string(r))
			b.WriteString(q[1 : len(q)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
