package discovery

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	MaxNameLen = 255

	InstanceIDLen      = 16
	advertisedIPv4Len  = 4
	headerLen          = 9
	announceFixedLen   = InstanceIDLen + advertisedIPv4Len
	announceNameOffset = headerLen + announceFixedLen
	MaxPacketBytes     = announceNameOffset + MaxNameLen
	version            = 1
)

var magic = [4]byte{'R', 'A', 'U', 'D'}

type MessageType uint8

const (
	TypeQuery    MessageType = 1
	TypeAnnounce MessageType = 2
)

type InstanceID [InstanceIDLen]byte

type Query struct {
	Name string
}

type Announce struct {
	TCPPort        int
	InstanceID     InstanceID
	AdvertisedAddr netip.Addr
	Name           string
}

type Message struct {
	Type     MessageType
	Query    Query
	Announce Announce
}

func EncodeQuery(q Query) ([]byte, error) {
	return encode(TypeQuery, 0, q.Name)
}

func EncodeAnnounce(a Announce) ([]byte, error) {
	if a.TCPPort <= 0 || a.TCPPort > 65535 {
		return nil, fmt.Errorf("announce tcp port out of range: %d", a.TCPPort)
	}
	return encodeAnnounce(a)
}

func Decode(packet []byte) (Message, error) {
	if len(packet) > MaxPacketBytes {
		return Message{}, fmt.Errorf("packet too large: %d > %d", len(packet), MaxPacketBytes)
	}
	if len(packet) < headerLen {
		return Message{}, fmt.Errorf("packet too short: %d < %d", len(packet), headerLen)
	}
	if !equalMagic(packet[:4]) {
		return Message{}, errors.New("invalid magic")
	}
	if packet[4] != version {
		return Message{}, fmt.Errorf("unsupported version: %d", packet[4])
	}

	msgType := MessageType(packet[5])
	switch msgType {
	case TypeQuery, TypeAnnounce:
	default:
		return Message{}, fmt.Errorf("unknown message type: %d", packet[5])
	}

	tcpPort := int(binary.BigEndian.Uint16(packet[6:8]))
	nameLen := int(packet[8])
	switch msgType {
	case TypeQuery:
		wantLen := headerLen + nameLen
		if len(packet) != wantLen {
			return Message{}, fmt.Errorf("invalid query packet length: got %d, want %d", len(packet), wantLen)
		}
		if tcpPort != 0 {
			return Message{}, fmt.Errorf("query tcp port must be zero: %d", tcpPort)
		}
		return Message{
			Type:  msgType,
			Query: Query{Name: string(packet[headerLen:wantLen])},
		}, nil
	case TypeAnnounce:
		if len(packet) < announceNameOffset {
			return Message{}, fmt.Errorf("announce packet too short: %d < %d", len(packet), announceNameOffset)
		}
		wantLen := announceNameOffset + nameLen
		if len(packet) != wantLen {
			return Message{}, fmt.Errorf("invalid announce packet length: got %d, want %d", len(packet), wantLen)
		}
		if tcpPort == 0 {
			return Message{}, errors.New("announce tcp port must be nonzero")
		}
		var instanceID InstanceID
		copy(instanceID[:], packet[headerLen:headerLen+InstanceIDLen])
		var advertisedIPv4 [4]byte
		copy(advertisedIPv4[:], packet[headerLen+InstanceIDLen:announceNameOffset])
		return Message{
			Type: msgType,
			Announce: Announce{
				TCPPort:        tcpPort,
				InstanceID:     instanceID,
				AdvertisedAddr: netip.AddrFrom4(advertisedIPv4),
				Name:           string(packet[announceNameOffset:wantLen]),
			},
		}, nil
	default:
		return Message{}, fmt.Errorf("unknown message type: %d", msgType)
	}
}

func encode(msgType MessageType, tcpPort int, name string) ([]byte, error) {
	if len(name) > MaxNameLen {
		return nil, fmt.Errorf("name too long: %d > %d", len(name), MaxNameLen)
	}
	packetLen := headerLen + len(name)
	if packetLen > MaxPacketBytes {
		return nil, fmt.Errorf("packet too large: %d > %d", packetLen, MaxPacketBytes)
	}

	packet := make([]byte, packetLen)
	copy(packet[:4], magic[:])
	packet[4] = version
	packet[5] = byte(msgType)
	binary.BigEndian.PutUint16(packet[6:8], uint16(tcpPort))
	packet[8] = byte(len(name))
	copy(packet[headerLen:], name)
	return packet, nil
}

func encodeAnnounce(a Announce) ([]byte, error) {
	if len(a.Name) > MaxNameLen {
		return nil, fmt.Errorf("name too long: %d > %d", len(a.Name), MaxNameLen)
	}
	advertisedIPv4, err := advertisedAddrBytes(a.AdvertisedAddr)
	if err != nil {
		return nil, err
	}
	packetLen := announceNameOffset + len(a.Name)
	if packetLen > MaxPacketBytes {
		return nil, fmt.Errorf("packet too large: %d > %d", packetLen, MaxPacketBytes)
	}

	packet := make([]byte, packetLen)
	copy(packet[:4], magic[:])
	packet[4] = version
	packet[5] = byte(TypeAnnounce)
	binary.BigEndian.PutUint16(packet[6:8], uint16(a.TCPPort))
	packet[8] = byte(len(a.Name))
	copy(packet[headerLen:headerLen+InstanceIDLen], a.InstanceID[:])
	copy(packet[headerLen+InstanceIDLen:announceNameOffset], advertisedIPv4[:])
	copy(packet[announceNameOffset:], a.Name)
	return packet, nil
}

func advertisedAddrBytes(addr netip.Addr) ([4]byte, error) {
	if !addr.IsValid() || addr.IsUnspecified() {
		return [4]byte{}, nil
	}
	addr = addr.Unmap()
	if !addr.Is4() {
		return [4]byte{}, fmt.Errorf("advertised connect address must be IPv4: %s", addr)
	}
	return addr.As4(), nil
}

func equalMagic(b []byte) bool {
	return len(b) == len(magic) &&
		b[0] == magic[0] &&
		b[1] == magic[1] &&
		b[2] == magic[2] &&
		b[3] == magic[3]
}
