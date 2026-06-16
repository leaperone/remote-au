// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/discovery"
	"remote-au/internal/logging"
	"remote-au/internal/stats"
	"remote-au/internal/transport"
)

var version = "dev"

var (
	discoveryFindPorts     = discovery.FindPorts
	encodeDevicesJSON      = audio.EncodeDevicesJSON
	printDevices           = audio.PrintDevices
	playbackDeviceSelector = audio.PlaybackDeviceBySelector
	captureDeviceSelector  = audio.CaptureDeviceBySelector
)

type globalOptions struct {
	rate      int
	channels  int
	frameMS   int
	verbose   bool
	version   bool
	logLevel  string
	logFormat string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	opts := globalOptions{
		rate:      audio.DefaultSampleRate,
		channels:  audio.DefaultChannels,
		frameMS:   audio.DefaultFrameMillis,
		logLevel:  "info",
		logFormat: "text",
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
	opts.logLevel = logging.EffectiveLevel(opts.logLevel, opts.verbose, flagWasSet(globals, "log-level"))
	logger, err := logging.New(stderr, opts.logLevel, opts.logFormat)
	if err != nil {
		return err
	}
	effectiveDebug := strings.EqualFold(opts.logLevel, "debug")

	remaining := globals.Args()
	if opts.version {
		fmt.Fprintln(stdout, resolveVersion())
		return nil
	}
	if len(remaining) == 0 {
		printUsage(stderr, globals)
		return errors.New("missing subcommand")
	}
	if remaining[0] == "version" {
		if len(remaining) != 1 {
			return fmt.Errorf("version takes no arguments: %v", remaining[1:])
		}
		fmt.Fprintln(stdout, resolveVersion())
		return nil
	}

	format, err := audio.NewFormat(opts.rate, opts.channels, opts.frameMS)
	if err != nil {
		return err
	}

	switch remaining[0] {
	case "devices":
		return runDevices(remaining[1:], stdout, stderr, format, effectiveDebug, logger)
	case "selftest":
		return runSelftest(remaining[1:], stdout, stderr, format, effectiveDebug, logger)
	case "recv":
		return runRecv(remaining[1:], stdout, stderr, format, effectiveDebug, logger)
	case "send":
		return runSend(remaining[1:], stdout, stderr, format, effectiveDebug, logger)
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
	fs.BoolVar(&opts.version, "version", opts.version, "print version and exit")
	fs.StringVar(&opts.logLevel, "log-level", opts.logLevel, "diagnostic log level: debug, info, warn, or error")
	fs.StringVar(&opts.logFormat, "log-format", opts.logFormat, "diagnostic log format: text or json")
}

func printUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: remote-au [global flags] <version|devices|selftest|recv|send>\n\n")
	fmt.Fprintln(w, "Global flags:")
	fs.PrintDefaults()
}

func runDevices(args []string, stdout, stderr io.Writer, format audio.Format, verbose bool, logger logging.Logger) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := false
	fs.BoolVar(&jsonOutput, "json", jsonOutput, "emit device lists as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("devices takes no positional arguments: %v", fs.Args())
	}
	if verbose {
		fmt.Fprintf(stderr, "format: %s, %d bytes per frame\n", format, format.BytesPerFrame())
	}
	if jsonOutput {
		return encodeDevicesJSON(stdout, verbose, logger)
	}
	return printDevices(stdout, verbose, logger)
}

