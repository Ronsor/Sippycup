package libsippycup

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

type sipCodec struct {
	Name        string
	PayloadType uint8
	SampleRate  uint32
}

var codecPCMU = sipCodec{"PCMU", 0, 8000}

var codecPCMA = sipCodec{"PCMA", 8, 8000}

var codecTelephoneEvent = sipCodec{"telephone-event", 101, 8000}

// sipMedia owns a single audio RTP/RTCP pair. Signaling is handled by sipgo.
// SDP and packets are parsed by Pion; no direct media bypass is permitted.
type sipMedia struct {
	AllowPrivateDestinations bool
	validatedSource          bool
	Codecs                   []sipCodec
	Mode                     string
	Laddr                    net.UDPAddr
	ExternalIP               net.IP
	ExternalPort             int
	ExternalRTCP             *net.UDPAddr
	Symmetric                bool
	SignalingIP              net.IP
	rtp, rtcp                *net.UDPConn
	remote, remoteRTCP       *net.UDPAddr
	common                   []sipCodec
	session                  uint64
	offered                  bool
	log                      *slog.Logger
	received, sent           atomic.Uint64
	wrongSource              atomic.Uint64
	malformed                atomic.Uint64
	blockedRTP, blockedRTCP  atomic.Bool
	remoteMu                 sync.RWMutex
	learned                  bool
	candidate                string
	candidateSSRC            uint32
	candidateSequence        uint16
}

// Init allocates an even RTP port and its following RTCP port.
func (s *sipMedia) Init() error {
	if len(s.Codecs) == 0 {
		return errors.New("no supported SIP codecs")
	}

	for i := 0; i < 100; i++ {
		conn, err := net.ListenUDP("udp", &s.Laddr)
		if err != nil {
			return err
		}

		addr := *conn.LocalAddr().(*net.UDPAddr)
		if addr.Port%2 != 0 || addr.Port == 65535 {
			conn.Close()
			if s.Laddr.Port != 0 {
				return errors.New("RTP port must be even")
			}

			continue
		}

		control, err := net.ListenUDP("udp", &net.UDPAddr{IP: s.Laddr.IP, Port: addr.Port + 1})
		if err != nil {
			conn.Close()
			if s.Laddr.Port != 0 {
				return err
			}

			continue
		}

		s.rtp = conn
		s.rtcp = control
		s.Laddr = addr
		var seed [8]byte
		_, _ = rand.Read(seed[:])
		s.session = binary.BigEndian.Uint64(seed[:])

		return nil
	}

	return errors.New("cannot allocate RTP/RTCP port pair")
}

// Close releases both media sockets and interrupts their readers.
func (s *sipMedia) Close() error {
	if s.rtp != nil {
		s.rtp.Close()
	}

	if s.rtcp != nil {
		s.rtcp.Close()
	}

	return nil
}

// CommonCodecs returns negotiated codecs in local preference order.
// The returned slice must not be modified.
func (s *sipMedia) CommonCodecs() []sipCodec {
	return s.common
}

func (s *sipMedia) audioCodec() sipCodec {
	for _, c := range s.common {
		if c.Name != "telephone-event" {
			return c
		}
	}

	return s.Codecs[0]
}

