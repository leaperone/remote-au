package audio

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/gen2brain/malgo"
)

type CaptureSource int

const (
	SourceMicrophone CaptureSource = iota
	SourceLoopback
)

type CaptureOptions struct {
	Format     Format
	Source     CaptureSource
	DeviceID   *malgo.DeviceID
	RingFrames int
	Verbose    bool
}

type Capture struct {
	ctx       *malgo.AllocatedContext
	dev       *malgo.Device
	format    Format
	ring      *pcmRing
	closeOnce sync.Once
	closeErr  error
}

func OpenCapture(opts CaptureOptions) (*Capture, error) {
	format := opts.Format
	if format == (Format{}) {
		format = DefaultFormat()
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if opts.Source == SourceLoopback && runtime.GOOS != "windows" {
		return nil, fmt.Errorf("loopback capture is only supported through WASAPI on Windows")
	}

	deviceType := malgo.Capture
	if opts.Source == SourceLoopback {
		deviceType = malgo.Loopback
	}

	ringFrames := opts.RingFrames
	if ringFrames <= 0 {
		ringFrames = format.Rate / 4
	}
	ring := newPCMRing(ringFrames*format.BytesPerFrame(), format.BytesPerFrame())

	ctx, err := initContext(opts.Verbose)
	if err != nil {
		return nil, fmt.Errorf("init capture context: %w", err)
	}

	cfg := malgo.DefaultDeviceConfig(deviceType)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = uint32(format.Channels)
	if opts.DeviceID != nil {
		cfg.Capture.DeviceID = opts.DeviceID.Pointer()
	}
	cfg.SampleRate = uint32(format.Rate)
	cfg.PeriodSizeInFrames = uint32(format.FrameSamples)
	cfg.PerformanceProfile = malgo.LowLatency

	cb := func(out, in []byte, frameCount uint32) {
		_, _ = out, frameCount
		// Copy immediately into a bounded ring; callers consume outside the audio callback.
		ring.TryWrite(in)
	}

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: cb})
	if err != nil {
		_ = closeContext(ctx)
		return nil, fmt.Errorf("init capture device: %w", err)
	}

	return &Capture{
		ctx:    ctx,
		dev:    dev,
		format: format,
		ring:   ring,
	}, nil
}

func (c *Capture) Start() error {
	if err := c.dev.Start(); err != nil {
		return fmt.Errorf("start capture device: %w", err)
	}
	return nil
}

func (c *Capture) Close() error {
	c.closeOnce.Do(func() {
		if c.dev != nil {
			c.dev.Uninit()
		}
		c.closeErr = closeContext(c.ctx)
	})
	if c.closeErr != nil {
		return fmt.Errorf("close capture context: %w", c.closeErr)
	}
	return nil
}

func (c *Capture) Read(dst []byte) int {
	return c.ring.Read(dst)
}

func (c *Capture) TryRead(dst []byte) int {
	return c.ring.TryRead(dst)
}

func (c *Capture) ReadFrames(dst []byte) int {
	return c.Read(dst) / c.format.BytesPerFrame()
}

func (c *Capture) TryReadFrames(dst []byte) int {
	return c.TryRead(dst) / c.format.BytesPerFrame()
}

func (c *Capture) AvailableFrames() int {
	return c.ring.Available() / c.format.BytesPerFrame()
}

func (c *Capture) DroppedFrames() uint64 {
	return c.ring.Dropped() / uint64(c.format.BytesPerFrame())
}

func (c *Capture) Format() Format {
	return c.format
}
