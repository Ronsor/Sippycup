package libsippycup

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func TestRTPInitialRandomness(t *testing.T) {
	var ssrcBits uint32
	for range 128 {
		out := newRTPOutput(0, -1, nil)
		ssrcBits |= out.ssrc
	}

	// ASCII-hex seeds can never set the high bit of any byte. For uniform
	// 32-bit seeds, missing any bit over 128 samples is vanishingly unlikely.
	if ssrcBits != ^uint32(0) {
		t.Fatalf("SSRC generator does not use the full 32-bit space: %08x", ssrcBits)
	}
}

func TestDynamicOutboundCredentialValidation(t *testing.T) {
	c, err := fixtureConfig([]byte(`credential "digest" {
 username = "alice"
 digest {
  realm = "test"
  algorithm = "MD5"
  ha1 = "00000000000000000000000000000000"
 }
}

credential "plain" {
 username = "alice"
 password = "secret"
}

source "audio" {
 type = "audiosocket"
 bind = "127.0.0.1:0"
}

endpoint "web" {
 type = "webrtc_http"
 url = "http://unused.invalid/calls"
 auth {
  credential = request.source == "audio" ? credential.digest : credential.plain
 }
}

route "call" {
 connect { endpoint = endpoint.web }
}

`), "credentials.hcl")
	if err != nil {
		t.Fatal(err)
	}

	_, eval, err := c.selectRoute(&Request{Source: "audio"})
	if err != nil {
		t.Fatal(err)
	}

	n := *c.endpoint("web")
	for _, protocol := range []string{"audiosocket", "audiosocket_ws", "webrtc_http"} {
		n.Type = protocol
		if _, err := c.outboundCredential(&n, eval, nil); err == nil {
			t.Fatalf("%s accepted a digest-only outbound credential", protocol)
		}
	}

	n.Type = "sip"
	if _, err := c.outboundCredential(&n, eval, nil); err != nil {
		t.Fatalf("SIP rejected a reusable HA1 credential: %v", err)
	}

	digest := c.credential("digest")
	digest.Digest = nil
	digest.PasswordHash = "verification-only"
	if _, err := c.outboundCredential(&n, eval, nil); err == nil {
		t.Fatal("SIP accepted a non-reusable password hash")
	}

	// Guard the protocol entry points too, including callers that bypass HCL.
	e := NewEngine(c)
	defer e.Close()
	ctx := context.Background()
	if _, err := e.webEndpoint(ctx, ctx, &n, digest); err == nil {
		t.Fatal("WebRTC accepted a nil password")
	}

	if _, err := e.audioEndpoint(ctx, ctx, &n, nil, nil, nil, digest, Format{}); err == nil {
		t.Fatal("AudioSocket accepted a nil password")
	}
}

func TestHTTPMetadataRedactsCredentials(t *testing.T) {
	r, _ := http.NewRequest("GET", "http://localhost/", nil)
	r.Header = http.Header{
		"authorization":       {"secret"},
		"pRoXy-AuThOrIzAtIoN": {"proxy-secret"},
		"cookie":              {"session=secret"},
		"x-call-id":           {"public"},
	}

	headers := httpRequestInfo(r)["headers"].(map[string]any)
	if len(headers) != 1 || headers["X-Call-Id"] == nil {
		t.Fatalf("unexpected metadata headers: %v", headers)
	}
}

func TestSIPStreamPortValidation(t *testing.T) {
	for _, port := range []int{0, 5060} {
		n := &Node{Type: "sip", Bind: fmt.Sprintf("127.0.0.1:%d", port), Transports: []string{"tcp", "tls"}}
		err := validateNodeShape(n, true)
		if (err == nil) != (port == 0) {
			t.Fatalf("port %d: %v", port, err)
		}
	}
}

