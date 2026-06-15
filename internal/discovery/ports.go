package discovery

import (
	"errors"
	"fmt"
	"net"
)

var DefaultPorts = []int{47001, 48001, 49001}

func normalizeDiscoveryPorts(ports []int) ([]int, error) {
	if len(ports) == 0 {
		return nil, errors.New("at least one discovery port is required")
	}

	seen := make(map[int]struct{}, len(ports))
	normalized := make([]int, 0, len(ports))
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("discovery port out of range: %d", port)
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		normalized = append(normalized, port)
	}
	return normalized, nil
}

func ListenFirst(ports []int) (*net.UDPConn, int, error) {
	normalized, err := normalizeDiscoveryPorts(ports)
	if err != nil {
		return nil, 0, err
	}

	errs := make([]error, 0, len(normalized))
	for _, port := range normalized {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
		if err == nil {
			return conn, port, nil
		}
		errs = append(errs, fmt.Errorf(":%d: %w", port, err))
	}
	return nil, 0, fmt.Errorf("listen discovery udp on %v: %w", normalized, errors.Join(errs...))
}
