package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/transport"
)

type globalOptions struct {
	rate     int
	channels int
	frameMS  int
	verbose  bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	opts := globalOptions{
		rate:     audio.DefaultSampleRate,
		channels: audio.DefaultChannels,
		frameMS:  audio.DefaultFrameMillis,
	}

	globals := flag.NewFlagSet("remote-au", flag.ContinueOnError)
	globals.SetOutput(stderr)
	registerGlobalFlags(globals, &opts)
	globals.Usage = func() {
		printUsage(stderr, globals)
	}
	if err := globals.Parse(args); err != nil {
		return err
	}

	remaining := globals.Args()
	if len(remaining) == 0 {
		printUsage(stderr, globals)
		return errors.New("missing subcommand")
	}

	format, err := audio.NewFormat(opts.rate, opts.channels, opts.frameMS)
	if err != nil {
		return err
	}

	switch remaining[0] {
	case "devices":
		return runDevices(remaining[1:], stdout, stderr, format, opts.verbose)
	case "selftest":
		return runSelftest(remaining[1:], stdout, stderr, format, opts.verbose)
	case "recv":
		return runRecv(remaining[1:], stdout, stderr, format, opts.verbose)
	case "send":
		return runSend(remaining[1:], stdout, stderr, format, opts.verbose)
	default:
		printUsage(stderr, globals)
		return fmt.Errorf("unknown subcommand %q", remaining[0])
	}
}

func registerGlobalFlags(fs *flag.FlagSet, opts *globalOptions) {
	fs.IntVar(&opts.rate, "rate", opts.rate, "sample rate in Hz")
	fs.IntVar(&opts.channels, "channels", opts.channels, "channel count")
	fs.IntVar(&opts.frameMS, "frame-ms", opts.frameMS, "PCM packet duration in milliseconds")
	fs.BoolVar(&opts.verbose, "verbose", opts.verbose, "enable verbose audio backend logging")
}

func printUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: remote-au [global flags] <devices|selftest|recv|send>\n\n")
	fmt.Fprintln(w, "Global flags:")
	fs.PrintDefaults()
}

func runDevices(args []string, stdout, stderr io.Writer, format audio.Format, verbose bool) error {
	if len(args) != 0 {
		return fmt.Errorf("devices takes no arguments: %v", args)
	}
	if verbose {
		fmt.Fprintf(stderr, "format: %s, %d bytes per frame\n", format, format.BytesPerFrame())
	}
	return audio.PrintDevices(stdout, verbose)
}

func runSelftest(args []string, stdout, _ io.Writer, format audio.Format, verbose bool) error {
	if len(args) != 0 {
		return fmt.Errorf("selftest takes no arguments: %v", args)
	}

	fmt.Fprintf(stdout, "Selftest using %s. Speak into the microphone; you should hear it through playback.\n", format)
	fmt.Fprintln(stdout, "Press Ctrl-C to stop.")

	capture, err := audio.OpenCapture(audio.CaptureOptions{
		Format:  format,
		Source:  audio.SourceMicrophone,
		Verbose: verbose,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = capture.Close()
	}()

	playback, err := audio.OpenPlayback(format, func(out []byte, _ uint32) {
		capture.TryRead(out)
	}, verbose)
	if err != nil {
		return err
	}
	defer func() {
		_ = playback.Close()
	}()

	if err := capture.Start(); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := playback.Start(); err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	<-signals
	fmt.Fprint(stdout, "\nselftest stopped\n")
	return nil
}

func runRecv(args []string, stdout, stderr io.Writer, format audio.Format, verbose bool) error {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := ":47000"
	deviceIndex := -1
	fs.StringVar(&addr, "addr", addr, "TCP listen address")
	fs.IntVar(&deviceIndex, "device", deviceIndex, "playback device index from devices")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("recv takes no positional arguments: %v", fs.Args())
	}

	receiver, err := transport.NewReceiver(transport.ReceiverOptions{
		Format: format,
		Logf:   writerLogf(stdout),
	})
	if err != nil {
		return err
	}

	playbackOpts := audio.PlaybackOptions{
		Format:  format,
		Pull:    receiver.Pull,
		Verbose: verbose,
	}
	if deviceIndex >= 0 {
		deviceID, name, err := audio.PlaybackDeviceByIndex(deviceIndex, verbose)
		if err != nil {
			return err
		}
		playbackOpts.DeviceID = deviceID
		fmt.Fprintf(stdout, "playback device: [%d] %s\n", deviceIndex, name)
	}

	playback, err := audio.OpenPlaybackWithOptions(playbackOpts)
	if err != nil {
		return err
	}
	defer func() {
		_ = playback.Close()
	}()

	if err := playback.Start(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "recv using %s\n", format)
	fmt.Fprintln(stdout, "Press Ctrl-C to stop.")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := receiver.Run(ctx, addr); err != nil {
		return err
	}
	fmt.Fprint(stdout, "\nrecv stopped\n")
	return nil
}

func runSend(args []string, stdout, stderr io.Writer, format audio.Format, verbose bool) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := ""
	sourceName := "mic"
	deviceIndex := -1
	fs.StringVar(&to, "to", to, "receiver TCP address, for example 127.0.0.1:47000")
	fs.StringVar(&sourceName, "source", sourceName, "capture source: mic or loopback")
	fs.IntVar(&deviceIndex, "device", deviceIndex, "capture device index from devices; loopback uses playback device index on Windows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("send takes no positional arguments: %v", fs.Args())
	}
	if to == "" {
		return fmt.Errorf("send requires --to <ip:port>")
	}

	source, err := parseCaptureSource(sourceName)
	if err != nil {
		return err
	}

	captureOpts := audio.CaptureOptions{
		Format:  format,
		Source:  source,
		Verbose: verbose,
	}
	if deviceIndex >= 0 {
		deviceID, name, err := audio.CaptureDeviceByIndex(deviceIndex, source, verbose)
		if err != nil {
			return err
		}
		captureOpts.DeviceID = deviceID
		fmt.Fprintf(stdout, "capture device: [%d] %s\n", deviceIndex, name)
	}

	capture, err := audio.OpenCapture(captureOpts)
	if err != nil {
		return err
	}
	defer func() {
		_ = capture.Close()
	}()

	if err := capture.Start(); err != nil {
		return err
	}

	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "remote-au"
	}

	fmt.Fprintf(stdout, "send using %s, source=%s\n", format, sourceName)
	fmt.Fprintln(stdout, "Press Ctrl-C to stop.")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := transport.RunSender(ctx, transport.SenderOptions{
		Address: to,
		Capture: capture,
		Name:    name,
		Logf:    writerLogf(stdout),
	}); err != nil {
		return err
	}
	fmt.Fprint(stdout, "\nsend stopped\n")
	return nil
}

func parseCaptureSource(name string) (audio.CaptureSource, error) {
	switch name {
	case "mic", "microphone":
		return audio.SourceMicrophone, nil
	case "loopback":
		return audio.SourceLoopback, nil
	default:
		return audio.SourceMicrophone, fmt.Errorf("unknown capture source %q (want mic or loopback)", name)
	}
}

func writerLogf(w io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...)
	}
}
