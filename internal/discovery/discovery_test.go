package discovery

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"remote-au/internal/logging"
)

func TestDecodeRejectsMalformedPackets(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "short header",
			data: []byte("RAUD\x01\x01\x00\x00"),
		},
		{
			name: "over max packet bytes",
			data: bytes.Repeat([]byte{0}, MaxPacketBytes+1),
		},
		{
			name: "wrong magic",
			data: []byte("NOPE\x01\x01\x00\x00\x00"),
		},
		{
			name: "wrong version",
			data: []byte("RAUD\x02\x01\x00\x00\x00"),
		},
		{
			name: "wrong type",
			data: []byte("RAUD\x01\x03\x00\x00\x00"),
		},
		{
			name: "query tcp port nonzero",
			data: []byte("RAUD\x01\x01\x12\x34\x00"),
		},
		{
			name: "announce tcp port zero",
			data: []byte("RAUD\x01\x02\x00\x00\x00"),
		},
		{
			name: "name length trailing bytes",
			data: []byte("RAUD\x01\x01\x00\x00\x00x"),
		},
		{
			name: "name length short packet",
			data: []byte("RAUD\x01\x01\x00\x00\x02x"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(tt.data); err == nil {
				t.Fatal("Decode returned nil error")
			}
		})
	}
}

func TestQueryRoundTrip(t *testing.T) {
	packet, err := EncodeQuery(Query{Name: "sender"})
	if err != nil {
		t.Fatalf("EncodeQuery: %v", err)
	}

	msg, err := Decode(packet)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if msg.Type != TypeQuery {
		t.Fatalf("Type=%d, want %d", msg.Type, TypeQuery)
	}
	if msg.Query.Name != "sender" {
		t.Fatalf("Query.Name=%q, want sender", msg.Query.Name)
	}
	if msg.Announce != (Announce{}) {
		t.Fatalf("Announce=%+v, want zero value", msg.Announce)
	}
}

func TestAnnounceRoundTrip(t *testing.T) {
	instanceID := testInstanceID(7)
	advertisedAddr := mustAddr(t, "192.168.1.10")
	packet, err := EncodeAnnounce(Announce{
		TCPPort:        47000,
		InstanceID:     instanceID,
		AdvertisedAddr: advertisedAddr,
		Name:           "recv-host",
	})
	if err != nil {
		t.Fatalf("EncodeAnnounce: %v", err)
	}

	msg, err := Decode(packet)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if msg.Type != TypeAnnounce {
		t.Fatalf("Type=%d, want %d", msg.Type, TypeAnnounce)
	}
	if msg.Announce.TCPPort != 47000 {
		t.Fatalf("Announce.TCPPort=%d, want 47000", msg.Announce.TCPPort)
	}
	if msg.Announce.InstanceID != instanceID {
		t.Fatalf("Announce.InstanceID=%x, want %x", msg.Announce.InstanceID, instanceID)
	}
	if msg.Announce.AdvertisedAddr != advertisedAddr {
		t.Fatalf("Announce.AdvertisedAddr=%s, want %s", msg.Announce.AdvertisedAddr, advertisedAddr)
	}
	if msg.Announce.Name != "recv-host" {
		t.Fatalf("Announce.Name=%q, want recv-host", msg.Announce.Name)
	}
	if msg.Query != (Query{}) {
		t.Fatalf("Query=%+v, want zero value", msg.Query)
	}
}

func TestMaxNameRoundTrip(t *testing.T) {
	name := strings.Repeat("x", MaxNameLen)
	packet, err := EncodeAnnounce(Announce{TCPPort: 1, InstanceID: testInstanceID(1), Name: name})
	if err != nil {
		t.Fatalf("EncodeAnnounce: %v", err)
	}
	if len(packet) != announceNameOffset+MaxNameLen {
		t.Fatalf("len(packet)=%d, want %d", len(packet), announceNameOffset+MaxNameLen)
	}

	msg, err := Decode(packet)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if msg.Announce.Name != name {
		t.Fatalf("Announce.Name length=%d, want %d", len(msg.Announce.Name), len(name))
	}
}

