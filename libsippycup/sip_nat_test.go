package libsippycup

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/stun/v4"
)

func TestRTPSTUNAdvertisesActualSocketMappings(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}

	defer server.Close()
	ports := make(chan int, 2)
	go func() {
		buf := make([]byte, 1500)
		for _, mappedPort := range []int{40000, 45000} {
			n, from, err := server.ReadFromUDP(buf)
			if err != nil {
				return
			}

			var request stun.Message
			if err := stun.Decode(buf[:n], &request); err != nil {
				t.Error(err)

				return
			}

			ports <- from.Port
			mapped := &stun.XORMappedAddress{IP: net.ParseIP("203.0.113.10"), Port: mappedPort}
			// Ignore a response with the wrong transaction ID.
			wrong := stun.MustBuild(stun.TransactionID, stun.BindingSuccess, mapped)
			server.WriteToUDP(wrong.Raw, from)
			response := stun.MustBuild(stun.NewTransactionIDSetter(request.TransactionID), stun.BindingSuccess, mapped)
			server.WriteToUDP(response.Raw, from)
		}
	}()
	n := &Node{
		RTP: &RTPConfig{
			Bind:       "127.0.0.1",
			STUNServer: "stun:" + server.LocalAddr().String(),
		},
	}
	s, err := newSIPMedia(n, "127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()
	if first, second := <-ports, <-ports; first != s.Laddr.Port || second != s.Laddr.Port+1 {
		t.Fatal("STUN did not use actual RTP/RTCP sockets")
	}

	sdp := string(s.LocalSDP())
	for _, want := range []string{
		"c=IN IP4 203.0.113.10",
		"m=audio 40000 RTP/AVP",
		"a=rtcp:45000 IN IP4 203.0.113.10",
	} {
		if !strings.Contains(sdp, want) {
			t.Errorf("missing mapped address %q in SDP", want)
		}
	}
}

func TestRTPSTUNCancellation(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}

	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	s, err := newSIPMediaContext(
		ctx,
		&Node{
			RTP: &RTPConfig{
				Bind:       "127.0.0.1",
				STUNServer: "stun:" + server.LocalAddr().String(),
			},
		},
		"127.0.0.1:5060",
	)
	if err == nil {
		s.Close()
		t.Fatal("unresponsive STUN server succeeded")
	}

	if time.Since(started) > time.Second {
		t.Fatal("STUN discovery did not honor cancellation")
	}
}

func TestRTPKeepaliveDoesNotReplaceApplicationAudio(t *testing.T) {
	var packets []*rtp.Packet
	out := newRTPOutput(
		0,
		101,
		func(p *rtp.Packet) error {
			packets = append(packets, p.Clone())

			return nil
		},
	)
	ctx := context.Background()
	f := Format{"pcmu", 8000, 1}
	if err := out.keepalive(ctx, f, time.Second); err != nil {
		t.Fatal(err)
	}

	out.lastPacket = time.Now().Add(-30 * time.Millisecond)
	if err := out.keepalive(ctx, f, time.Second); err != nil {
		t.Fatal(err)
	}

	if len(packets) != 2 || packets[1].SequenceNumber != packets[0].SequenceNumber+1 {
		t.Fatal("startup did not send two sequential RTP packets")
	}

	for _, sample := range packets[0].Payload {
		if sample != 0xff {
			t.Fatal("startup RTP was not PCMU silence")
		}
	}

	if err := out.send(ctx, Frame{Format: f, Data: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}

	out.keepalive(ctx, f, time.Second)
	if len(packets) != 3 || packets[2].Payload[0] != 1 {
		t.Fatal("keepalive interrupted active application media")
	}

	out.lastPacket = time.Now().Add(-2 * time.Second)
	out.keepalive(ctx, f, time.Second)
	if len(packets) != 4 || packets[3].Timestamp-packets[2].Timestamp < 8000 {
		t.Fatal("idle keepalive did not advance the RTP clock")
	}
}

func TestSIPReceiveWithoutApplicationAudio(t *testing.T) {
	e := testEngine(
		t,
		"source \"in\" {\n type=\"sip\"\n bind=\"127.0.0.1:0\"\n rtp { keepalive=\"0s\" }\n}\n"+echoEndpoint,
	)
	contact := e.getSIP("in").contacts["udp"]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leg, err := e.sipEndpoint(
		ctx,
		ctx,
		&Node{Name: "out", Type: "sip", URI: contact.Address.Addr()},
		"echo",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	defer leg.Close()
	// The remote echo server sends nothing until our endpoint opens the path.
	frame, err := leg.Read(ctx)
	if err != nil || len(frame.Data) != 160 {
		t.Fatalf("receive-only client never opened the RTP path: %v", err)
	}
}
