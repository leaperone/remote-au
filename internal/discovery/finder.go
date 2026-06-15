package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"time"
)

const MaxPeerInstances = 128

// queryResendInterval re-broadcasts the discovery query during the window so a
// lost UDP query or a responder that started slightly late is still found.
const queryResendInterval = 300 * time.Millisecond

type Peer struct {
	InstanceID InstanceID
	Name       string
	Addr       string
	Addrs      []string
}

type peerInstanceKey struct {
	instanceID InstanceID
}

type peerInstance struct {
	instanceID     InstanceID
	name           string
	tcpPort        int
	advertisedAddr netip.Addr
	addrs          map[netip.Addr]struct{}
}

func Find(ctx context.Context, discoveryPort int, timeout time.Duration, name string, logf func(format string, args ...any)) ([]Peer, error) {
	if discoveryPort <= 0 || discoveryPort > 65535 {
		return nil, fmt.Errorf("discovery port out of range: %d", discoveryPort)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("discovery timeout must be positive")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	query, err := EncodeQuery(Query{Name: name})
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen discovery udp ephemeral: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	targets := discoveryTargets(discoveryPort)
	sendQueries := func() {
		for _, target := range targets {
			if _, err := conn.WriteToUDPAddrPort(query, target); err != nil {
				logf("discovery query to %s failed: %v", target, err)
			}
		}
	}
	sendQueries()

	deadline := time.Now().Add(timeout)
	nextQuery := time.Now().Add(queryResendInterval)
	buf := make([]byte, MaxPacketBytes+1)
	instances := make(map[peerInstanceKey]*peerInstance)
	capLogged := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		if !now.Before(nextQuery) {
			sendQueries()
			nextQuery = now.Add(queryResendInterval)
		}
		readDeadline := now.Add(readDeadlineInterval)
		if readDeadline.After(deadline) {
			readDeadline = deadline
		}
		if err := conn.SetReadDeadline(readDeadline); err != nil {
			return nil, fmt.Errorf("set discovery read deadline: %w", err)
		}

		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil, nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, fmt.Errorf("read discovery reply: %w", err)
		}

		msg, err := Decode(buf[:n])
		if err != nil {
			logf("ignoring malformed discovery reply from %s: %v", src, err)
			continue
		}
		if msg.Type != TypeAnnounce {
			continue
		}

		addPeerInstance(instances, msg.Announce, src.Addr(), logf, &capLogged)
	}

	return peerInstancesToPeers(instances), nil
}

func addPeerInstance(instances map[peerInstanceKey]*peerInstance, announce Announce, src netip.Addr, logf func(format string, args ...any), capLogged *bool) {
	key := peerInstanceKey{instanceID: announce.InstanceID}
	instance := instances[key]
	if instance == nil {
		if len(instances) >= MaxPeerInstances {
			if capLogged != nil && !*capLogged {
				logf("discovery peer instance cap reached (%d); ignoring additional instances", MaxPeerInstances)
				*capLogged = true
			}
			return
		}
		instance = &peerInstance{
			instanceID:     announce.InstanceID,
			name:           announce.Name,
			tcpPort:        announce.TCPPort,
			advertisedAddr: normalizeAdvertisedAddr(announce.AdvertisedAddr),
			addrs:          make(map[netip.Addr]struct{}),
		}
		instances[key] = instance
	} else if !isSpecifiedAdvertisedAddr(instance.advertisedAddr) && isSpecifiedAdvertisedAddr(announce.AdvertisedAddr) {
		instance.advertisedAddr = normalizeAdvertisedAddr(announce.AdvertisedAddr)
	}
	instance.addrs[src.Unmap()] = struct{}{}
}

func peerInstancesToPeers(instances map[peerInstanceKey]*peerInstance) []Peer {
	peers := make([]Peer, 0, len(instances))
	for _, instance := range instances {
		addrs := sortedPeerAddrs(instance)
		if len(addrs) == 0 {
			continue
		}
		peerAddrs := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			peerAddrs = append(peerAddrs, net.JoinHostPort(addr.String(), strconv.Itoa(instance.tcpPort)))
		}
		peers = append(peers, Peer{
			InstanceID: instance.instanceID,
			Name:       instance.name,
			Addr:       peerConnectAddr(instance, addrs),
			Addrs:      peerAddrs,
		})
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name != peers[j].Name {
			return peers[i].Name < peers[j].Name
		}
		if peers[i].Addr != peers[j].Addr {
			return peers[i].Addr < peers[j].Addr
		}
		return bytes.Compare(peers[i].InstanceID[:], peers[j].InstanceID[:]) < 0
	})
	return peers
}

func peerConnectAddr(instance *peerInstance, sortedAddrs []netip.Addr) string {
	if isSpecifiedAdvertisedAddr(instance.advertisedAddr) {
		return net.JoinHostPort(instance.advertisedAddr.String(), strconv.Itoa(instance.tcpPort))
	}
	return net.JoinHostPort(sortedAddrs[0].String(), strconv.Itoa(instance.tcpPort))
}

func normalizeAdvertisedAddr(addr netip.Addr) netip.Addr {
	if !addr.IsValid() {
		return netip.AddrFrom4([4]byte{})
	}
	return addr.Unmap()
}

func isSpecifiedAdvertisedAddr(addr netip.Addr) bool {
	addr = normalizeAdvertisedAddr(addr)
	return addr.IsValid() && addr.Is4() && !addr.IsUnspecified()
}

func sortedPeerAddrs(instance *peerInstance) []netip.Addr {
	addrs := make([]netip.Addr, 0, len(instance.addrs))
	for addr := range instance.addrs {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		leftScore := peerAddrPreference(addrs[i])
		rightScore := peerAddrPreference(addrs[j])
		if leftScore != rightScore {
			return leftScore < rightScore
		}
		return addrs[i].String() < addrs[j].String()
	})
	return addrs
}

func peerAddrPreference(addr netip.Addr) int {
	addr = addr.Unmap()
	if addr.Is4() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast() {
		return 0
	}
	if !addr.IsLoopback() && !addr.IsLinkLocalUnicast() {
		return 1
	}
	if !addr.IsLoopback() {
		return 2
	}
	return 3
}
