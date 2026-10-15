package libsippycup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/turn/v5"
	"github.com/ronsor/sippycup/audiosocket"
)

func TestAudioSocketSIPCredentialPassthrough(t *testing.T) {
	audio, sipAddr := freeAddress(t), freeAddress(t)
	text := fmt.Sprintf(`credential "user" {
 username="alice"
 password="secret"
}

credential_set "users" { credentials=[credential.user] }
source "sip" {
 type="sip"
 bind=%q
 auth {
  mode="verify"
  credential_set=credential_set.users
 }
}
source "audio" {
 type="audiosocket"
 bind=%q
 extended=true
 auth {
  mode="passthrough"
  sources=["basic_auth"]
 }
}
endpoint "upstream" {
 type="sip"
 bind="127.0.0.1:0"
 uri="sip:%s"
 auth { credential=request.auth.credential }
}
route "gateway" {
 match { source=source.audio }
 connect {
  endpoint=endpoint.upstream
  destination=request.identity.destination
 }
}
`, sipAddr, audio, sipAddr) + echoEndpoint
	testEngine(t, text)
	for _, password := range []string{"secret", "wrong"} {
		t.Run(password, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := audiosocket.Dial(ctx, "tcp://"+audio, audiosocket.WithExtended(true))
			if err != nil {
				t.Fatal(err)
			}

			defer c.Close()
			stop := context.AfterFunc(ctx, func() {
				c.Close()
			})
			defer stop()
			identity, _ := json.Marshal(map[string]any{
				"basic_auth":  map[string]string{"username": "alice", "password": password},
				"destination": "echo",
				"uuid":        "550e8400-e29b-41d4-a716-446655440000",
			})
			if err = c.WriteIdentity(audiosocket.Identity{JSON: identity}); err != nil {
				t.Fatal(err)
			}

			pcm := make([]byte, 320)
			if err = c.WriteAudio(8000, pcm); err != nil {
				t.Fatal(err)
			}

			p, err := c.ReadPacket()
			if err != nil {
				t.Fatal(err)
			}

			if password == "secret" {
				if p.Kind != 0x10 || len(p.Payload) != 320 {
					t.Fatal("gateway did not relay audio")
				}
			} else if p.Kind != audiosocket.KindError {
				t.Fatal("invalid upstream password accepted")
			}
		})
	}
}

func TestSIPAndWebRTCDTMFTranslation(t *testing.T) {
	for _, protocol := range []string{"sip", "webrtc_http"} {
		t.Run(protocol, func(t *testing.T) {
			addr := freeAddress(t)
			source := fmt.Sprintf("source \"in\" {\n type=%q\n bind=%q\n codecs=[\"pcmu\"]\n", protocol, addr)
			if protocol == "webrtc_http" {
				source += "path=\"/calls\"\nice { bind=\"127.0.0.1\" }\n"
			}

			source += "}\n"
			text := strings.Replace(
				echoEndpoint,
				"io = \"pcm\"",
				"io = \"pcm\"\n dtmf {\n receive=[\"inband\"]\n send=\"inband\"\n }",
				1,
			)
			e := testEngine(t, source+text)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			node := &Node{
				Name:   "client",
				Type:   protocol,
				Bind:   "127.0.0.1:0",
				URI:    "sip:" + addr,
				URL:    "http://" + addr + "/calls",
				Codecs: []string{"pcmu"},
				ICE:    &ICEConfig{Bind: "127.0.0.1"},
			}
			var leg Leg
			var err error
			if protocol == "sip" {
				leg, err = e.sipEndpoint(ctx, ctx, node, "echo", nil)
			} else {
				leg, err = e.webEndpoint(ctx, ctx, node, nil)
			}

			if err != nil {
				t.Fatal(err)
			}

			defer leg.Close()
			// Establish the incoming RTP audio track before sending telephone events.
			silence := encode(make([]int16, 160), leg.Format())
			if err = leg.Write(ctx, Frame{Format: leg.Format(), Data: silence}); err != nil {
				t.Fatal(err)
			}

			if err = leg.Write(ctx, Frame{Digit: '#'}); err != nil {
				t.Fatal(err)
			}

			for {
				f, err := leg.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}

				if f.Digit == '#' {
					break
				}
			}
		})
	}
}

func testCertificate(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}

	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	priv := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certPath, cert, 0600); err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(keyPath, priv, 0600); err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(cert)

	return certPath, keyPath, pool
}

func TestAudioSocketTLS(t *testing.T) {
	cert, key, pool := testCertificate(t)
	address := freeAddress(t)
	text := fmt.Sprintf(`source "secure" {
 type="audiosocket_ws"
 bind=%q
 path="/audio"
 tls {
 certificate_file=%q
 private_key_file=%q
 }
}
`, address, cert, key) + echoEndpoint
	testEngine(t, text)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := audiosocket.Dial(
		ctx,
		"wss://"+address+"/audio",
		audiosocket.WithTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}),
	)
	if err != nil {
		t.Fatal(err)
	}

	defer c.Close()
	if err = c.WriteIdentity(audiosocket.Identity{}); err != nil {
		t.Fatal(err)
	}

	pcm := make([]byte, 320)
	if err = c.WriteAudio(8000, pcm); err != nil {
		t.Fatal(err)
	}

	packet, err := c.ReadPacket()
	if err != nil || !bytes.Equal(packet.Payload, pcm) {
		t.Fatal(err)
	}
}

