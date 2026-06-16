package main

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"remote-au/internal/discovery"
	"remote-au/internal/transport"
)

func TestChooseDiscoveredPeerRequiresPeerNameForSingleInstance(t *testing.T) {
	var out bytes.Buffer
	addr, err := chooseDiscoveredPeer(&out, []discovery.Peer{
		{Name: "recv-a", Addr: "10.0.0.20:47000"},
	}, "recv-b")
	if err == nil {
		t.Fatal("chooseDiscoveredPeer returned nil error")
	}
	if addr != "" {
		t.Fatalf("addr=%q, want empty", addr)
	}
	got := out.String()
	if !strings.Contains(got, "recv-a") || !strings.Contains(got, "10.0.0.20:47000") {
		t.Fatalf("output=%q, want discovered peer name and address", got)
	}
}

func TestChooseDiscoveredPeerAutoConnectsMatchingSingleInstance(t *testing.T) {
	var out bytes.Buffer
	addr, err := chooseDiscoveredPeer(&out, []discovery.Peer{
		{Name: "recv-a", Addr: "10.0.0.20:47000"},
	}, "recv-a")
	if err != nil {
		t.Fatalf("chooseDiscoveredPeer: %v", err)
	}
	if addr != "10.0.0.20:47000" {
		t.Fatalf("addr=%q, want 10.0.0.20:47000", addr)
	}
	if got := out.String(); !strings.Contains(got, "AUTO-CONNECTING") {
		t.Fatalf("output=%q, want AUTO-CONNECTING line", got)
	}
}

func TestChooseDiscoveredPeerKeepsSameNameInstancesAmbiguous(t *testing.T) {
	var out bytes.Buffer
	addr, err := chooseDiscoveredPeer(&out, []discovery.Peer{
		{Name: "recv-a", Addr: "10.0.0.20:47000"},
		{Name: "recv-a", Addr: "10.0.0.21:47000"},
	}, "")
	if err == nil {
		t.Fatal("chooseDiscoveredPeer returned nil error")
	}
	if addr != "" {
		t.Fatalf("addr=%q, want empty", addr)
	}
	if !strings.Contains(err.Error(), "multiple receivers discovered") {
		t.Fatalf("err=%v, want multiple receivers error", err)
	}
}

func TestAdvertisedDiscoveryAddr(t *testing.T) {
	zero := netip.AddrFrom4([4]byte{})
	tests := []struct {
		name string
		addr *net.TCPAddr
		want netip.Addr
	}{
		{
			name: "specific IPv4",
			addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.10")},
			want: netip.MustParseAddr("192.168.1.10"),
		},
		{
			name: "unspecified IPv4",
			addr: &net.TCPAddr{IP: net.IPv4zero},
			want: zero,
		},
		{
			name: "unspecified IPv6",
			addr: &net.TCPAddr{IP: net.ParseIP("::")},
			want: zero,
		},
		{
			name: "nil IP",
			addr: &net.TCPAddr{},
			want: zero,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := advertisedDiscoveryAddr(tt.addr); got != tt.want {
				t.Fatalf("advertisedDiscoveryAddr(%v)=%s, want %s", tt.addr, got, tt.want)
			}
		})
	}
}

func TestDiscoveryPortsForFlag(t *testing.T) {
	got := discoveryPortsForFlag(0)
	if !reflect.DeepEqual(got, discovery.DefaultPorts) {
		t.Fatalf("discoveryPortsForFlag(0)=%v, want %v", got, discovery.DefaultPorts)
	}
	got[0] = 1
	if discovery.DefaultPorts[0] == 1 {
		t.Fatal("discoveryPortsForFlag(0) returned discovery.DefaultPorts backing array")
	}

	if got := discoveryPortsForFlag(12345); !reflect.DeepEqual(got, []int{12345}) {
		t.Fatalf("discoveryPortsForFlag(12345)=%v, want [12345]", got)
	}
}

