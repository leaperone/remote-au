package main

import (
	"bytes"
	"net"
	"net/netip"
	"strings"
	"testing"

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
