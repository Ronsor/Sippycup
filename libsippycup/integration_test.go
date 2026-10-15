package libsippycup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ronsor/sippycup/audiosocket"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--sippycup-test-echo" {
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			os.Exit(1)
		}

		os.Exit(0)
	}

	os.Exit(m.Run())
}

func freeAddress(t *testing.T) string {
	t.Helper()
	// Check both protocols: a TCP-only probe can choose an occupied RTP port.
	for attempt := 0; attempt < 100; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		addr := l.Addr().String()
		udp, err := net.ListenPacket("udp", addr)
		if err != nil {
			l.Close()
			continue
		}

		udp.Close()
		l.Close()

		return addr
	}

	t.Fatal("cannot find a loopback port available for TCP and UDP")
	return ""
}

func testEngine(t *testing.T, text string) *Engine {
	t.Helper()
	c, err := fixtureConfig([]byte(text), t.Name()+".hcl")
	if err != nil {
		t.Fatal(err)
	}

	for _, endpoint := range c.Endpoints {
		if len(endpoint.Command) == 1 && endpoint.Command[0] == "@test-echo" {
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}

			endpoint.Command = []string{binary, "--sippycup-test-echo"}
		}
	}

	e := NewEngine(c)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			e.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("engine did not shut down")
		}
	})

	return e
}

const echoEndpoint = `endpoint "echo" {
 type = "exec"
 command = ["@test-echo"]
 io = "pcm"
 transcode {
  codec = "pcm_s16le"
  sample_rate = 8000
 }
}
route "echo" {
 connect { endpoint = endpoint.echo }
}
`

func TestOutboundAudioSocket(t *testing.T) {
	for _, protocol := range []string{"audiosocket", "audiosocket_ws"} {
		t.Run(protocol, func(t *testing.T) {
			upstream, ingress := freeAddress(t), freeAddress(t)
			path, target, method := "", fmt.Sprintf("address=%q", upstream), "basic_auth"
			if protocol == "audiosocket_ws" {
				path = "path=\"/audio\""
				target = fmt.Sprintf("url=%q", "ws://"+upstream+"/audio")
				method = "http_basic"
			}

			credentials := `credential "user" {
 username="alice"
 password="secret"
}
credential_set "users" { credentials=[credential.user] }
`
			testEngine(t, credentials+fmt.Sprintf(`source "upstream" {
 type=%q
 bind=%q
 %s
 extended=true
 auth {
  mode="verify"
  sources=[%q]
  credential_set=credential_set.users
 }
}
`, protocol, upstream, path, method)+strings.Replace(echoEndpoint, `route "echo" {`, `route "echo" {
 match {
  capture "uuid" {
   value = request.identity.uuid
   pattern = "^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
  }
  when = request.identity.destination == "echo"
 }
`, 1))
			testEngine(t, credentials+fmt.Sprintf(`source "in" {
 type="audiosocket"
 bind=%q
}
endpoint "out" {
 type=%q
 %s
 extended=true
 auth { credential=credential.user }
}
route "forward" {
 connect {
  endpoint=endpoint.out
  identity={ destination="echo", uuid="must-be-replaced" }
 }
}
`, ingress, protocol, target))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := audiosocket.Dial(ctx, "tcp://"+ingress, audiosocket.WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}

			defer c.Close()
			if err = c.WriteIdentity(audiosocket.Identity{UUID: [16]byte{1}}); err != nil {
				t.Fatal(err)
			}

			data := encode(tone('5', 8000)[:160], Format{"pcm_s16le", 8000, 1})
			if err = c.WriteAudio(8000, data); err != nil {
				t.Fatal(err)
			}

			p, err := c.ReadPacket()
			if err != nil || p.Kind != 0x10 || !bytes.Equal(p.Payload, data) {
				t.Fatalf("outbound AudioSocket echo: kind=%x err=%v", p.Kind, err)
			}
		})
	}
}

