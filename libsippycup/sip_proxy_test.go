package libsippycup

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/rtp"
)

func TestSIPOutboundProxy(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer pc.Close()
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}

	defer ua.Close()
	server, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}

	proxy := "sip:" + pc.LocalAddr().String()
	media, err := newSIPMedia(
		&Node{Codecs: []string{"pcmu", "pcma"}, RTP: &RTPConfig{Bind: "127.0.0.1"}},
		pc.LocalAddr().String(),
	)
	if err != nil {
		t.Fatal(err)
	}

	defer media.Close()
	// Exercise a remote answer that accepts both codecs and sends PCMA,
	// even though our caller's preferred outgoing codec is PCMU.
	media.offered = true
	events := make(chan sip.RequestMethod, 16)
	challenge := &digest.Challenge{
		Realm:     "proxy.test",
		Nonce:     "test-nonce",
		Algorithm: "SHA-256",
		QOP:       []string{"auth"},
	}
	authenticate := func(r *sip.Request, tx sip.ServerTransaction) bool {
		if r.Recipient.Host != "provider.invalid" {
			t.Errorf("proxy replaced request domain: %s", r.Recipient.Addr())
		}

		if h := r.Route(); h == nil || !h.Address.UriParams.Has("lr") || h.Address.Host != "127.0.0.1" {
			t.Errorf("missing preloaded loose route: %v", h)
		}

		h := r.GetHeader("Proxy-Authorization")
		if h == nil {
			res := sip.NewResponseFromRequest(r, 407, "Proxy Authentication Required", nil)
			res.AppendHeader(sip.NewHeader("Proxy-Authenticate", challenge.String()))
			_ = respondSIP(tx, res)

			return false
		}

		got, err := digest.ParseCredentials(h.Value())
		if err != nil {
			t.Error(err)

			return false
		}

		want, err := digest.Digest(
			challenge,
			digest.Options{
				Username: "alice",
				Password: "secret",
				Method:   string(r.Method),
				URI:      r.Recipient.Addr(),
				Cnonce:   got.Cnonce,
				Count:    got.Nc,
			},
		)
		if err != nil || got.URI != r.Recipient.Addr() || got.Response != want.Response {
			t.Errorf("invalid proxy digest for %s: %v", r.Method, err)
			_ = tx.Respond(sip.NewResponseFromRequest(r, 403, "Forbidden", nil))

			return false
		}

		return true
	}
	server.OnRegister(func(r *sip.Request, tx sip.ServerTransaction) {
		if !authenticate(r, tx) {
			return
		}

		_ = tx.Respond(sip.NewResponseFromRequest(r, 200, "OK", nil))
		events <- sip.REGISTER
	})
	server.OnInvite(func(r *sip.Request, tx sip.ServerTransaction) {
		if !authenticate(r, tx) {
			return
		}

		if r.Recipient.User != "1234" || r.From().Address.Host != "provider.invalid" {
			t.Error("destination or From domain changed")
		}

		if err := media.RemoteSDP(r.Body()); err != nil {
			t.Error(err)

			return
		}

		res := sip.NewResponseFromRequest(r, 200, "OK", media.LocalSDP())
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		res.AppendHeader(sip.NewHeader("Contact", "<sip:1234@contact.invalid>"))
		res.AppendHeader(sip.NewHeader("Record-Route", "<"+proxy+";lr>"))
		_ = tx.Respond(res)
		events <- sip.INVITE
	})
	checkDialog := func(r *sip.Request) {
		if r.Recipient.Host != "contact.invalid" || r.Route() == nil || r.Route().Address.Host != "127.0.0.1" {
			t.Errorf("%s lost remote target or proxy route", r.Method)
		}

		events <- r.Method
	}
	server.OnAck(func(r *sip.Request, _ sip.ServerTransaction) {
		checkDialog(r)
		packet := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 8, SSRC: 123, SequenceNumber: 1},
			Payload: encode(make([]int16, 160), Format{"pcma", 8000, 1}),
		}
		if err := media.WriteRTP(packet); err != nil {
			t.Error(err)
		}
	})
	server.OnInfo(func(r *sip.Request, tx sip.ServerTransaction) {
		checkDialog(r)
		_ = tx.Respond(sip.NewResponseFromRequest(r, 200, "OK", nil))
	})
	server.OnBye(func(r *sip.Request, tx sip.ServerTransaction) {
		checkDialog(r)
		_ = tx.Respond(sip.NewResponseFromRequest(r, 200, "OK", nil))
	})
	go func() {
		_ = server.ServeUDP(pc)
	}()
	e := testEngine(t, fmt.Sprintf(`credential "account" {
 username="alice"
 password="secret"
}
source "registered" {
 type="sip"
 bind="127.0.0.1:0"
 outbound_proxy=%q
 registration {
  registrar="sip:provider.invalid"
  aor="sip:alice@provider.invalid"
  credential=credential.account
 }
}
endpoint "provider" {
 type="sip"
 uri="sip:provider.invalid"
 source=source.registered
 auth { credential=credential.account }
 dtmf { send="sip_info" }
}
`, proxy))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	leg, err := e.sipEndpoint(ctx, ctx, e.config.endpoint("provider"), "1234", e.config.credential("account"))
	if err != nil {
		t.Fatal(err)
	}

	defer leg.Close()
	frame, err := leg.Read(ctx)
	if err != nil || frame.Format.Codec != "pcma" || len(frame.Data) != 160 {
		t.Fatalf("negotiated secondary codec was not received: format=%v err=%v", frame.Format, err)
	}

	if err := leg.Write(ctx, Frame{Digit: '5'}); err != nil {
		t.Fatal(err)
	}

	leg.Close()
	want := map[sip.RequestMethod]bool{
		sip.REGISTER: true,
		sip.INVITE:   true,
		sip.ACK:      true,
		sip.INFO:     true,
		sip.BYE:      true,
	}
	for len(want) > 0 {
		select {
		case method := <-events:
			delete(want, method)
		case <-ctx.Done():
			t.Fatalf("missing proxy traffic: %v", want)
		}
	}
	// Shut down registration while the test proxy is still running.
	e.Close()
}

func TestSIPProxyConfiguration(t *testing.T) {
	for _, tc := range []struct{ proxy, transport string }{
		{"sip:proxy.example", "udp"},
		{"sip:proxy.example;transport=tcp", "tcp"},
		{"sips:proxy.example", "tls"},
	} {
		c, err := fixtureConfig([]byte(fmt.Sprintf(`endpoint "out" {
 type="sip"
 uri="sip:provider.example"
 outbound_proxy=%q
}
`, tc.proxy)), "proxy.hcl")
		if err != nil {
			t.Fatal(err)
		}

		if got := sipNetwork(c.endpoint("out")); got != tc.transport {
			t.Errorf("%s: transport %s, want %s", tc.proxy, got, tc.transport)
		}
	}

	for _, proxy := range []string{
		"http://proxy.example",
		"sip:alice@proxy.example",
		"sip:proxy.example;transport=ws",
		"sip:proxy.example?Subject=test",
		"sip:proxy.example:99999",
	} {
		_, err := fixtureConfig([]byte(fmt.Sprintf(`endpoint "out" {
 type="sip"
 uri="sip:provider.example"
 outbound_proxy=%q
}
`, proxy)), "invalid-proxy.hcl")
		if err == nil {
			t.Errorf("accepted invalid proxy %q", proxy)
		}
	}
}
