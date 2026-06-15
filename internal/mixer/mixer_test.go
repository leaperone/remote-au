package mixer

import (
	"testing"

	"remote-au/internal/audio"
)

func TestMixerZeroStreamsOutputsSilence(t *testing.T) {
	mix := newTestMixer(t)
	out := filledBytes(6, 0x7f)

	mix.Read(out, 3)

	if got := pcm16Values(out); !equalInt16(got, []int16{0, 0, 0}) {
		t.Fatalf("zero-stream output = %v, want silence", got)
	}
}

func TestMixerAppliesDefaultNStreamGainForArbitraryFrameCount(t *testing.T) {
	mix := newTestMixer(t)
	s1 := addTestStream(t, mix, "s1")
	s2 := addTestStream(t, mix, "s2")
	s1.Write(pcm16(10000, 10000, 10000))
	s2.Write(pcm16(20000, 20000, 20000))

	out := make([]byte, 6)
	mix.Read(out, 3)

	if got := pcm16Values(out); !equalInt16(got, []int16{15000, 15000, 15000}) {
		t.Fatalf("mixed output = %v, want default 1/N gain", got)
	}
}

func TestMixerReadChunksBeyondScratchWithoutAllocating(t *testing.T) {
	mix := newTestMixerWithScratch(t, 2)
	s1 := addTestStream(t, mix, "s1")
	s2 := addTestStream(t, mix, "s2")
	s1.Write(pcm16(1000, 2000, 3000, 4000, 5000))
	s2.Write(pcm16(5000, 4000, 3000, 2000, 1000))

	out := make([]byte, 10)
	mix.Read(out, 5)

	if got := pcm16Values(out); !equalInt16(got, []int16{3000, 3000, 3000, 3000, 3000}) {
		t.Fatalf("chunked output = %v, want mixed chunks", got)
	}

	allocMix := newTestMixerWithScratch(t, 2)
	stream := addTestStream(t, allocMix, "s1")
	packet := pcm16(1, 2, 3, 4, 5)
	allocOut := make([]byte, 10)
	allocs := testing.AllocsPerRun(100, func() {
		stream.Write(packet)
		allocMix.Read(allocOut, 5)
	})
	if allocs != 0 {
		t.Fatalf("Mixer.Read allocations = %v, want 0", allocs)
	}
}

func TestMixerClampsAfterMasterGain(t *testing.T) {
	mix := newTestMixer(t)
	if err := mix.SetMasterGain(2); err != nil {
		t.Fatalf("SetMasterGain: %v", err)
	}
	s1 := addTestStream(t, mix, "s1")
	s2 := addTestStream(t, mix, "s2")
	s1.Write(pcm16(32767, 32767))
	s2.Write(pcm16(32767, 32767))

	out := make([]byte, 4)
	mix.Read(out, 2)

	if got := pcm16Values(out); !equalInt16(got, []int16{32767, 32767}) {
		t.Fatalf("clamped output = %v, want max S16", got)
	}
}

func newTestMixer(t *testing.T) *Mixer {
	t.Helper()
	return newTestMixerWithScratch(t, 1)
}

func newTestMixerWithScratch(t *testing.T, scratchFrames int) *Mixer {
	t.Helper()
	mix, err := New(Options{
		Format: audio.Format{
			Rate:         1000,
			Channels:     1,
			FrameSamples: 1,
		},
		Jitter: JitterBufferOptions{
			TargetFrames:        1,
			HighWatermarkFrames: 8,
		},
		ScratchFrames: scratchFrames,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mix
}

func addTestStream(t *testing.T, mix *Mixer, id string) *Stream {
	t.Helper()
	stream, err := mix.AddStream(id, id, "test")
	if err != nil {
		t.Fatalf("AddStream: %v", err)
	}
	return stream
}