func TestPeerInstancesCollapseMultiAddressSameInstance(t *testing.T) {
	instances := make(map[peerInstanceKey]*peerInstance)
	logger := logging.Nop()
	capLogged := false

	announce := Announce{TCPPort: 47000, InstanceID: testInstanceID(1), Name: "recv-host"}
	addPeerInstance(instances, announce, mustAddr(t, "127.0.0.1"), logger, &capLogged)
	addPeerInstance(instances, announce, mustAddr(t, "169.254.10.20"), logger, &capLogged)
	addPeerInstance(instances, announce, mustAddr(t, "192.168.1.20"), logger, &capLogged)
	addPeerInstance(instances, announce, mustAddr(t, "192.168.1.20"), logger, &capLogged)

	peers := peerInstancesToPeers(instances)
	if len(peers) != 1 {
		t.Fatalf("len(peers)=%d, want 1: %+v", len(peers), peers)
	}
	if peers[0].Name != "recv-host" {
		t.Fatalf("Name=%q, want recv-host", peers[0].Name)
	}
	if peers[0].Addr != "192.168.1.20:47000" {
		t.Fatalf("Addr=%q, want preferred non-loopback non-link-local IPv4", peers[0].Addr)
	}
	wantAddrs := []string{"192.168.1.20:47000", "169.254.10.20:47000", "127.0.0.1:47000"}
	if !reflect.DeepEqual(peers[0].Addrs, wantAddrs) {
		t.Fatalf("Addrs=%v, want %v", peers[0].Addrs, wantAddrs)
	}
}

func TestPeerInstancesKeepDifferentInstanceIDsSeparate(t *testing.T) {
	instances := make(map[peerInstanceKey]*peerInstance)
	logger := logging.Nop()
	capLogged := false
	src := mustAddr(t, "10.0.0.20")

	addPeerInstance(instances, Announce{TCPPort: 47000, InstanceID: testInstanceID(1), Name: "alpha"}, src, logger, &capLogged)
	addPeerInstance(instances, Announce{TCPPort: 47000, InstanceID: testInstanceID(2), Name: "alpha"}, src, logger, &capLogged)

	peers := peerInstancesToPeers(instances)
	if len(peers) != 2 {
		t.Fatalf("len(peers)=%d, want 2: %+v", len(peers), peers)
	}
	got := make([]string, 0, len(peers))
	for _, peer := range peers {
		got = append(got, peer.Name+" "+peer.Addr)
	}
	want := []string{"alpha 10.0.0.20:47000", "alpha 10.0.0.20:47000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peers=%v, want %v", got, want)
	}
}

func TestPeerAdvertisedAddressOverridesSourceAddress(t *testing.T) {
	instances := make(map[peerInstanceKey]*peerInstance)
	logger := logging.Nop()
	capLogged := false

	announce := Announce{
		TCPPort:        47000,
		InstanceID:     testInstanceID(1),
		AdvertisedAddr: mustAddr(t, "192.168.1.10"),
		Name:           "recv-host",
	}
	addPeerInstance(instances, announce, mustAddr(t, "10.0.0.20"), logger, &capLogged)
	addPeerInstance(instances, announce, mustAddr(t, "10.0.0.21"), logger, &capLogged)

	peers := peerInstancesToPeers(instances)
	if len(peers) != 1 {
		t.Fatalf("len(peers)=%d, want 1: %+v", len(peers), peers)
	}
	if peers[0].Addr != "192.168.1.10:47000" {
		t.Fatalf("Addr=%q, want advertised connect address", peers[0].Addr)
	}
	wantAddrs := []string{"10.0.0.20:47000", "10.0.0.21:47000"}
	if !reflect.DeepEqual(peers[0].Addrs, wantAddrs) {
		t.Fatalf("Addrs=%v, want observed source addresses %v", peers[0].Addrs, wantAddrs)
	}
}

