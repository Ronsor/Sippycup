package libsippycup

import (
	"errors"
	"net"
)

// parseSDPAddress accepts numeric unicast addresses only. Remote SDP must not
// cause DNS queries or reinterpret an IP4 address as IP6 (or vice versa).
func parseSDPAddress(network, family, address string) (net.IP, error) {
	ip := net.ParseIP(address)
	if network != "IN" || ip == nil ||
		(family != "IP4" && family != "IP6") || (family == "IP4") != (ip.To4() != nil) ||
		!(ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return nil, errors.New("SDP requires a numeric unicast address matching its IP family")
	}

	return ip, nil
}

// mediaDestinationAllowed prevents public signaling peers from directing
// unsolicited media into local networks. NAT peers can still establish a
// symmetric RTP path by sending valid media to our advertised socket first.
func mediaDestinationAllowed(destination, signaling net.IP) bool {
	switch {
	case destination.IsLoopback():
		return signaling.IsLoopback()
	case destination.IsLinkLocalUnicast():
		return signaling.IsLinkLocalUnicast()
	case destination.IsPrivate():
		return signaling.IsPrivate() || signaling.IsLoopback()
	default:
		return destination.IsGlobalUnicast()
	}
}
