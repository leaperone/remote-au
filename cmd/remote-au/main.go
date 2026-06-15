package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/discovery"
	"remote-au/internal/stats"
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
	fs.BoolVar(&opts.verbose, "verbose", opts.verbose, "enable verbose audio backend and stats logging")
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
	discoveryPort := 47001
	name := defaultHostname()
	noDiscovery := false
	fs.StringVar(&addr, "addr", addr, "audio listen address")
	fs.IntVar(&deviceIndex, "device", deviceIndex, "playback device index from devices")
	fs.IntVar(&discoveryPort, "discovery-port", discoveryPort, "UDP discovery port")
	fs.StringVar(&name, "name", name, "discovery name")
	fs.BoolVar(&noDiscovery, "no-discovery", noDiscovery, "disable UDP discovery responder")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer func() {
		_ = ln.Close()
	}()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("unexpected TCP listener address type %T", ln.Addr())
	}
	udpConn, err := net.ListenUDP("udp", udpListenAddrFromTCP(tcpAddr))
	if err != nil {
		return fmt.Errorf("listen udp %s: %w", udpListenAddrFromTCP(tcpAddr), err)
	}
	defer func() {
		_ = udpConn.Close()
	}()

	playbackOpts := audio.PlaybackOptions{
		Format:  format,
		Pull:    receiver.Pull,
		Verbose: verbose,
	}
	if deviceIndex >= 0 {
		deviceID, devName, err := audio.PlaybackDeviceByIndex(deviceIndex, verbose)
		if err != nil {
			return err
		}
		playbackOpts.DeviceID = deviceID
		fmt.Fprintf(stdout, "playback device: [%d] %s\n", deviceIndex, devName)
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

	if verbose {
		go printReceiverStats(ctx, stdout, receiver, 2*time.Second)
	}

	discoveryErr := make(chan error, 1)
	if noDiscovery {
		fmt.Fprintln(stdout, "discovery disabled")
	} else if isLoopbackOnlyTCPAddr(tcpAddr) {
		fmt.Fprintf(stdout, "discovery disabled: audio listener is loopback-only (%s), so LAN discovery will not advertise it\n", tcpAddr)
	} else {
		advertisedAddr := advertisedDiscoveryAddr(tcpAddr)
		fmt.Fprintf(stdout, "listening for discovery on UDP :%d as %q\n", discoveryPort, name)
		go func() {
			if err := discovery.RunResponder(ctx, discoveryPort, tcpAddr.Port, name, advertisedAddr, verboseLogf(stdout, verbose)); err != nil && ctx.Err() == nil {
				discoveryErr <- err
				stop()
			}
		}()
	}

	if err := receiver.RunListeners(ctx, ln, udpConn); err != nil {
		return err
	}
	select {
	case err := <-discoveryErr:
		return fmt.Errorf("discovery responder: %w", err)
	default:
	}
	fmt.Fprint(stdout, "\nrecv stopped\n")
	return nil
}

