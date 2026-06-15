package mixer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/stats"
)

const (
	defaultLowWatermarkMillis  = 50
	defaultTargetMillis        = 60
	defaultHighWatermarkMillis = 80
)

type JitterBufferOptions struct {
	Format              audio.Format
	LowWatermarkFrames  int
	TargetFrames        int
	HighWatermarkFrames int
}

type JitterBuffer struct {
	mu sync.Mutex

	format        audio.Format
	bytesPerFrame int

	buf  []byte
	read int
	used int

	lowFrames    int
	targetFrames int
	highFrames   int
	primed       bool

	discardedFrames atomic.Uint64
	underrunFrames  atomic.Uint64
	queueFrames     atomic.Int64
}

func NewJitterBuffer(opts JitterBufferOptions) (*JitterBuffer, error) {
	format := opts.Format
	if format == (audio.Format{}) {
		format = audio.DefaultFormat()
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}

	lowFrames := opts.LowWatermarkFrames
	if lowFrames <= 0 {
		lowFrames = millisToFrames(format.Rate, defaultLowWatermarkMillis)
	}
	targetFrames := opts.TargetFrames
	if targetFrames <= 0 {
		targetFrames = millisToFrames(format.Rate, defaultTargetMillis)
	}
	highFrames := opts.HighWatermarkFrames
	if highFrames <= 0 {
		highFrames = millisToFrames(format.Rate, defaultHighWatermarkMillis)
	}
	if lowFrames <= 0 || targetFrames <= 0 || highFrames <= 0 {
		return nil, fmt.Errorf("jitter buffer watermarks must be positive")
	}
	if lowFrames > targetFrames {
		return nil, fmt.Errorf("low watermark %d exceeds target %d", lowFrames, targetFrames)
	}
	if targetFrames > highFrames {
		return nil, fmt.Errorf("target watermark %d exceeds high watermark %d", targetFrames, highFrames)
	}

	bytesPerFrame := format.BytesPerFrame()
	return &JitterBuffer{
		format:        format,
		bytesPerFrame: bytesPerFrame,
		buf:           make([]byte, highFrames*bytesPerFrame),
		lowFrames:     lowFrames,
		targetFrames:  targetFrames,
		highFrames:    highFrames,
	}, nil
}

func (b *JitterBuffer) Write(pcm []byte) int {
	frames := len(pcm) / b.bytesPerFrame
	if frames <= 0 {
		return 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if frames > b.highFrames {
		dropFrames := frames - b.highFrames
		pcm = pcm[dropFrames*b.bytesPerFrame:]
		frames = b.highFrames
		b.discardedFrames.Add(uint64(dropFrames))
	}
	pcm = pcm[:frames*b.bytesPerFrame]

	excessFrames := b.used/b.bytesPerFrame + frames - b.highFrames
	if excessFrames > 0 {
		b.dropOldestLocked(excessFrames)
	}
	b.writeLocked(pcm)
	if !b.primed && b.used/b.bytesPerFrame >= b.targetFrames {
		b.primed = true
	}
	b.storeQueueFramesLocked()
	return frames
}

func (b *JitterBuffer) Read(dst []byte, frameCount int) int {
	if frameCount <= 0 || len(dst) == 0 {
		return 0
	}
	frames := frameCount
	if maxFrames := len(dst) / b.bytesPerFrame; frames > maxFrames {
		frames = maxFrames
	}
	if frames <= 0 {
		return 0
	}

	wantBytes := frames * b.bytesPerFrame
	if !b.mu.TryLock() {
		b.underrunFrames.Add(uint64(frames))
		clear(dst[:wantBytes])
		return 0
	}
	defer b.mu.Unlock()

	queuedFrames := b.used / b.bytesPerFrame
	if !b.primed {
		if queuedFrames < b.targetFrames {
			b.underrunFrames.Add(uint64(frames))
			clear(dst[:wantBytes])
			return 0
		}
		b.primed = true
	}

	if queuedFrames < b.lowFrames || queuedFrames < frames {
		b.primed = false
		b.underrunFrames.Add(uint64(frames))
		clear(dst[:wantBytes])
		return 0
	}

	copiedBytes := b.readLocked(dst[:wantBytes])
	b.storeQueueFramesLocked()
	return copiedBytes / b.bytesPerFrame
}

func (b *JitterBuffer) Stats() stats.AudioStats {
	queueFrames := int(b.queueFrames.Load())
	return stats.AudioStats{
		QueueSize:            queueFrames,
		MaxQueueSize:         b.highFrames,
		DiscardedBufferCount: b.discardedFrames.Load(),
		UnderrunCount:        b.underrunFrames.Load(),
		PlayoutLatency:       framesDuration(queueFrames, b.format.Rate),
	}
}

func (b *JitterBuffer) QueueSizeFrames() int {
	return int(b.queueFrames.Load())
}

func (b *JitterBuffer) LowWatermarkFrames() int {
	return b.lowFrames
}

func (b *JitterBuffer) LowWatermarkBytes() int {
	return b.lowFrames * b.bytesPerFrame
}

func (b *JitterBuffer) TargetFrames() int {
	return b.targetFrames
}

func (b *JitterBuffer) TargetBytes() int {
	return b.targetFrames * b.bytesPerFrame
}

func (b *JitterBuffer) HighWatermarkFrames() int {
	return b.highFrames
}

func (b *JitterBuffer) HighWatermarkBytes() int {
	return b.highFrames * b.bytesPerFrame
}

func (b *JitterBuffer) dropOldestLocked(frames int) {
	if frames <= 0 || b.used == 0 {
		return
	}
	usedFrames := b.used / b.bytesPerFrame
	if frames > usedFrames {
		frames = usedFrames
	}
	dropBytes := frames * b.bytesPerFrame
	b.read = (b.read + dropBytes) % len(b.buf)
	b.used -= dropBytes
	b.discardedFrames.Add(uint64(frames))
}

func (b *JitterBuffer) writeLocked(pcm []byte) {
	write := (b.read + b.used) % len(b.buf)
	copied := copy(b.buf[write:], pcm)
	if copied < len(pcm) {
		copy(b.buf, pcm[copied:])
	}
	b.used += len(pcm)
}

func (b *JitterBuffer) readLocked(dst []byte) int {
	n := min(len(dst), b.used)
	n -= n % b.bytesPerFrame
	if n == 0 {
		return 0
	}

	first := min(n, len(b.buf)-b.read)
	copied := copy(dst, b.buf[b.read:b.read+first])
	if copied < n {
		copy(dst[copied:], b.buf[:n-copied])
	}
	b.read = (b.read + n) % len(b.buf)
	b.used -= n
	return n
}

func (b *JitterBuffer) storeQueueFramesLocked() {
	b.queueFrames.Store(int64(b.used / b.bytesPerFrame))
}

func millisToFrames(rate, millis int) int {
	frames := rate * millis / 1000
	if frames < 1 {
		return 1
	}
	return frames
}

func framesDuration(frames, rate int) time.Duration {
	if rate <= 0 || frames <= 0 {
		return 0
	}
	return time.Duration(frames) * time.Second / time.Duration(rate)
}