func runSelftest(args []string, stdout, _ io.Writer, format audio.Format, verbose bool, logger logging.Logger) error {
	if len(args) != 0 {
		return fmt.Errorf("selftest takes no arguments: %v", args)
	}

	fmt.Fprintf(stdout, "Selftest using %s. Speak into the microphone; you should hear it through playback.\n", format)
	fmt.Fprintln(stdout, "Press Ctrl-C to stop.")

	capture, err := audio.OpenCapture(audio.CaptureOptions{
		Format:  format,
		Source:  audio.SourceMicrophone,
		Verbose: verbose,
		Logger:  logger,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = capture.Close()
	}()

	playback, err := audio.OpenPlaybackWithOptions(audio.PlaybackOptions{
		Format:  format,
		Pull:    func(out []byte, _ uint32) { capture.TryRead(out) },
		Verbose: verbose,
		Logger:  logger,
	})
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

func runRecv(args []string, stdout, stderr io.Writer, format audio.Format, verbose bool, logger logging.Logger) error {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := ":47000"
	deviceSelector := ""
	discoveryPort := 0
	name := defaultHostname()
	noDiscovery := false
	fs.StringVar(&addr, "addr", addr, "audio listen address")
	fs.StringVar(&deviceSelector, "device", deviceSelector, "playback device index or name from devices")
	fs.IntVar(&discoveryPort, "discovery-port", discoveryPort, "UDP discovery port (0 = auto (47001, 48001, 49001))")
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
		Logger: logger,
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
		Logger:  logger,
	}
	if deviceSelectorRequestsSelection(deviceSelector) {
		deviceID, device, err := playbackDeviceSelector(deviceSelector, verbose, logger)
		if err != nil {
			return err
		}
		playbackOpts.DeviceID = deviceID
		fmt.Fprintf(stdout, "playback device: [%d] %s\n", device.Index, device.Name)
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
		go printReceiverStats(ctx, logger, receiver, 2*time.Second)
	}

	discoveryErr := make(chan error, 1)
	if noDiscovery {
		fmt.Fprintln(stdout, "discovery disabled")
	} else if isLoopbackOnlyTCPAddr(tcpAddr) {
		fmt.Fprintf(stdout, "discovery disabled: audio listener is loopback-only (%s), so LAN discovery will not advertise it\n", tcpAddr)
	} else {
		discoveryPorts := discoveryPortsForFlag(discoveryPort)
		discoveryConn, actualDiscoveryPort, err := discovery.ListenFirst(discoveryPorts)
		if err != nil {
			return fmt.Errorf("discovery responder: %w", err)
		}
		advertisedAddr := advertisedDiscoveryAddr(tcpAddr)
		fmt.Fprintf(stdout, "listening for discovery on UDP :%d as %q\n", actualDiscoveryPort, name)
		go func() {
			if err := discovery.RunResponderOnConn(ctx, discoveryConn, tcpAddr.Port, name, advertisedAddr, logger); err != nil && ctx.Err() == nil {
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

func runSend(args []string, stdout, stderr io.Writer, format audio.Format, verbose bool, logger logging.Logger) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := ""
	peerName := ""
	discoverTimeout := 1500 * time.Millisecond
	discoveryPort := 0
	name := defaultHostname()
	sourceName := "mic"
	transportName := string(transport.TransportUDP)
	deviceSelector := ""
	fs.StringVar(&to, "to", to, "receiver audio address, for example 127.0.0.1:47000; skips LAN discovery")
	fs.StringVar(&peerName, "peer", peerName, "discovered receiver name to require; discovery trusts the LAN, so use --to or --peer on untrusted networks")
	fs.DurationVar(&discoverTimeout, "discover-timeout", discoverTimeout, "UDP discovery timeout")
	fs.IntVar(&discoveryPort, "discovery-port", discoveryPort, "UDP discovery port (0 = auto (47001, 48001, 49001))")
	fs.StringVar(&name, "name", name, "sender name")
	fs.StringVar(&sourceName, "source", sourceName, "capture source: mic or loopback")
	fs.StringVar(&transportName, "transport", transportName, "audio transport: udp or tcp")
	fs.StringVar(&deviceSelector, "device", deviceSelector, "capture device index or name from devices; loopback uses playback device index on Windows")
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

	var resolve func(context.Context) (string, error)
	if to == "" {
		// Fail fast on bad discovery config so it never enters the retry loop
		// (only "no receiver found" should be retryable).
		if discoveryPort < 0 || discoveryPort > 65535 {
			return fmt.Errorf("invalid --discovery-port %d: must be 0 (auto) or 1-65535", discoveryPort)
		}
		if discoverTimeout <= 0 {
			return fmt.Errorf("invalid --discover-timeout %s: must be positive", discoverTimeout)
		}
		discoveryPorts := discoveryPortsForFlag(discoveryPort)
		resolve = newDiscoveredPeerResolver(stdout, discoveryPorts, discoverTimeout, name, peerName, logger)
	} else {
		if err := validateReceiverAddress(to); err != nil {
			return err
		}
		resolve = newFixedPeerResolver(to)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	captureOpts := audio.CaptureOptions{
		Format:  format,
		Source:  source,
		Verbose: verbose,
		Logger:  logger,
	}
	if deviceSelectorRequestsSelection(deviceSelector) {
		deviceID, device, err := captureDeviceSelector(deviceSelector, source, verbose, logger)
		if err != nil {
			return err
		}
		captureOpts.DeviceID = deviceID
		fmt.Fprintf(stdout, "capture device: [%d] %s\n", device.Index, device.Name)
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
		Resolve:   resolve,
		Capture:   capture,
		Name:      name,
		Transport: senderTransport,
		Logger:    logger,
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

func deviceSelectorRequestsSelection(selector string) bool {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}
	index, err := strconv.Atoi(selector)
	return err != nil || index >= 0
}

func validateReceiverAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --to address %q: expected host:port", addr)
	}
	if host == "" {
		return fmt.Errorf("invalid --to address %q: host is required", addr)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("invalid --to address %q: port must be 1-65535", addr)
	}
	return nil
}

func newFixedPeerResolver(addr string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		return addr, nil
	}
}

func newDiscoveredPeerResolver(w io.Writer, ports []int, timeout time.Duration, senderName, peerName string, logger logging.Logger) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		fmt.Fprintf(w, "discovering receivers on UDP %s for %s\n", discoveryPortLabel(ports), timeout)
		peers, err := discoveryFindPorts(ctx, ports, timeout, senderName, logger)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				fmt.Fprintf(w, "waiting for a receiver: %v\n", err)
			}
			return "", err
		}
		addr, err := chooseDiscoveredPeer(w, peers, peerName)
		if err != nil {
			fmt.Fprintf(w, "waiting for a receiver: %v\n", err)
			return "", err
		}
		return addr, nil
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

func discoveryPortsForFlag(port int) []int {
	if port == 0 {
		ports := make([]int, len(discovery.DefaultPorts))
		copy(ports, discovery.DefaultPorts)
		return ports
	}
	return []int{port}
}

func discoveryPortLabel(ports []int) string {
	labels := make([]string, 0, len(ports))
	for _, port := range ports {
		labels = append(labels, fmt.Sprintf(":%d", port))
	}
	return strings.Join(labels, ", ")
}

func chooseDiscoveredPeer(w io.Writer, peers []discovery.Peer, peerName string) (string, error) {
	peer, err := selectDiscoveredPeer(peers, peerName)
	if err != nil {
		if len(peers) > 0 && (peerName != "" || len(peers) > 1) {
			printDiscoveredPeers(w, peers)
		}
		return "", err
	}
	printDiscoveredPeer(w, peer)
	return peer.Addr, nil
}

func selectDiscoveredPeer(peers []discovery.Peer, peerName string) (discovery.Peer, error) {
	if len(peers) == 0 {
		return discovery.Peer{}, fmt.Errorf("no receivers discovered; use --to <ip:port> to connect manually")
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
			return match, nil
		}
		if matches == 0 && len(peers) == 1 {
			return discovery.Peer{}, fmt.Errorf("--peer %q did not match discovered receiver %s; use --to <ip:port> or the discovered --peer name", peerName, displayPeerName(peers[0].Name))
		}
		if matches == 0 {
			return discovery.Peer{}, fmt.Errorf("--peer %q did not match any discovered receiver; use --to <ip:port> or one of the listed --peer names", peerName)
		}
		return discovery.Peer{}, fmt.Errorf("--peer %q matched multiple receiver instances; use --to <ip:port> or a unique --peer name", peerName)
	}

	if len(peers) == 1 {
		return peers[0], nil
	}

	return discovery.Peer{}, fmt.Errorf("multiple receivers discovered; use --to <ip:port> or --peer <name>")
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

func printReceiverStats(ctx context.Context, logger logging.Logger, receiver *transport.Receiver, interval time.Duration) {
	if logger == nil {
		logger = logging.Nop()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, line := range strings.Split(strings.TrimSpace(stats.FormatVerbose(receiver.Stats())), "\n") {
				if line != "" {
					logger.Debugf("%s", line)
				}
			}
		}
	}
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func resolveVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return "dev+" + shortRevision(setting.Value)
			}
		}
	}
	return "dev"
}

func shortRevision(rev string) string {
	if len(rev) <= 12 {
		return rev
	}
	return rev[:12]
}
