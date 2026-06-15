package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"remote-au/internal/audio"
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
	case "recv", "send":
		return runStub(remaining[1:], stdout, stderr, remaining[0], format, opts.verbose)
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

func runStub(_ []string, _, _ io.Writer, name string, _ audio.Format, _ bool) error {
	return fmt.Errorf("%s: not implemented yet (coming in a later batch)", name)
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
