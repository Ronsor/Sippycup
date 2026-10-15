package libsippycup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/pion/stun/v4"
)

func parseRTPSTUN(raw string) (*stun.URI, error) {
	u, err := stun.ParseURI(raw)
	if err != nil {
		return nil, err
	}

	if u.Scheme != stun.SchemeTypeSTUN || u.Proto != stun.ProtoTypeUDP {
		return nil, errors.New("RTP stun_server must be a stun:host[:port] UDP URL")
	}

	return u, nil
}

// Query the actual media sockets before their RTP readers start. NAT may map
// RTP and RTCP to unrelated ports, so discover and advertise both separately.
func (s *sipMedia) discoverMapping(ctx context.Context, raw string) error {
	u, err := parseRTPSTUN(raw)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Host)
	if err != nil {
		return err
	}

	var server *net.UDPAddr
	for _, ip := range ips {
		if (ip.IP.To4() != nil) == (s.ExternalIP.To4() != nil) {
			server = &net.UDPAddr{IP: ip.IP, Zone: ip.Zone, Port: u.Port}
			break
		}
	}

	if server == nil {
		return errors.New("STUN server has no address matching the media IP family")
	}

	rtpAddr, err := stunBinding(ctx, s.rtp, server)
	if err != nil {
		return err
	}

	rtcpAddr, err := stunBinding(ctx, s.rtcp, server)
	if err != nil {
		return err
	}

	s.ExternalIP, s.ExternalPort, s.ExternalRTCP = rtpAddr.IP, rtpAddr.Port, rtcpAddr

	return nil
}

func stunBinding(ctx context.Context, conn *net.UDPConn, server *net.UDPAddr) (*net.UDPAddr, error) {
	request, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		conn.SetDeadline(time.Now())
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}

		conn.SetDeadline(time.Time{})
	}()
	buf := make([]byte, 2048)
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		deadline := time.Now().Add((250 * time.Millisecond) << attempt)
		if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
			deadline = end
		}

		conn.SetDeadline(deadline)
		if _, err := conn.WriteToUDP(request.Raw, server); err != nil {
			return nil, err
		}

		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					break
				}

				return nil, err
			}

			if !from.IP.Equal(server.IP) || from.Port != server.Port {
				continue
			}

			var response stun.Message
			if stun.Decode(buf[:n], &response) != nil || response.TransactionID != request.TransactionID {
				continue
			}

			if response.Type != stun.BindingSuccess {
				return nil, fmt.Errorf("STUN server returned %s", response.Type)
			}

			var mapped stun.XORMappedAddress
			if err := mapped.GetFrom(&response); err != nil {
				return nil, err
			}

			if mapped.IP.IsUnspecified() || mapped.IP.IsMulticast() || mapped.Port <= 0 || mapped.Port > 65535 {
				return nil, errors.New("invalid STUN mapped address")
			}

			return &net.UDPAddr{IP: append(net.IP(nil), mapped.IP...), Port: mapped.Port}, nil
		}
	}

	return nil, errors.New("STUN binding timed out")
}