func runSend(args []string, stdout, stderr io.Writer, format audio.Format, verbose bool) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := ""
	peerName := ""
	discoverTimeout := 1500 * time.Millisecond
	discoveryPort := 47001
	name := defaultHostname()
	sourceName := "mic"
	transportName := string(transport.TransportUDP)
	deviceIndex := -1
	fs.StringVar(&to, "to", to, "receiver audio address, for example 127.0.0.1:47000; skips LAN discovery")
	fs.StringVar(&peerName, "peer", peerName, "discovered receiver name to require; discovery trusts the LAN, so use --to or --peer on untrusted networks")
	fs.DurationVar(&discoverTimeout, "discover-timeout", discoverTimeout, "UDP discovery timeout")
	fs.IntVar(&discoveryPort, "discovery-port", discoveryPort, "UDP discovery port")
	fs.StringVar(&name, "name", name, "sender name")
	fs.StringVar(&sourceName, "source", sourceName, "capture source: mic or loopback")
	fs.StringVar(&transportName, "transport", transportName, "audio transport: udp or tcp")
	fs.IntVar(&deviceIndex, "device", deviceIndex, "capture device index from devices; loopback uses playback device index on Windows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("send takes no positional arguments: %v", fs.Args())
	}

	source, err := parseCaptureSource(sourceName)
	if err != nil {
		return err
	}
	senderTransport, err := parseSenderTransport(transportName)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if to == "" {
		fmt.Fprintf(stdout, "discovering receivers on UDP :%d for %s\n", discoveryPort, discoverTimeout)
		peers, err := discovery.Find(ctx, discoveryPort, discoverTimeout, name, verboseLogf(stdout, verbose))
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		to, err = chooseDiscoveredPeer(stdout, peers, peerName)
		if err != nil {
			return err
		}
	}

	captureOpts := audio.CaptureOptions{
		Format:  format,
		Source:  source,
		Verbose: verbose,
	}
	if deviceIndex >= 0 {
		deviceID, devName, err := audio.CaptureDeviceByIndex(deviceIndex, source, verbose)
		if err != nil {
			return err
		}
		captureOpts.DeviceID = deviceID
		fmt.Fprintf(stdout, "capture device: [%d] %s\n", deviceIndex, devName)
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

	fmt.Fprintf(stdout, "send using %s, source=%s, transport=%s\n", format, sourceName, senderTransport)
	fmt.Fprintln(stdout, "Press Ctrl-C to stop.")

	if err := transport.RunSender(ctx, transport.SenderOptions{
		Address:   to,
		Capture:   capture,
		Name:      name,
		Transport: senderTransport,
		Logf:      writerLogf(stdout),
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

func parseSenderTransport(name string) (transport.SenderTransport, error) {
	switch strings.ToLower(name) {
	case "", string(transport.TransportUDP):
		return transport.TransportUDP, nil
	case string(transport.TransportTCP):
		return transport.TransportTCP, nil
	default:
		return "", fmt.Errorf("unknown transport %q (want udp or tcp)", name)
	}
}

func defaultHostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "remote-au"
	}
	return name
}

func udpListenAddrFromTCP(addr *net.TCPAddr) *net.UDPAddr {
	if addr == nil {
		return &net.UDPAddr{}
	}
	return &net.UDPAddr{IP: addr.IP, Port: addr.Port, Zone: addr.Zone}
}

func isLoopbackOnlyTCPAddr(addr *net.TCPAddr) bool {
	return addr != nil && addr.IP != nil && addr.IP.IsLoopback()
}

func advertisedDiscoveryAddr(addr *net.TCPAddr) netip.Addr {
	if addr == nil || addr.IP == nil || addr.IP.IsUnspecified() {
		return netip.AddrFrom4([4]byte{})
	}
	ip4 := addr.IP.To4()
	if ip4 == nil {
		return netip.AddrFrom4([4]byte{})
	}
	var raw [4]byte
	copy(raw[:], ip4)
	return netip.AddrFrom4(raw)
}

func writerLogf(w io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...)
	}
}

func verboseLogf(w io.Writer, verbose bool) func(format string, args ...any) {
	if !verbose {
		return nil
	}
	return writerLogf(w)
}

func chooseDiscoveredPeer(w io.Writer, peers []discovery.Peer, peerName string) (string, error) {
	if len(peers) == 0 {
		return "", fmt.Errorf("no receivers discovered; use --to <ip:port> to connect manually")
	}

	if peerName != "" {
		var match discovery.Peer
		matches := 0
		for _, peer := range peers {
			if peer.Name == peerName {
				match = peer
				matches++
			}
		}
		if matches == 1 {
			printDiscoveredPeer(w, match)
			return match.Addr, nil
		}
		printDiscoveredPeers(w, peers)
		if matches == 0 && len(peers) == 1 {
			return "", fmt.Errorf("--peer %q did not match discovered receiver %s; use --to <ip:port> or the discovered --peer name", peerName, displayPeerName(peers[0].Name))
		}
		if matches == 0 {
			return "", fmt.Errorf("--peer %q did not match any discovered receiver; use --to <ip:port> or one of the listed --peer names", peerName)
		}
		return "", fmt.Errorf("--peer %q matched multiple receiver instances; use --to <ip:port> or a unique --peer name", peerName)
	}

	if len(peers) == 1 {
		printDiscoveredPeer(w, peers[0])
		return peers[0].Addr, nil
	}

	printDiscoveredPeers(w, peers)
	return "", fmt.Errorf("multiple receivers discovered; use --to <ip:port> or --peer <name>")
}

func printDiscoveredPeer(w io.Writer, peer discovery.Peer) {
	fmt.Fprintf(w, "AUTO-CONNECTING to discovered LAN peer %s at %s\n", displayPeerName(peer.Name), peer.Addr)
}

func printDiscoveredPeers(w io.Writer, peers []discovery.Peer) {
	fmt.Fprintln(w, "discovered receivers:")
	for _, peer := range peers {
		fmt.Fprintf(w, "  %s\t%s\n", displayPeerName(peer.Name), peerAddressSummary(peer))
	}
}

func peerAddressSummary(peer discovery.Peer) string {
	if len(peer.Addrs) <= 1 {
		return peer.Addr
	}
	return fmt.Sprintf("%s (observed: %s)", peer.Addr, strings.Join(peer.Addrs, ", "))
}

func displayPeerName(name string) string {
	name = stats.SafeDisplayName(name)
	if name == "" {
		return "(unnamed)"
	}
	return name
}

func printReceiverStats(ctx context.Context, w io.Writer, receiver *transport.Receiver, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, stats.FormatVerbose(receiver.Stats()))
		}
	}
}