func TestAudioSocketExecEcho(t *testing.T) {
	address := freeAddress(t)
	e := testEngine(
		t,
		fmt.Sprintf("source \"in\" {\n type=\"audiosocket\"\n bind=%q\n extended=true\n}\n", address)+echoEndpoint,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := audiosocket.Dial(ctx, "tcp://"+address, audiosocket.WithExtended(true))
	if err != nil {
		t.Fatal(err)
	}

	defer c.Close()
	stop := context.AfterFunc(ctx, func() {
		c.Close()
	})
	defer stop()
	if err = c.WriteIdentity(audiosocket.Identity{
		JSON: json.RawMessage(`{"uuid":"550e8400-e29b-41d4-a716-446655440000","label":"echo"}`),
	}); err != nil {
		t.Fatal(err)
	}

	data := encode(tone('8', 8000)[:160], Format{"pcm_s16le", 8000, 1})
	if err = c.WriteAudio(8000, data); err != nil {
		t.Fatal(err)
	}

	p, err := c.ReadPacket()
	if err != nil || p.Kind != 0x10 || !bytes.Equal(p.Payload, data) {
		t.Fatalf("echo: kind=%x length=%d err=%v", p.Kind, len(p.Payload), err)
	}

	c.Close()
	_ = e
}

func TestHTTPSharedPathsAndBasicAuth(t *testing.T) {
	address := freeAddress(t)
	text := fmt.Sprintf(`credential "user" {
 username="alice"
 password="secret"
}
credential_set "users" { credentials=[credential.user] }
source "one" {
 type="audiosocket_ws"
 bind=%q
 path="/one"
 auth {
  mode="verify"
  sources=["http_basic"]
  credential_set=credential_set.users
 }
}
source "two" {
 type="audiosocket_ws"
 bind=%q
 path="/two"
}
`, address, address) + echoEndpoint
	e := testEngine(t, text)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c, err := audiosocket.Dial(ctx, "ws://"+address+"/one"); err == nil {
		c.Close()
		t.Fatal("missing auth accepted")
	}

	for _, path := range []string{"/one", "/two"} {
		headers := http.Header{}
		if path == "/one" {
			request, _ := http.NewRequest("GET", "http://example.com", nil)
			request.SetBasicAuth("alice", "secret")
			headers = request.Header
		}

		c, err := audiosocket.Dial(ctx, "ws://"+address+path, audiosocket.WithHTTPHeader(headers))
		if err != nil {
			t.Fatal(err)
		}

		if err = c.WriteIdentity(audiosocket.Identity{}); err != nil {
			t.Fatal(err)
		}

		data := make([]byte, 320)
		if err = c.WriteAudio(8000, data); err != nil {
			t.Fatal(err)
		}

		p, err := c.ReadPacket()
		if err != nil || !bytes.Equal(p.Payload, data) {
			t.Fatal(err)
		}

		c.Close()
	}

	_ = e
}

func TestSIPG711AndDigestLoopback(t *testing.T) {
	address := freeAddress(t)
	text := fmt.Sprintf(`credential "phone" {
 username="alice"
 password="secret"
}
credential_set "phones" { credentials=[credential.phone] }
source "sip" {
 type="sip"
 bind=%q
 transports=["udp"]
 codecs=["pcmu"]
 auth {
  mode="verify"
  realm="test"
  credential_set=credential_set.phones
 }
}
`, address) + echoEndpoint
	e := testEngine(t, text)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	n := &Node{
		Name:   "out",
		Type:   "sip",
		Bind:   "127.0.0.1:0",
		URI:    "sip:" + address,
		Codecs: []string{"pcmu"},
	}
	leg, err := e.sipEndpoint(ctx, ctx, n, "echo", e.config.credential("phone"))
	if err != nil {
		t.Fatal(err)
	}

	defer leg.Close()
	format := leg.Format()
	if format.Codec != "pcmu" {
		t.Fatal(format)
	}

	data := encode(tone('5', 8000)[:160], format)
	if err = leg.Write(ctx, Frame{Format: format, Data: data}); err != nil {
		t.Fatal(err)
	}

	f, err := leg.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(f.Data, data) {
		t.Fatalf("SIP echo mismatch %d/%d", len(f.Data), len(data))
	}
}

func TestWebRTCHTTPBidirectional(t *testing.T) {
	address := freeAddress(t)
	text := fmt.Sprintf(`source "web" {
 type="webrtc_http"
 bind=%q
 path="/calls"
 codecs=["pcmu"]
 ice {
  bind="127.0.0.1"
  trickle=true
 }
}
`, address) + echoEndpoint
	e := testEngine(t, text)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	n := &Node{
		Name:   "client",
		Type:   "webrtc_http",
		URL:    "http://" + address + "/calls",
		Codecs: []string{"pcmu"},
		ICE:    &ICEConfig{Bind: "127.0.0.1"},
	}
	leg, err := e.webEndpoint(ctx, ctx, n, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer leg.Close()
	format := leg.Format()
	data := encode(tone('5', 8000)[:160], format)
	if err = leg.Write(ctx, Frame{Format: format, Data: data}); err != nil {
		t.Fatal(err)
	}

	f, err := leg.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(f.Data, data) {
		t.Fatalf("WebRTC echo mismatch %d/%d", len(f.Data), len(data))
	}
}

func TestLimitsReleaseAfterFailure(t *testing.T) {
	c, err := fixtureConfig([]byte(`runtime {
 limits {
  max_calls=1
  max_pending_calls=1
 }
}
`), "limits.hcl")
	if err != nil {
		t.Fatal(err)
	}

	e := NewEngine(c)
	defer e.Close()
	source := &Node{Name: "in"}
	endpoint := &Node{Name: "out"}
	activate, release, err := e.reserve(source, endpoint)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := e.reserve(source, endpoint); err == nil {
		t.Fatal("limit not enforced")
	}

	activate()
	release()
	release()
	_, release, err = e.reserve(source, endpoint)
	if err != nil {
		t.Fatal(err)
	}

	release()
}

func TestHTTPRejectOversizeOffer(t *testing.T) {
	address := freeAddress(t)
	e := testEngine(t, fmt.Sprintf(`source "web" {
 type="webrtc_http"
 bind=%q
 path="/calls"
 limits { max_http_body_bytes=32 }
}
`, address)+echoEndpoint)
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Post(
		"http://"+address+"/calls",
		"application/sdp",
		strings.NewReader(strings.Repeat("x", 64)),
	)
	if err != nil {
		t.Fatal(err)
	}

	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != 413 {
		t.Fatal(res.StatusCode)
	}

	_ = e
}

func TestFixedAudioSocketUUID(t *testing.T) {
	const fixed = "00112233-4455-6677-8899-AABBCCDDEEFF"
	for _, extended := range []bool{false, true} {
		t.Run(fmt.Sprint(extended), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}

			defer listener.Close()
			node := &Node{Type: "audiosocket", Name: "out", Address: listener.Addr().String(),
				UUID: fixed, Extended: extended}
			config := &Config{Endpoints: []*Node{node}}
			if err := config.Validate(); err != nil {
				t.Fatal(err)
			}

			engine := NewEngine(config)
			defer engine.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			leg, err := engine.audioEndpoint(ctx, ctx, node, &Request{ID: "call"}, nil,
				&Connect{}, nil, Format{"pcm_s16le", 8000, 1})
			if err != nil {
				t.Fatal(err)
			}

			defer leg.Close()
			conn, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}

			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			packet, err := audiosocket.Wrap(conn, audiosocket.WithExtended(extended)).ReadPacket()
			if err != nil {
				t.Fatal(err)
			}

			if packet.Kind != audiosocket.KindID {
				t.Fatalf("expected identity, got %x", packet.Kind)
			}

			if extended {
				var metadata map[string]any
				if err := json.Unmarshal(packet.Payload, &metadata); err != nil {
					t.Fatal(err)
				}

				if metadata["uuid"] != strings.ToLower(fixed) {
					t.Fatalf("wrong JSON UUID: %v", metadata)
				}
			} else if fmt.Sprintf("%x", packet.Payload) != "00112233445566778899aabbccddeeff" {
				t.Fatalf("wrong binary UUID: %x", packet.Payload)
			}
		})
	}
}