func TestSIPStatelessNonceAndReplay(t *testing.T) {
	transport := &sipTransport{}
	now := time.Now().Truncate(time.Second)
	const source = "192.0.2.1:5060"
	var nonce string
	for range 10000 {
		nonce = transport.issueNonce("test", "SHA-256", source, now)
	}

	if len(transport.usedNonces) != 0 {
		t.Fatal("unauthenticated challenges allocated replay entries")
	}

	info, ok := transport.verifyNonce(nonce, "test", "SHA-256", source, now)
	if !ok {
		t.Fatal("valid nonce rejected after challenge flood")
	}

	for _, tc := range []struct {
		value, realm, algorithm, source string
		when                            time.Time
	}{
		{nonce, "other", "SHA-256", source, now},
		{nonce, "test", "MD5", source, now},
		{nonce, "test", "SHA-256", "192.0.2.2:5060", now},
		{nonce, "test", "SHA-256", source, now.Add(time.Minute)},
		{"!" + nonce[1:], "test", "SHA-256", source, now},
	} {
		if _, ok := transport.verifyNonce(tc.value, tc.realm, tc.algorithm, tc.source, tc.when); ok {
			t.Fatalf("invalid nonce accepted: %+v", tc)
		}
	}

	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if transport.consumeNonce("alice", nonce, info.expires, now) {
				accepted.Add(1)
			}
		}()
	}

	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("concurrent nonce replay accepted %d times", accepted.Load())
	}
}

func TestSDPAddressValidation(t *testing.T) {
	for _, tc := range []struct {
		family, address string
		valid           bool
	}{
		{"IP4", "127.0.0.1", true}, {"IP6", "::1", true},
		{"IP4", "192.168.1.2", true}, {"IP4", "example.invalid", false},
		{"IP4", "::1", false}, {"IP6", "127.0.0.1", false},
		{"IP4", "224.0.0.1", false}, {"IP4", "255.255.255.255", false},
		{"IP4", "0.0.0.0", false},
	} {
		_, err := parseSDPAddress("IN", tc.family, tc.address)
		if (err == nil) != tc.valid {
			t.Errorf("%s %s: %v", tc.family, tc.address, err)
		}
	}

	for _, tc := range []struct {
		destination, signaling string
		allowed                bool
	}{
		{"127.0.0.1", "198.51.100.1", false},
		{"192.168.1.2", "198.51.100.1", false},
		{"169.254.169.254", "198.51.100.1", false},
		{"127.0.0.1", "127.0.0.1", true},
		{"192.168.1.2", "192.168.1.3", true},
		{"203.0.113.1", "198.51.100.1", true},
	} {
		if got := mediaDestinationAllowed(net.ParseIP(tc.destination), net.ParseIP(tc.signaling)); got != tc.allowed {
			t.Errorf("%s from %s: allowed=%t", tc.destination, tc.signaling, got)
		}
	}

	for _, attr := range []string{"a=rtcp:0", "a=rtcp:5001 IN IP4 ::1", "a=rtcp:5001 IN IP4 224.0.0.1"} {
		media := &sipMedia{Codecs: []sipCodec{codecPCMU}}
		if err := media.RemoteSDP([]byte(reviewSDP(5000) + attr + "\r\n")); err == nil {
			t.Errorf("invalid RTCP attribute accepted: %s", attr)
		}
	}
}

func reviewSDP(port int) string {
	return fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=test\r\n"+
		"c=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\n", port)
}

func TestRestrictedRTPWaitsForReceivedMedia(t *testing.T) {
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}

	defer peer.Close()
	media, err := newSIPMedia(&Node{RTP: &RTPConfig{Bind: "127.0.0.1"}}, "198.51.100.1:5060")
	if err != nil {
		t.Fatal(err)
	}

	defer media.Close()
	if err := media.RemoteSDP([]byte(reviewSDP(peer.LocalAddr().(*net.UDPAddr).Port))); err != nil {
		t.Fatal(err)
	}

	packet := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1}, Payload: make([]byte, 160)}
	if err := media.WriteRTP(packet); err != nil {
		t.Fatal(err)
	}

	peer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	buf := make([]byte, 1500)
	if _, _, err := peer.ReadFromUDP(buf); err == nil {
		t.Fatal("public signaling caused unsolicited loopback RTP")
	}

	wire, _ := packet.Marshal()
	peer.WriteToUDP(wire, &media.Laddr)
	media.rtp.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := media.ReadRTP(buf, &rtp.Packet{}); err != nil {
		t.Fatal(err)
	}

	if err := media.WriteRTP(packet); err != nil {
		t.Fatal(err)
	}

	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peer.ReadFromUDP(buf); err != nil {
		t.Fatal("received RTP did not open the NAT return path", err)
	}

	media.remoteRTCP = peer.LocalAddr().(*net.UDPAddr)
	if err := media.WriteRTCP(&rtcp.ReceiverReport{SSRC: 1}); err != nil {
		t.Fatal(err)
	}

	peer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := peer.ReadFromUDP(buf); err == nil {
		t.Fatal("RTP validation authorized an unrelated RTCP destination")
	}
}