func TestPeerInstanceCap(t *testing.T) {
	instances := make(map[peerInstanceKey]*peerInstance)
	logger := logging.Nop()
	capLogged := false
	src := mustAddr(t, "10.0.0.20")

	for i := range MaxPeerInstances {
		addPeerInstance(instances, Announce{TCPPort: 47000, InstanceID: testInstanceID(byte(i)), Name: fmt.Sprintf("peer-%03d", i)}, src, logger, &capLogged)
	}
	overflowID := testInstanceID(250)
	addPeerInstance(instances, Announce{TCPPort: 47000, InstanceID: overflowID, Name: "overflow"}, src, logger, &capLogged)

	if len(instances) != MaxPeerInstances {
		t.Fatalf("len(instances)=%d, want %d", len(instances), MaxPeerInstances)
	}
	if _, ok := instances[peerInstanceKey{instanceID: overflowID}]; ok {
		t.Fatal("overflow instance was collected after cap")
	}
	if !capLogged {
		t.Fatal("capLogged=false, want true")
	}
}

func TestReplyRateLimiter(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newReplyRateLimiter(now)
	for i := range announceReplyBurst {
		if !limiter.Allow(now) {
			t.Fatalf("Allow burst token %d=false, want true", i)
		}
	}
	if limiter.Allow(now) {
		t.Fatal("Allow over burst=true, want false")
	}
	if !limiter.Allow(now.Add(announceReplyInterval)) {
		t.Fatal("Allow after refill=false, want true")
	}
}

func TestRunResponderReturnsOnContextCancel(t *testing.T) {
	port := freeUDPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- RunResponder(ctx, port, 47000, "recv-host", netip.AddrFrom4([4]byte{}), logging.Nop())
	}()

	waitForResponderDiscovery(t, port, done)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunResponder returned error after ctx cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunResponder did not return promptly after ctx cancel")
	}
}

func waitForResponderDiscovery(t *testing.T, port int, done <-chan error) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("RunResponder returned before cancel: %v", err)
		default:
		}

		findCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		peers, err := FindPorts(findCtx, []int{port}, 100*time.Millisecond, "sender", logging.Nop())
		cancel()
		if err == nil && len(peers) == 1 && peers[0].Name == "recv-host" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("RunResponder did not answer discovery")
}

func TestListenFirstFallsBackWhenPreferredPortOccupied(t *testing.T) {
	busy := listenUDPForTest(t, 0)
	defer func() {
		_ = busy.Close()
	}()
	busyPort := busy.LocalAddr().(*net.UDPAddr).Port
	fallbackPort := freeUDPPort(t)

	conn, gotPort, err := ListenFirst([]int{busyPort, fallbackPort})
	if err != nil {
		t.Fatalf("ListenFirst: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	if gotPort != fallbackPort {
		t.Fatalf("ListenFirst port=%d, want fallback %d", gotPort, fallbackPort)
	}
	if conn.LocalAddr().(*net.UDPAddr).Port != fallbackPort {
		t.Fatalf("conn local port=%d, want %d", conn.LocalAddr().(*net.UDPAddr).Port, fallbackPort)
	}
}

func TestListenFirstRejectsInvalidOrEmptyPorts(t *testing.T) {
	tests := []struct {
		name  string
		ports []int
	}{
		{name: "nil"},
		{name: "empty", ports: []int{}},
		{name: "zero", ports: []int{0}},
		{name: "negative", ports: []int{-1}},
		{name: "too high", ports: []int{65536}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, port, err := ListenFirst(tt.ports)
			if err == nil {
				_ = conn.Close()
				t.Fatalf("ListenFirst(%v)=(conn, %d, nil), want error", tt.ports, port)
			}
			if conn != nil || port != 0 {
				t.Fatalf("ListenFirst(%v) returned conn=%v port=%d with error", tt.ports, conn, port)
			}
		})
	}
}

