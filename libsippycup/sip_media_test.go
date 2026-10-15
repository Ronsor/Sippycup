package libsippycup

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

func TestSIPAnswerAcceptsAllNegotiatedCodecs(t *testing.T) {
	body := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 127.0.0.1\r\n" +
		"s=test\r\n" +
		"c=IN IP4 127.0.0.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 10000 RTP/AVP 0 8 101\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\n")
	for _, offered := range []bool{false, true} {
		s := &sipMedia{
			Codecs:     []sipCodec{codecPCMU, codecPCMA, codecTelephoneEvent},
			offered:    offered,
			ExternalIP: net.ParseIP("127.0.0.1"),
			Laddr:      net.UDPAddr{Port: 10002},
		}
		if err := s.RemoteSDP(body); err != nil {
			t.Fatal(err)
		}

		want := 2 // A locally generated answer selects one audio codec plus DTMF.
		if offered {
			want = 3 // A received answer can allow both PCMU and PCMA.
		}

		if len(s.CommonCodecs()) != want {
			t.Fatalf("offered=%t: codecs %v", offered, s.CommonCodecs())
		}

		if s.audioCodec().Name != "PCMU" {
			t.Fatal("outgoing codec preference changed")
		}

		if !offered {
			var desc sdp.SessionDescription
			if err := desc.Unmarshal(s.LocalSDP()); err != nil {
				t.Fatal(err)
			}

			if len(desc.MediaDescriptions[0].MediaName.Formats) != 2 {
				t.Fatal("incoming offer response unexpectedly enabled multiple audio codecs")
			}
		}
	}
}

func TestSymmetricRTPNATRoundTrip(t *testing.T) {
	for _, symmetric := range []bool{false, true} {
		t.Run(fmt.Sprintf("symmetric=%t", symmetric), func(t *testing.T) {
			peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
			if err != nil {
				t.Fatal(err)
			}

			defer peer.Close()
			s, err := newSIPMedia(&Node{RTP: &RTPConfig{Bind: "127.0.0.1", Symmetric: &symmetric}}, "127.0.0.1:5060")
			if err != nil {
				t.Fatal(err)
			}

			defer s.Close()
			// The phone advertises its public mapping, but is reachable directly
			// on the same LAN as the gateway and sends from its local address.
			body := fmt.Sprintf("v=0\r\n"+
				"o=- 1 1 IN IP4 203.0.113.1\r\n"+
				"s=test\r\n"+
				"c=IN IP4 203.0.113.1\r\n"+
				"t=0 0\r\n"+
				"m=audio %d RTP/AVP 0\r\n", peer.LocalAddr().(*net.UDPAddr).Port)
			if err := s.RemoteSDP([]byte(body)); err != nil {
				t.Fatal(err)
			}

			p := &rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: 0, SSRC: 1234},
				Payload: make([]byte, 160),
			}
			for seq := uint16(1); seq <= 2; seq++ {
				p.SequenceNumber = seq
				b, _ := p.Marshal()
				if _, err := peer.WriteToUDP(b, &s.Laddr); err != nil {
					t.Fatal(err)
				}
			}

			s.rtp.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			var got rtp.Packet
			buf := make([]byte, 1500)
			_, err = s.ReadRTP(buf, &got)
			if !symmetric {
				if err == nil || s.received.Load() != 0 || s.wrongSource.Load() != 2 {
					t.Fatal("strict mode accepted a different media source")
				}

				return
			}

			if err != nil || got.SequenceNumber != 2 {
				t.Fatalf("NAT media was not learned after two packets: %v", err)
			}

			if err := s.WriteRTP(p); err != nil {
				t.Fatal(err)
			}

			peer.SetReadDeadline(time.Now().Add(time.Second))
			if _, from, err := peer.ReadFromUDP(buf); err != nil || from.Port != s.Laddr.Port {
				t.Fatalf("return RTP did not reach learned address: %v", err)
			}
		})
	}
}

func TestSymmetricRTPLearningRestrictions(t *testing.T) {
	s := &sipMedia{
		Symmetric:   true,
		SignalingIP: net.ParseIP("192.0.2.2"),
		remote:      &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 10000},
		common:      []sipCodec{codecPCMU},
	}
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 20000}
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SSRC: 1},
		Payload: []byte{0},
	}
	for seq := uint16(1); seq <= 3; seq++ {
		p.SequenceNumber = seq
		if s.acceptRTPSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.3"), Port: 20000}, p) {
			t.Fatal("unrelated IP was learned")
		}
	}

	p.PayloadType = 99
	if s.acceptRTPSource(peer, p) {
		t.Fatal("unnegotiated codec was accepted")
	}

	p.PayloadType = 0
	if s.acceptRTPSource(peer, p) {
		t.Fatal("address learned from only one packet")
	}

	p.SequenceNumber = 7
	if s.acceptRTPSource(peer, p) {
		t.Fatal("nonsequential packets established an address")
	}

	p.SequenceNumber = 8
	if !s.acceptRTPSource(peer, p) {
		t.Fatal("valid signaling peer was not learned")
	}

	for seq := uint16(9); seq <= 11; seq++ {
		p.SequenceNumber = seq
		if s.acceptRTPSource(&net.UDPAddr{IP: peer.IP, Port: 30000}, p) {
			t.Fatal("established media address was replaced")
		}
	}
}
