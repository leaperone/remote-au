package audio

import (
	"fmt"
	"sync"

	"github.com/gen2brain/malgo"

	"remote-au/internal/logging"
)

type PullFunc func(out []byte, frameCount uint32)

type PlaybackOptions struct {
	Format   Format
	DeviceID *malgo.DeviceID
	Pull     PullFunc
	Verbose  bool
	Logger   logging.Logger
}

type Playback struct {
	ctx       *malgo.AllocatedContext
	dev       *malgo.Device
	format    Format
	closeOnce sync.Once
	closeErr  error
}

func OpenPlayback(format Format, pull PullFunc, verbose bool) (*Playback, error) {
	return OpenPlaybackWithOptions(PlaybackOptions{
		Format:  format,
		Pull:    pull,
		Verbose: verbose,
	})
}

func OpenPlaybackWithOptions(opts PlaybackOptions) (*Playback, error) {
	format := opts.Format
	if format == (Format{}) {
		format = DefaultFormat()
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}
	pull := opts.Pull
	if pull == nil {
		pull = func(out []byte, _ uint32) {
			clear(out)
		}
	}

	ctx, err := initContext(opts.Verbose, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("init playback context: %w", err)
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = uint32(format.Channels)
	if opts.DeviceID != nil {
		cfg.Playback.DeviceID = opts.DeviceID.Pointer()
		defer freeDeviceIDPointer(cfg.Playback.DeviceID)
	}
	cfg.SampleRate = uint32(format.Rate)
	cfg.PeriodSizeInFrames = uint32(format.FrameSamples)
	cfg.PerformanceProfile = malgo.LowLatency

	bytesPerFrame := format.BytesPerFrame()
	cb := func(out, in []byte, frameCount uint32) {
		_ = in
		frames := frameCount
		wantBytes := int(frameCount) * bytesPerFrame
		if wantBytes > len(out) {
			frames = uint32(len(out) / bytesPerFrame)
			wantBytes = int(frames) * bytesPerFrame
		}
		out = out[:wantBytes]
		clear(out)
		// frameCount is the audio clock; PeriodSizeInFrames is only a latency hint.
		pull(out, frames)
	}

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: cb})
	if err != nil {
		_ = closeContext(ctx)
		return nil, fmt.Errorf("init playback device: %w", err)
	}

	return &Playback{
		ctx:    ctx,
		dev:    dev,
		format: format,
	}, nil
}

func (p *Playback) Start() error {
	if err := p.dev.Start(); err != nil {
		return fmt.Errorf("start playback device: %w", err)
	}
	return nil
}

func (p *Playback) Close() error {
	p.closeOnce.Do(func() {
		if p.dev != nil {
			p.dev.Uninit()
		}
		p.closeErr = closeContext(p.ctx)
	})
	if p.closeErr != nil {
		return fmt.Errorf("close playback context: %w", p.closeErr)
	}
	return nil
}

func (p *Playback) Format() Format {
	return p.format
}