// RemoteSDP negotiates a single G.711 audio stream and its remote addresses.
// Call it before starting media readers or writers.
func (s *sipMedia) RemoteSDP(body []byte) error {
	var desc sdp.SessionDescription
	if err := desc.Unmarshal(body); err != nil {
		return err
	}

	if len(desc.MediaDescriptions) != 1 {
		return errors.New("exactly one audio media stream is supported")
	}

	m := desc.MediaDescriptions[0]
	if m.MediaName.Media != "audio" ||
		m.MediaName.Port.Value <= 0 ||
		m.MediaName.Port.Value >= 65535 ||
		strings.Join(m.MediaName.Protos, "/") != "RTP/AVP" {
		return errors.New("SIP requires an active RTP/AVP audio stream")
	}

	ci := m.ConnectionInformation
	if ci == nil {
		ci = desc.ConnectionInformation
	}

	if ci == nil || ci.Address == nil {
		return errors.New("missing SDP connection address")
	}

	ip, err := parseSDPAddress(ci.NetworkType, ci.AddressType, ci.Address.Address)
	if err != nil {
		return err
	}
	addr := &net.UDPAddr{IP: ip, Port: m.MediaName.Port.Value}

	remoteCodecs := map[uint8]sipCodec{0: codecPCMU, 8: codecPCMA}
	for _, attr := range m.Attributes {
		if attr.Key != "rtpmap" {
			continue
		}

		fields := strings.Fields(attr.Value)
		if len(fields) != 2 {
			continue
		}

		pt, err := strconv.Atoi(fields[0])
		if err != nil || pt < 0 || pt > 127 {
			continue
		}

		parts := strings.Split(fields[1], "/")
		if len(parts) < 2 || len(parts) > 3 {
			continue
		}

		rate, err := strconv.Atoi(parts[1])
		if err != nil || len(parts) == 3 && parts[2] != "1" {
			continue
		}

		remoteCodecs[uint8(pt)] = sipCodec{Name: parts[0], SampleRate: uint32(rate), PayloadType: uint8(pt)}
	}

	offeredPT := map[int]bool{}
	for _, v := range m.MediaName.Formats {
		if pt, err := strconv.Atoi(v); err == nil {
			offeredPT[pt] = true
		}
	}

	common := []sipCodec{}
	audioSelected := false
	for _, want := range s.Codecs {
		for _, ptText := range m.MediaName.Formats {
			pt, err := strconv.Atoi(ptText)
			if err != nil {
				continue
			}

			got, ok := remoteCodecs[uint8(pt)]
			if !ok || !offeredPT[pt] || !strings.EqualFold(want.Name, got.Name) || want.SampleRate != got.SampleRate {
				continue
			}

			got.Name = want.Name
			if got.Name != "telephone-event" {
				// An answer may accept several codecs from our offer, and
				// the peer may send any of them. When answering an incoming
				// offer ourselves, continue selecting a single audio codec.
				if audioSelected && !s.offered {
					continue
				}

				audioSelected = true
			}

			common = append(common, got)
			break
		}
	}

	if !audioSelected {
		return errors.New("no common G.711 codec")
	}

	rtcpAddr := &net.UDPAddr{IP: addr.IP, Port: addr.Port + 1}
	if value, ok := m.Attribute("rtcp"); ok {
		fields := strings.Fields(value)
		if len(fields) != 1 && len(fields) != 4 {
			return errors.New("invalid SDP RTCP address")
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || port < 1 || port > 65535 {
			return errors.New("invalid SDP RTCP port")
		}
		rtcpAddr.Port = port
		if len(fields) == 4 {
			rtcpAddr.IP, err = parseSDPAddress(fields[1], fields[2], fields[3])
			if err != nil {
				return err
			}
		}
	}

	s.common = common
	s.learned = false
	s.blockedRTP.Store(false)
	s.blockedRTCP.Store(false)
	s.candidate = ""
	s.remoteMu.Lock()
	s.remote = addr
	s.validatedSource = false
	s.remoteMu.Unlock()
	s.remoteRTCP = rtcpAddr

	return nil
}

// LocalSDP builds an offer or answer with configured advertised addresses.
// Calling it before RemoteSDP marks this session as the offerer.
func (s *sipMedia) LocalSDP() []byte {
	codecs := s.common
	if len(codecs) == 0 {
		codecs = s.Codecs
		s.offered = true
	}

	family := "IP4"
	if s.ExternalIP.To4() == nil {
		family = "IP6"
	}

	pts := []string{}
	for _, c := range codecs {
		pts = append(pts, strconv.Itoa(int(c.PayloadType)))
	}

	var b strings.Builder
	port := s.Laddr.Port
	if s.ExternalPort != 0 {
		port = s.ExternalPort
	}

	fmt.Fprintf(&b, "v=0\r\n"+
		"o=- %d 1 IN %s %s\r\n"+
		"s=sippycup\r\n"+
		"c=IN %s %s\r\n"+
		"t=0 0\r\n"+
		"m=audio %d RTP/AVP %s\r\n"+
		"a=sendrecv\r\n"+
		"a=ptime:20\r\n", s.session, family, s.ExternalIP, family, s.ExternalIP, port, strings.Join(pts, " "))
	if s.ExternalRTCP != nil {
		controlFamily := "IP4"
		if s.ExternalRTCP.IP.To4() == nil {
			controlFamily = "IP6"
		}

		fmt.Fprintf(
			&b,
			"a=rtcp:%d IN %s %s\r\n",
			s.ExternalRTCP.Port,
			controlFamily,
			s.ExternalRTCP.IP,
		)
	}

	for _, c := range codecs {
		fmt.Fprintf(&b, "a=rtpmap:%d %s/%d\r\n", c.PayloadType, c.Name, c.SampleRate)
		if c.Name == "telephone-event" {
			fmt.Fprintf(&b, "a=fmtp:%d 0-15\r\n", c.PayloadType)
		}
	}

	return []byte(b.String())
}

// ReadRTP reads a valid packet from the negotiated or learned source.
// It drops malformed and disallowed packets and updates diagnostic counters.
// Only one reader may call it at a time.
func (s *sipMedia) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	for {
		n, addr, err := s.rtp.ReadFromUDP(buf)
		if err != nil {
			return 0, err
		}

		if err = p.Unmarshal(buf[:n]); err != nil || p.Version != 2 {
			s.malformed.Add(1)
			continue
		}

		if !s.acceptRTPSource(addr, p) {
			if s.wrongSource.Add(1) == 1 && s.log != nil {
				s.log.Debug(
					"SIP RTP source mismatch",
					"received_from",
					addr.String(),
					"expected_from",
					s.rtpDestination(),
					"symmetric",
					s.Symmetric,
				)
			}

			continue
		}

		if s.received.Add(1) == 1 && s.log != nil {
			s.log.Debug(
				"SIP first RTP received",
				"remote",
				addr.String(),
				"payload_type",
				p.PayloadType,
				"payload_bytes",
				len(p.Payload),
			)
		}

		return n, nil
	}
}