func TestSIPInfoDoesNotBlockRTP(t *testing.T) {
	c, err := fixtureConfig(nil, "info.hcl")
	if err != nil {
		t.Fatal(err)
	}

	e := NewEngine(c)
	defer e.Close()
	node := &Node{RTP: &RTPConfig{Bind: "127.0.0.1", Keepalive: "0s"}, DTMF: &DTMFConfig{Send: "sip_info"}}
	media, err := newSIPMedia(node, "127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}

	peer, err := newSIPMedia(node, "127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}

	defer peer.Close()
	if err := media.RemoteSDP(peer.LocalSDP()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started, finished := make(chan struct{}), make(chan struct{})
	transport := &sipTransport{engine: e, legs: make(map[string]*channelLeg)}
	leg := transport.makeLeg(e, ctx, node, media, "test", func(context.Context) error { return nil },
		func(ctx context.Context, req *sip.Request) (*sip.Response, error) {
			close(started)
			<-ctx.Done()
			close(finished)

			return nil, ctx.Err()
		}, sip.Uri{Scheme: "sip", Host: "127.0.0.1"}, ctx, false)
	defer leg.Close()
	if err := leg.Write(ctx, Frame{Digit: '5'}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("INFO delivery never started")
	}

	if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: make([]byte, 160)}); err != nil {
		t.Fatal(err)
	}

	peer.rtp.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := peer.rtp.ReadFromUDP(make([]byte, 1500)); err != nil {
		t.Fatal("RTP stalled behind INFO", err)
	}

	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("INFO was not bounded independently of the call lifetime")
	}
}

func TestExecEnvironmentIsolation(t *testing.T) {
	t.Setenv("SIPPYCUP_TEST_SECRET", "secret")
	t.Setenv("SIPPYCUP_TEST_EXPLICIT", "inherited")
	n := &Node{PassEnvironment: []string{"SIPPYCUP_TEST_EXPLICIT"}, Environment: map[string]string{"CUSTOM": "value"}}
	env := strings.Join(execEnvironment(n), "\n")
	if strings.Contains(env, "SIPPYCUP_TEST_SECRET=") ||
		!strings.Contains(env, "SIPPYCUP_TEST_EXPLICIT=inherited") || !strings.Contains(env, "CUSTOM=value") {
		t.Fatal("child environment did not honor isolation and explicit configuration")
	}
}

func TestSIPReplayCacheIsolation(t *testing.T) {
	transport := &sipTransport{}
	now := time.Now()
	for i := range 4096 {
		if !transport.consumeNonce("alice", fmt.Sprint(i), now.Add(time.Minute), now) {
			t.Fatal("replay cache filled prematurely")
		}
	}

	if transport.consumeNonce("alice", "overflow", now.Add(time.Minute), now) {
		t.Fatal("replay cache exceeded its bound")
	}

	if !transport.consumeNonce("bob", "new", now.Add(time.Minute), now) {
		t.Fatal("one account exhausted another account's authentication capacity")
	}

	if !transport.consumeNonce("alice", "fresh", now.Add(2*time.Minute), now.Add(time.Minute)) {
		t.Fatal("expired replay entries were not removed")
	}
}

func TestLoopbackICEIsOptIn(t *testing.T) {
	c, err := fixtureConfig(nil, "ice.hcl")
	if err != nil {
		t.Fatal(err)
	}

	e := NewEngine(c)
	defer e.Close()
	for _, tc := range []struct {
		name string
		ice  *ICEConfig
		want bool
	}{
		{"default", nil, false},
		{"explicit", &ICEConfig{IncludeLoopback: true}, true},
		{"bound", &ICEConfig{Bind: "127.0.0.1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			peer, err := e.newPeer(ctx, &Node{Type: "webrtc_http", ICE: tc.ice})
			if err != nil {
				t.Fatal(err)
			}

			defer peer.leg.Close()
			offer, err := peer.pc.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}

			body, err := gatherSDP(ctx, peer.pc, offer)
			if err != nil {
				t.Fatal(err)
			}

			found := false
			for _, line := range strings.Split(body, "\n") {
				fields := strings.Fields(line)
				if strings.HasPrefix(line, "a=candidate:") && len(fields) > 4 {
					found = found || net.ParseIP(fields[4]).IsLoopback()
				}
			}

			if found != tc.want {
				t.Fatalf("loopback candidate present=%t, want %t", found, tc.want)
			}
		})
	}
}
