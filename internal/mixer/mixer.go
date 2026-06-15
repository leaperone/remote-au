package mixer

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"remote-au/internal/audio"
	"remote-au/internal/stats"
)

type Options struct {
	Format        audio.Format
	Jitter        JitterBufferOptions
	MasterGain    float32
	ScratchFrames int
}

type Mixer struct {
	format        audio.Format
	bytesPerFrame int
	channels      int
	jitter        JitterBufferOptions

	streamMu sync.Mutex
	streams  atomic.Value // []*Stream

	scratch []byte
	acc     []float32

	masterGain atomic.Uint32
}

type Stream struct {
	id         string
	name       string
	remoteAddr string
	buffer     *JitterBuffer
	gain       atomic.Uint32
}

func New(opts Options) (*Mixer, error) {
	format := opts.Format
	if format == (audio.Format{}) {
		format = audio.DefaultFormat()
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}

	jitter := opts.Jitter
	jitter.Format = format
	if _, err := NewJitterBuffer(jitter); err != nil {
		return nil, err
	}

	scratchFrames := opts.ScratchFrames
	if scratchFrames <= 0 {
		scratchFrames = format.FrameSamples
	}

	masterGain := opts.MasterGain
	if masterGain == 0 {
		masterGain = 1
	}

	m := &Mixer{
		format:        format,
		bytesPerFrame: format.BytesPerFrame(),
		channels:      format.Channels,
		jitter:        jitter,
		scratch:       make([]byte, scratchFrames*format.BytesPerFrame()),
		acc:           make([]float32, scratchFrames*format.Channels),
	}
	m.streams.Store([]*Stream(nil))
	m.masterGain.Store(math.Float32bits(masterGain))
	return m, nil
}

func (m *Mixer) AddStream(id, name, remoteAddr string) (*Stream, error) {
	if id == "" {
		return nil, fmt.Errorf("stream id is required")
	}
	jitter := m.jitter
	jitter.Format = m.format
	buffer, err := NewJitterBuffer(jitter)
	if err != nil {
		return nil, err
	}
	stream := &Stream{
		id:         id,
		name:       name,
		remoteAddr: remoteAddr,
		buffer:     buffer,
	}
	stream.gain.Store(math.Float32bits(1))

	m.streamMu.Lock()
	defer m.streamMu.Unlock()
	current := m.loadStreams()
	next := make([]*Stream, 0, len(current)+1)
	for _, existing := range current {
		if existing.id == id {
			return nil, fmt.Errorf("stream id already exists: %s", id)
		}
		next = append(next, existing)
	}
	next = append(next, stream)
	m.streams.Store(next)
	return stream, nil
}

func (m *Mixer) RemoveStream(id string) bool {
	m.streamMu.Lock()
	defer m.streamMu.Unlock()

	current := m.loadStreams()
	next := make([]*Stream, 0, len(current))
	removed := false
	for _, stream := range current {
		if stream.id == id {
			removed = true
			continue
		}
		next = append(next, stream)
	}
	if removed {
		m.streams.Store(next)
	}
	return removed
}

// Read mixes frameCount frames into out. It reuses callback-owned scratch space
// and must only be called from the serialized playback callback thread.
func (m *Mixer) Read(out []byte, frameCount uint32) {
	frames := int(frameCount)
	if frames <= 0 || len(out) == 0 {
		return
	}
	if maxFrames := len(out) / m.bytesPerFrame; frames > maxFrames {
		frames = maxFrames
	}
	if frames <= 0 {
		clear(out)
		return
	}

	wantBytes := frames * m.bytesPerFrame
	if wantBytes < len(out) {
		clear(out[wantBytes:])
	}

	streams := m.loadStreams()
	if len(streams) == 0 {
		clear(out[:wantBytes])
		return
	}

	defaultGain := m.MasterGain() / float32(len(streams))
	chunkFramesMax := len(m.scratch) / m.bytesPerFrame
	for offsetFrames := 0; offsetFrames < frames; {
		chunkFrames := min(chunkFramesMax, frames-offsetFrames)
		chunkBytes := chunkFrames * m.bytesPerFrame
		acc := m.acc[:chunkFrames*m.channels]
		clear(acc)
		scratch := m.scratch[:chunkBytes]

		for _, stream := range streams {
			stream.buffer.Read(scratch, chunkFrames)
			mixS16LE(acc, scratch, defaultGain*stream.Gain())
		}

		offsetBytes := offsetFrames * m.bytesPerFrame
		writeS16LE(out[offsetBytes:offsetBytes+chunkBytes], acc)
		offsetFrames += chunkFrames
	}
}

func (m *Mixer) Snapshot() stats.AggregateSnapshot {
	streams := m.loadStreams()
	snapshots := make([]stats.StreamSnapshot, 0, len(streams))
	for _, stream := range streams {
		snapshots = append(snapshots, stats.StreamSnapshot{
			ID:         stream.id,
			Name:       stream.name,
			RemoteAddr: stream.remoteAddr,
			AudioStats: stream.buffer.Stats(),
		})
	}
	return stats.NewAggregate(snapshots)
}

func (m *Mixer) ActiveStreams() int {
	return len(m.loadStreams())
}

func (m *Mixer) SetMasterGain(gain float32) error {
	if gain < 0 || math.IsNaN(float64(gain)) || math.IsInf(float64(gain), 0) {
		return fmt.Errorf("invalid master gain: %v", gain)
	}
	m.masterGain.Store(math.Float32bits(gain))
	return nil
}

func (m *Mixer) MasterGain() float32 {
	return math.Float32frombits(m.masterGain.Load())
}

func (m *Mixer) loadStreams() []*Stream {
	streams, _ := m.streams.Load().([]*Stream)
	return streams
}

func (s *Stream) ID() string {
	return s.id
}

func (s *Stream) Name() string {
	return s.name
}

func (s *Stream) RemoteAddr() string {
	return s.remoteAddr
}

func (s *Stream) Write(pcm []byte) int {
	return s.buffer.Write(pcm)
}

func (s *Stream) Stats() stats.AudioStats {
	return s.buffer.Stats()
}

func (s *Stream) SetGain(gain float32) error {
	if gain < 0 || math.IsNaN(float64(gain)) || math.IsInf(float64(gain), 0) {
		return fmt.Errorf("invalid stream gain: %v", gain)
	}
	s.gain.Store(math.Float32bits(gain))
	return nil
}

func (s *Stream) Gain() float32 {
	return math.Float32frombits(s.gain.Load())
}

func mixS16LE(acc []float32, pcm []byte, gain float32) {
	for i := range acc {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		acc[i] += float32(sample) * gain
	}
}

func writeS16LE(dst []byte, acc []float32) {
	for i, sample := range acc {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(clampS16(sample)))
	}
}

func clampS16(v float32) int16 {
	const (
		maxS16 = float32(32767)
		minS16 = float32(-32768)
	)
	if v > maxS16 {
		return 32767
	}
	if v < minS16 {
		return -32768
	}
	if v >= 0 {
		return int16(v + 0.5)
	}
	return int16(v - 0.5)
}