func (s *sipMedia) rtpDestination() *net.UDPAddr {
	s.remoteMu.RLock()
	defer s.remoteMu.RUnlock()

	return s.remote // Addresses are immutable; learning replaces the pointer.
}

// Called by the single RTP reader. Learn once per session, after two sequential
// packets from one SSRC with a negotiated payload type. Restrict candidate IPs
// to the SDP media host or signaling peer rather than accepting arbitrary senders.
func (s *sipMedia) acceptRTPSource(addr *net.UDPAddr, p *rtp.Packet) bool {
	remote := s.rtpDestination()
	if remote == nil {
		return false
	}

	validPayload := false
	for _, c := range s.common {
		if c.PayloadType == p.PayloadType && len(p.Payload) > 0 {
			validPayload = true
			break
		}
	}

	if addr.IP.Equal(remote.IP) && addr.Port == remote.Port {
		if validPayload {
			s.remoteMu.Lock()
			s.validatedSource = true
			s.remoteMu.Unlock()
		}
		if s.Symmetric && validPayload {
			s.learned = true
		}

		return true
	}

	if !s.Symmetric || s.learned || !validPayload || !(addr.IP.Equal(remote.IP) || addr.IP.Equal(s.SignalingIP)) {
		return false
	}

	if s.candidate != addr.String() || s.candidateSSRC != p.SSRC || p.SequenceNumber != s.candidateSequence+1 {
		s.candidate = addr.String()
		s.candidateSSRC = p.SSRC
		s.candidateSequence = p.SequenceNumber

		return false
	}

	learned := *addr
	s.remoteMu.Lock()
	s.remote = &learned
	s.validatedSource = true
	s.remoteMu.Unlock()
	s.learned = true
	if s.log != nil {
		s.log.Debug(
			"SIP symmetric RTP learned",
			"sdp_remote",
			remote.String(),
			"remote",
			addr.String(),
		)
	}

	return true
}

// WriteRTP sends a packet to the current negotiated or learned destination.
func (s *sipMedia) WriteRTP(p *rtp.Packet) error {
	s.remoteMu.RLock()
	remote, validated := s.remote, s.validatedSource
	s.remoteMu.RUnlock()
	if remote == nil {
		return errors.New("RTP remote unavailable")
	}
	if !s.AllowPrivateDestinations && !validated && !mediaDestinationAllowed(remote.IP, s.SignalingIP) {
		if s.blockedRTP.CompareAndSwap(false, true) && s.log != nil {
			s.log.Debug("SIP RTP destination awaiting inbound media", "remote", remote, "signaling_ip", s.SignalingIP)
		}

		return nil // Wait for received RTP to establish reachability before replying.
	}

	b, err := p.Marshal()
	if err != nil {
		return err
	}

	_, err = s.rtp.WriteToUDP(b, remote)
	if err == nil && s.sent.Add(1) == 1 && s.log != nil {
		s.log.Debug(
			"SIP first RTP sent",
			"remote",
			remote.String(),
			"payload_type",
			p.PayloadType,
			"payload_bytes",
			len(p.Payload),
		)
	}

	return err
}

// ReadRTCPRaw reads a control datagram without interpreting its contents.
func (s *sipMedia) ReadRTCPRaw(b []byte) (int, net.Addr, error) {
	return s.rtcp.ReadFrom(b)
}

// WriteRTCP sends a control packet to the remote SDP control address.
func (s *sipMedia) WriteRTCP(p rtcp.Packet) error {
	if s.remoteRTCP == nil {
		return nil
	}
	if !s.AllowPrivateDestinations && !mediaDestinationAllowed(s.remoteRTCP.IP, s.SignalingIP) {
		if s.blockedRTCP.CompareAndSwap(false, true) && s.log != nil {
			s.log.Debug("SIP RTCP destination blocked", "remote", s.remoteRTCP, "signaling_ip", s.SignalingIP)
		}

		return nil
	}

	b, err := p.Marshal()
	if err != nil {
		return err
	}

	_, err = s.rtcp.WriteToUDP(b, s.remoteRTCP)

	return err
}