func TestWebRTCTURNRelay(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	authenticate := func(ra *turn.RequestAttributes) (string, []byte, bool) {
		return "relay", turn.GenerateAuthKey("relay", "test", "secret"), ra.Username == "relay" && ra.Realm == "test"
	}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm:       "test",
		AuthHandler: authenticate,
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: conn,
				RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
					RelayAddress: net.ParseIP("127.0.0.1"),
					Address:      "127.0.0.1",
				},
			},
		},
	})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}

	defer server.Close()
	address := freeAddress(t)
	turnURL := "turn:" + conn.LocalAddr().String() + "?transport=udp"
	text := fmt.Sprintf(`credential "relay" {
 username="relay"
 password="secret"
}
source "web" {
 type="webrtc_http"
 bind=%q
 path="/calls"
 codecs=["pcmu"]
 ice {
  bind="127.0.0.1"
  policy="relay"
  server "local" {
   urls=[%q]
   credential=credential.relay
  }
 }
}
`, address, turnURL) + echoEndpoint
	e := testEngine(t, text)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	n := &Node{
		Name:   "client",
		Type:   "webrtc_http",
		URL:    "http://" + address + "/calls",
		Codecs: []string{"pcmu"},
		ICE: &ICEConfig{
			Bind:    "127.0.0.1",
			Policy:  "relay",
			Servers: []*ICEServer{{URLs: []string{turnURL}, Credential: "relay"}},
		},
	}
	leg, err := e.webEndpoint(ctx, ctx, n, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer leg.Close()
	data := encode(tone('5', 8000)[:160], leg.Format())
	if err = leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
		t.Fatal(err)
	}

	f, err := leg.Read(ctx)
	if err != nil || !bytes.Equal(f.Data, data) {
		t.Fatal("TURN relay audio failed", err)
	}
}

func TestSIPSetupTimeoutAndCancel(t *testing.T) {
	for _, ringing := range []bool{false, true} {
		t.Run(fmt.Sprintf("ringing=%t", ringing), func(t *testing.T) {
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
			server, _ := sipgo.NewServer(ua)
			canceled := make(chan struct{}, 1)
			server.OnInvite(func(r *sip.Request, tx sip.ServerTransaction) {
				if ringing {
					tx.OnCancel(func(*sip.Request) {
						select {
						case canceled <- struct{}{}:
						default:
						}
					})
					_ = tx.Respond(sip.NewResponseFromRequest(r, 180, "Ringing", nil))
				}

				select {
				case <-tx.Done():
				case <-tx.Acks():
				}
			})
			go func() {
				_ = server.ServeUDP(pc)
			}()
			c, err := fixtureConfig(nil, "timeout.hcl")
			if err != nil {
				t.Fatal(err)
			}

			e := NewEngine(c)
			defer e.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			start := time.Now()
			node := &Node{
				Name: "out",
				Type: "sip",
				URI:  "sip:" + pc.LocalAddr().String(),
				Bind: "127.0.0.1:0",
			}
			if ringing {
				node.OutboundProxy = node.URI
				node.URI = "sip:provider.invalid"
			}

			leg, err := e.sipEndpoint(ctx, ctx, node, "test", nil)
			if err == nil {
				leg.Close()
				t.Fatal("unanswered call succeeded")
			}

			if time.Since(start) > 1500*time.Millisecond {
				t.Fatal("setup timeout was not enforced")
			}

			if ringing {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("ringing call was not canceled")
				}
			}
		})
	}
}

func TestSIPTCPAndTLS(t *testing.T) {
	cert, key, _ := testCertificate(t)
	for _, transport := range []string{"tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			addr := freeAddress(t)
			tlsBlock := ""
			if transport == "tls" {
				tlsBlock = fmt.Sprintf("tls {\n certificate_file=%q\n private_key_file=%q\n}\n", cert, key)
			}

			e := testEngine(
				t,
				fmt.Sprintf(
					"source \"in\" {\n type=\"sip\"\n bind=%q\n transports=[%q]\n%s}\n",
					addr,
					transport,
					tlsBlock,
				)+echoEndpoint,
			)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			node := &Node{
				Name:   "out",
				Type:   "sip",
				Bind:   "127.0.0.1:0",
				URI:    "sip:" + addr + ";transport=" + transport,
				Codecs: []string{"pcma"},
			}
			if transport == "tls" {
				node.TLS = &TLSConfig{CAFile: cert}
				node.URI = "sips:" + addr
			}

			leg, err := e.sipEndpoint(ctx, ctx, node, "echo", nil)
			if err != nil {
				t.Fatal(err)
			}

			defer leg.Close()
			data := encode(tone('3', 8000)[:160], leg.Format())
			if err = leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
				t.Fatal(err)
			}

			f, err := leg.Read(ctx)
			if err != nil || !bytes.Equal(data, f.Data) {
				t.Fatal("SIP stream audio failed", err)
			}
		})
	}
}

func TestSIPDefaultBindOutboundProxy(t *testing.T) {
	addr := freeAddress(t)
	e := testEngine(t, fmt.Sprintf("source \"in\" {\n type=\"sip\"\n bind=%q\n}\n", addr)+echoEndpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	node := &Node{
		Name:          "out",
		Type:          "sip",
		URI:           "sip:provider.invalid",
		OutboundProxy: "sip:" + addr + ";transport=udp",
	}
	leg, err := e.sipEndpoint(ctx, ctx, node, "echo", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer leg.Close()
	data := encode(tone('3', 8000)[:160], leg.Format())
	if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
		t.Fatal(err)
	}

	f, err := leg.Read(ctx)
	if err != nil || !bytes.Equal(data, f.Data) {
		t.Fatal("default-bind SIP media failed", err)
	}
}
