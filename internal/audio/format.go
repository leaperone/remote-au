package audio

import "fmt"

const (
	DefaultSampleRate   = 48000
	DefaultChannels     = 2
	DefaultFrameMillis  = 10
	DefaultFrameSamples = 480

	SampleFormatS16LE = "S16LE"
	BytesPerSampleS16 = 2
)

// Format describes the interleaved PCM format used by Batch 1.
type Format struct {
	Rate         int
	Channels     int
	FrameSamples int
}

func DefaultFormat() Format {
	return Format{
		Rate:         DefaultSampleRate,
		Channels:     DefaultChannels,
		FrameSamples: DefaultFrameSamples,
	}
}

func NewFormat(rate, channels, frameMillis int) (Format, error) {
	if rate <= 0 {
		return Format{}, fmt.Errorf("sample rate must be positive: %d", rate)
	}
	if channels <= 0 {
		return Format{}, fmt.Errorf("channel count must be positive: %d", channels)
	}
	if frameMillis <= 0 {
		return Format{}, fmt.Errorf("frame duration must be positive: %dms", frameMillis)
	}
	samplesNumerator := rate * frameMillis
	if samplesNumerator%1000 != 0 {
		return Format{}, fmt.Errorf("rate %d and frame duration %dms do not produce whole frame samples", rate, frameMillis)
	}

	return Format{
		Rate:         rate,
		Channels:     channels,
		FrameSamples: samplesNumerator / 1000,
	}, nil
}

func (f Format) Validate() error {
	if f.Rate <= 0 {
		return fmt.Errorf("sample rate must be positive: %d", f.Rate)
	}
	if f.Channels <= 0 {
		return fmt.Errorf("channel count must be positive: %d", f.Channels)
	}
	if f.FrameSamples <= 0 {
		return fmt.Errorf("frame sample count must be positive: %d", f.FrameSamples)
	}
	return nil
}

func (f Format) BytesPerFrame() int {
	return f.Channels * BytesPerSampleS16
}

func (f Format) PacketBytes() int {
	return f.FrameSamples * f.BytesPerFrame()
}

func (f Format) String() string {
	return fmt.Sprintf("%d Hz %s %dch %d-frame packets", f.Rate, SampleFormatS16LE, f.Channels, f.FrameSamples)
}
