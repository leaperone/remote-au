package discovery

import (
	"net"
	"net/netip"
)

func discoveryTargets(discoveryPort int) []netip.AddrPort {
	seen := make(map[netip.Addr]struct{})
	targets := make([]netip.AddrPort, 0, 4)
	add := func(addr netip.Addr) {
		if !addr.Is4() {
			return
		}
		if _, ok := seen[addr]; ok {
			return
		}
		seen[addr] = struct{}{}
		targets = append(targets, netip.AddrPortFrom(addr, uint16(discoveryPort)))
	}

	add(netip.AddrFrom4([4]byte{127, 0, 0, 1}))
	add(netip.AddrFrom4([4]byte{255, 255, 255, 255}))
	for _, addr := range interfaceBroadcastAddrs() {
		add(addr)
	}
	return targets
}

func interfaceBroadcastAddrs() []netip.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var addrs []netip.Addr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range ifaceAddrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || len(ipNet.Mask) != net.IPv4len {
				continue
			}

			var raw [4]byte
			for i := range raw {
				raw[i] = ip4[i] | ^ipNet.Mask[i]
			}
			broadcast := netip.AddrFrom4(raw)
			if broadcast.IsUnspecified() {
				continue
			}
			addrs = append(addrs, broadcast)
		}
	}
	return addrs
}
