package mixer

import (
	"encoding/binary"
	"testing"
	"time"

	"remote-au/internal/audio"
)

func TestJitterBufferPrimesBeforeEmittingAudio(t *testing.T) {
	buffer := newTestJitterBuffer(t, 1, 2, 4, 6)

	buffer.Write(pcm16(1, 2, 3))
	out := filledBytes(4, 0x7f)
	if got := buffer.Read(out, 2); got != 0 {
		t.Fatalf("Read copied %d frames before priming, want 0", got)
	}
	if got := pcm16Values(out); !equalInt16(got, []int16{0, 0}) {
		t.Fatalf("unprimed read = %v, want silence", got)
	}
	stats := buffer.Stats()
	if stats.QueueSize != 3 {
		t.Fatalf("queueSize = %d, want 3", stats.QueueSize)
	}
	if stats.UnderrunCount != 2 {
		t.Fatalf("underrun = %d, want 2", stats.UnderrunCount)
	}

	buffer.Write(pcm16(4))
	out = filledBytes(4, 0x7f)
	if got := buffer.Read(out, 2); got != 2 {
		t.Fatalf("Read copied %d frames after priming, want 2", got)
	}
	if got := pcm16Values(out); !equalInt16(got, []int16{1, 2}) {
		t.Fatalf("primed read = %v, want [1 2]", got)
	}
}

func TestJitterBufferPartialReadReprimesAndFillsSilence(t *testing.T) {
	buffer := newTestJitterBuffer(t, 1, 2, 4, 6)
	buffer.Write(pcm16(1, 2, 3, 4))

	out := filledBytes(12, 0x7f)
	if got := buffer.Read(out, 6); got != 0 {
		t.Fatalf("Read copied %d frames from partial queue, want 0", got)
	}
	if got := pcm16Values(out); !equalInt16(got, []int16{0, 0, 0, 0, 0, 0}) {
		t.Fatalf("partial read = %v, want silence", got)
	}
	stats := buffer.Stats()
	if stats.QueueSize != 4 {
		t.Fatalf("queueSize = %d, want retained target queue", stats.QueueSize)
	}
	if stats.UnderrunCount != 6 {
		t.Fatalf("underrun = %d, want requested frame count", stats.UnderrunCount)
	}
}

func TestJitterBufferReprimesAfterLowWatermark(t *testing.T) {
	buffer := newTestJitterBuffer(t, 1, 2, 4, 8)
	buffer.Write(pcm16(1, 2, 3, 4))

	out := filledBytes(6, 0x7f)
	if got := buffer.Read(out, 3); got != 3 {
		t.Fatalf("initial Read copied %d frames, want 3", got)
	}
	if got := pcm16Values(out); !equalInt16(got, []int16{1, 2, 3}) {
		t.Fatalf("initial read = %v, want [1 2 3]", got)
	}

	out = filledBytes(2, 0x7f)
	if got := buffer.Read(out, 1); got != 0 {
		t.Fatalf("below-low Read copied %d frames, want 0", got)
	}
	if got := pcm16Values(out); !equalInt16(got, []int16{0}) {
		t.Fatalf("below-low read = %v, want silence", got)
	}
	if stats := buffer.Stats(); stats.UnderrunCount != 1 || stats.QueueSize != 1 {
		t.Fatalf("stats after low watermark = underrun %d queue %d, want 1 and 1", stats.UnderrunCount, stats.QueueSize)
	}

	buffer.Write(pcm16(5, 6, 7))
	out = filledBytes(8, 0x7f)
	if got := buffer.Read(out, 4); got != 4 {
		t.Fatalf("re-primed Read copied %d frames, want 4", got)
	}
	if got := pcm16Values(out); !equalInt16(got, []int16{4, 5, 6, 7}) {
		t.Fatalf("re-primed read = %v, want queued audio", got)
	}
}

func TestJitterBufferDropsOldestFramesAboveHighWatermark(t *testing.T) {
	buffer := newTestJitterBuffer(t, 1, 2, 2, 4)
	buffer.Write(pcm16(1, 2, 3, 4))
	buffer.Write(pcm16(5, 6))

	stats := buffer.Stats()
	if stats.QueueSize != 4 {
		t.Fatalf("queueSize = %d, want 4", stats.QueueSize)
	}
	if stats.DiscardedBufferCount != 2 {
		t.Fatalf("discarded = %d, want 2", stats.DiscardedBufferCount)
	}

	out := make([]byte, 8)
	if got := buffer.Read(out, 4); got != 4 {
		t.Fatalf("Read copied %d frames, want 4", got)
	}
	if got := pcm16Values(out); !equalInt16(got, []int16{3, 4, 5, 6}) {
		t.Fatalf("drop read = %v, want newest high-watermark frames", got)
	}
}

func TestJitterBufferReadDoesNotBlockWhenLocked(t *testing.T) {
	buffer := newTestJitterBuffer(t, 1, 1, 2, 4)
	buffer.Write(pcm16(1, 2))

	buffer.mu.Lock()
	out := filledBytes(4, 0x7f)
	if got := buffer.Read(out, 2); got != 0 {
		buffer.mu.Unlock()
		t.Fatalf("locked Read copied %d frames, want 0", got)
	}
	buffer.mu.Unlock()

	if got := pcm16Values(out); !equalInt16(got, []int16{0, 0}) {
		t.Fatalf("locked read = %v, want silence", got)
	}
	if stats := buffer.Stats(); stats.UnderrunCount != 2 {
		t.Fatalf("underrun = %d, want 2", stats.UnderrunCount)
	}
}

func TestJitterBufferStatsDoesNotNeedDataLock(t *testing.T) {
	buffer := newTestJitterBuffer(t, 1, 1, 2, 4)
	buffer.Write(pcm16(1, 2))

	done := make(chan struct{})
	buffer.mu.Lock()
	go func() {
		_ = buffer.Stats()
		close(done)
	}()

	select {
	case <-done:
		buffer.mu.Unlock()
	case <-time.After(200 * time.Millisecond):
		buffer.mu.Unlock()
		t.Fatal("Stats blocked on jitter buffer data mutex")
	}
}

func newTestJitterBuffer(t *testing.T, channels, low, target, high int) *JitterBuffer {
	t.Helper()
	buffer, err := NewJitterBuffer(JitterBufferOptions{
		Format: audio.Format{
			Rate:         1000,
			Channels:     channels,
			FrameSamples: 1,
		},
		LowWatermarkFrames:  low,
		TargetFrames:        target,
		HighWatermarkFrames: high,
	})
	if err != nil {
		t.Fatalf("NewJitterBuffer: %v", err)
	}
	return buffer
}

func pcm16(values ...int16) []byte {
	pcm := make([]byte, len(values)*2)
	for i, value := range values {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(value))
	}
	return pcm
}

func pcm16Values(pcm []byte) []int16 {
	values := make([]int16, len(pcm)/2)
	for i := range values {
		values[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return values
}

func filledBytes(n int, value byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = value
	}
	return b
}

func equalInt16(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