func TestDiscoveryPortLabelUsesResolvedPorts(t *testing.T) {
	got := discoveryPortLabel(discoveryPortsForFlag(0))
	if strings.Contains(got, ":0") {
		t.Fatalf("discoveryPortLabel default=%q, must not contain :0", got)
	}
	for _, want := range []string{":47001", ":48001", ":49001"} {
		if !strings.Contains(got, want) {
			t.Fatalf("discoveryPortLabel default=%q, want %s", got, want)
		}
	}
	if got := discoveryPortLabel(discoveryPortsForFlag(12345)); got != ":12345" {
		t.Fatalf("discoveryPortLabel explicit=%q, want :12345", got)
	}
}

func TestParseSenderTransport(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want transport.SenderTransport
	}{
		{name: "default", want: transport.TransportUDP},
		{name: "udp", in: "udp", want: transport.TransportUDP},
		{name: "tcp", in: "tcp", want: transport.TransportTCP},
		{name: "case insensitive", in: "TCP", want: transport.TransportTCP},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSenderTransport(tt.in)
			if err != nil {
				t.Fatalf("parseSenderTransport(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseSenderTransport(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSenderTransportRejectsUnknown(t *testing.T) {
	if _, err := parseSenderTransport("quic"); err == nil || !strings.Contains(err.Error(), "udp or tcp") {
		t.Fatalf("parseSenderTransport unknown err = %v, want udp/tcp error", err)
	}
}

func TestValidateReceiverAddress(t *testing.T) {
	valid := []string{
		"127.0.0.1:47000",
		"localhost:47000",
		"[::1]:47000",
	}
	for _, addr := range valid {
		if err := validateReceiverAddress(addr); err != nil {
			t.Fatalf("validateReceiverAddress(%q): %v", addr, err)
		}
	}

	invalid := []string{
		":47000",
		"127.0.0.1",
		"127.0.0.1:http",
		"127.0.0.1:",
	}
	for _, addr := range invalid {
		if err := validateReceiverAddress(addr); err == nil {
			t.Fatalf("validateReceiverAddress(%q) returned nil error", addr)
		}
	}
}

func TestFixedPeerResolverReturnsAddress(t *testing.T) {
	addr, err := newFixedPeerResolver("127.0.0.1:47000")(context.Background())
	if err != nil {
		t.Fatalf("fixed resolver: %v", err)
	}
	if addr != "127.0.0.1:47000" {
		t.Fatalf("addr=%q, want 127.0.0.1:47000", addr)
	}
}

func TestDiscoveredPeerResolverRetriesSelectionErrors(t *testing.T) {
	oldFindPorts := discoveryFindPorts
	t.Cleanup(func() {
		discoveryFindPorts = oldFindPorts
	})

	var calls int
	discoveryFindPorts = func(context.Context, []int, time.Duration, string, func(string, ...any)) ([]discovery.Peer, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return []discovery.Peer{{Name: "recv-a", Addr: "10.0.0.20:47000"}}, nil
	}

	var out bytes.Buffer
	resolve := newDiscoveredPeerResolver(&out, []int{47001}, time.Millisecond, "sender", "", false)
	addr, err := resolve(context.Background())
	if err == nil {
		t.Fatal("discovery resolver returned nil error with no peers")
	}
	if addr != "" {
		t.Fatalf("addr=%q, want empty", addr)
	}
	if !strings.Contains(out.String(), "waiting for a receiver") {
		t.Fatalf("output=%q, want waiting log", out.String())
	}

	addr, err = resolve(context.Background())
	if err != nil {
		t.Fatalf("discovery resolver retry: %v", err)
	}
	if addr != "10.0.0.20:47000" {
		t.Fatalf("addr=%q, want 10.0.0.20:47000", addr)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
	got := out.String()
	if !strings.Contains(got, "discovering receivers") || !strings.Contains(got, "AUTO-CONNECTING") {
		t.Fatalf("output=%q, want discovery and auto-connect logs", got)
	}
}
