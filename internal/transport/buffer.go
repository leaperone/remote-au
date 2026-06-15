package transport

import (
	"sync"
	"sync/atomic"
)

type pcmBuffer struct {
	mu       sync.Mutex
	buf      []byte
	quantum  int
	read     int
	used     int
	dropped  atomic.Uint64
	underrun atomic.Uint64
}

func newPCMBuffer(sizeBytes, quantum int) *pcmBuffer {
	if quantum <= 0 {
		quantum = 1
	}
	if sizeBytes < quantum {
		sizeBytes = quantum
	}
	sizeBytes -= sizeBytes % quantum
	if sizeBytes == 0 {
		sizeBytes = quantum
	}
	return &pcmBuffer{
		buf:     make([]byte, sizeBytes),
		quantum: quantum,
	}
}

func (b *pcmBuffer) Write(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writeLocked(p)
}

func (b *pcmBuffer) TryRead(dst []byte) int {
	if len(dst) == 0 {
		return 0
	}
	if !b.mu.TryLock() {
		b.underrun.Add(uint64(b.alignedLen(len(dst))))
		return 0
	}
	defer b.mu.Unlock()

	n := b.readLocked(dst)
	if n < len(dst) {
		b.underrun.Add(uint64(b.alignedLen(len(dst) - n)))
	}
	return n
}

func (b *pcmBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.read = 0
	b.used = 0
}

func (b *pcmBuffer) DroppedBytes() uint64 {
	return b.dropped.Load()
}

func (b *pcmBuffer) UnderrunBytes() uint64 {
	return b.underrun.Load()
}

func (b *pcmBuffer) writeLocked(p []byte) int {
	n := b.alignedLen(len(p))
	if n == 0 {
		b.dropped.Add(uint64(len(p)))
		return 0
	}
	if n != len(p) {
		b.dropped.Add(uint64(len(p) - n))
		p = p[:n]
	}

	if len(p) > len(b.buf) {
		drop := len(p) - len(b.buf)
		b.dropped.Add(uint64(drop))
		p = p[drop:]
	}

	space := len(b.buf) - b.used
	if len(p) > space {
		drop := alignUp(len(p)-space, b.quantum)
		if drop > b.used {
			drop = b.used
		}
		b.read = (b.read + drop) % len(b.buf)
		b.used -= drop
		b.dropped.Add(uint64(drop))
	}

	write := (b.read + b.used) % len(b.buf)
	copied := copy(b.buf[write:], p)
	if copied < len(p) {
		copy(b.buf, p[copied:])
	}
	b.used += len(p)
	return len(p)
}

func (b *pcmBuffer) readLocked(dst []byte) int {
	n := b.alignedLen(len(dst))
	if n > b.used {
		n = b.used
	}
	n = b.alignedLen(n)
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

func (b *pcmBuffer) alignedLen(n int) int {
	return n - n%b.quantum
}

func alignUp(n, quantum int) int {
	if n <= 0 {
		return 0
	}
	rem := n % quantum
	if rem == 0 {
		return n
	}
	return n + quantum - rem
}