func TestFindPortsDiscoversMultiplePorts(t *testing.T) {
	connA := listenUDPForTest(t, 0)
	connB := listenUDPForTest(t, 0)
	startResponderLoopForTest(t, connA, Announce{TCPPort: 47000, InstanceID: testInstanceID(1), Name: "recv-a"})
	startResponderLoopForTest(t, connB, Announce{TCPPort: 47002, InstanceID: testInstanceID(2), Name: "recv-b"})

	ports := []int{
		connA.LocalAddr().(*net.UDPAddr).Port,
		connB.LocalAddr().(*net.UDPAddr).Port,
	}
	peers, err := FindPorts(context.Background(), ports, 800*time.Millisecond, "sender", nil)
	if err != nil {
		t.Fatalf("FindPorts: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("len(peers)=%d, want 2: %+v", len(peers), peers)
	}
	got := []string{peers[0].Name, peers[1].Name}
	want := []string{"recv-a", "recv-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peer names=%v, want %v", got, want)
	}
}

func TestFindPortsDeduplicatesSameInstanceAcrossPorts(t *testing.T) {
	connA := listenUDPForTest(t, 0)
	connB := listenUDPForTest(t, 0)
	announce := Announce{TCPPort: 47000, InstanceID: testInstanceID(1), Name: "recv-host"}
	startResponderLoopForTest(t, connA, announce)
	startResponderLoopForTest(t, connB, announce)

	ports := []int{
		connA.LocalAddr().(*net.UDPAddr).Port,
		connB.LocalAddr().(*net.UDPAddr).Port,
	}
	peers, err := FindPorts(context.Background(), ports, 800*time.Millisecond, "sender", nil)
	if err != nil {
		t.Fatalf("FindPorts: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("len(peers)=%d, want 1: %+v", len(peers), peers)
	}
	if peers[0].Name != "recv-host" {
		t.Fatalf("peer name=%q, want recv-host", peers[0].Name)
	}
}

func TestResponderLoopReturnsOnFatalSocketError(t *testing.T) {
	conn := listenUDPForTest(t, 0)
	announce, err := EncodeAnnounce(Announce{TCPPort: 47000, InstanceID: testInstanceID(1), Name: "recv-host"})
	if err != nil {
		t.Fatalf("EncodeAnnounce: %v", err)
	}

	done := make(chan error, 1)
	limiter := newReplyRateLimiter(time.Now())
	go func() {
		done <- runResponderLoop(context.Background(), conn, announce, &limiter, nil)
	}()
	_ = conn.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runResponderLoop returned nil after fatal socket close")
		}
	case <-time.After(time.Second):
		t.Fatal("runResponderLoop did not return promptly after fatal socket close")
	}
}

func startResponderLoopForTest(t *testing.T, conn *net.UDPConn, announce Announce) {
	t.Helper()
	packet, err := EncodeAnnounce(announce)
	if err != nil {
		t.Fatalf("EncodeAnnounce: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	limiter := newReplyRateLimiter(time.Now())
	go func() {
		done <- runResponderLoop(ctx, conn, packet, &limiter, nil)
	}()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("runResponderLoop cleanup: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("runResponderLoop did not stop during cleanup")
		}
	})
}

func mustAddr(t *testing.T, raw string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", raw, err)
	}
	return addr
}

func testInstanceID(seed byte) InstanceID {
	var id InstanceID
	for i := range id {
		id[i] = seed + byte(i)
	}
	return id
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn := listenUDPForTest(t, 0)
	defer func() {
		_ = conn.Close()
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func listenUDPForTest(t *testing.T, port int) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("UDP listen unavailable in this environment: %v", err)
		}
		t.Fatalf("ListenUDP: %v", err)
	}
	return conn
}
