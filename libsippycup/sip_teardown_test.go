package libsippycup

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/ronsor/sippycup/audiosocket"
)

func TestAudioSocketSIPTeardown(t *testing.T) {
	for _, protocol := range []string{"audiosocket", "audiosocket_ws"} {
		for _, outcome := range []string{"bye", "bye_after_reload", "rejected", "connection_refused", "dial_timeout"} {
			t.Run(protocol+"/"+outcome, func(t *testing.T) {
				audioAddress, sipAddress := freeAddress(t), freeAddress(t)
				var upstream *Engine
				switch outcome {
				case "bye", "bye_after_reload", "rejected":
					routes := echoEndpoint
					if outcome == "rejected" {
						routes = "route \"reject\" {\n reject { code=486 }\n}\n"
					}
					config := fmt.Sprintf(`source "sip" {
 type="sip"
 bind=%q
}
`, sipAddress)
					upstream = testEngine(t, config+routes)
				case "dial_timeout":
					pc, err := net.ListenPacket("udp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer pc.Close()
					sipAddress = pc.LocalAddr().String()
				}

				transport := "udp"
				if outcome == "connection_refused" {
					transport = "tcp"
				}

				path, audioURL := "", "tcp://"+audioAddress
				if protocol == "audiosocket_ws" {
					path, audioURL = "path=\"/audio\"", "ws://"+audioAddress+"/audio"
				}

				gateway := testEngine(t, fmt.Sprintf(`source "audio" {
 type=%q
 bind=%q
 %s
}
endpoint "sip" {
 type="sip"
 bind="127.0.0.1:0"
 uri="sip:%s"
 transport=%q
 timeouts { dial="300ms" }
}
route "bridge" {
 connect {
  endpoint=endpoint.sip
  destination="echo"
 }
}
`, protocol, audioAddress, path, sipAddress, transport))
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				c, err := audiosocket.Dial(ctx, audioURL, audiosocket.WithContext(ctx))
				if err != nil {
					t.Fatal(err)
				}

				defer c.Close()
				if err := c.WriteIdentity(audiosocket.Identity{UUID: [16]byte{1}}); err != nil {
					t.Fatal(err)
				}

				if outcome == "bye" || outcome == "bye_after_reload" {
					if err := c.WriteAudio(8000, make([]byte, 320)); err != nil {
						t.Fatal(err)
					}

					p, err := c.ReadPacket()
					if err != nil || p.Kind != 0x10 {
						t.Fatalf("call did not establish: %v, kind=%x", err, p.Kind)
					}

					if outcome == "bye_after_reload" {
						next := fmt.Sprintf("source \"sip\" {\n type=\"sip\"\n bind=%q\n}\n", sipAddress)
						reloadFixture(t, upstream, next+"route \"deny\" {\n reject { code=403 }\n}\n")
						if err := c.WriteAudio(8000, make([]byte, 320)); err != nil {
							t.Fatal(err)
						}

						p, err := c.ReadPacket()
						if err != nil || p.Kind != 0x10 {
							t.Fatalf("audio stopped after reload: %v, kind=%x", err, p.Kind)
						}
					}

					sipServer := upstream.getSIP("sip")
					sipServer.mu.Lock()
					var remote Leg
					for _, leg := range sipServer.legs {
						remote = leg
					}

					sipServer.mu.Unlock()
					if remote == nil {
						t.Fatal("missing established SIP leg")
					}
					// Initiate BYE from the remote SIP side of the bridge.
					remote.Close()
				} else {
					p, err := c.ReadPacket()
					if err != nil || p.Kind != audiosocket.KindError {
						t.Fatalf("missing AudioSocket setup error: %v, kind=%x", err, p.Kind)
					}
				}

				if _, err := c.ReadPacket(); err == nil {
					t.Fatal("AudioSocket remained open after SIP ended")
				}

				if ctx.Err() != nil {
					t.Fatal("AudioSocket closed only because the test deadline expired")
				}

				for {
					gateway.mu.Lock()
					u := *gateway.use("global")
					gateway.mu.Unlock()
					if u.calls == 0 && u.pending == 0 {
						break
					}

					select {
					case <-ctx.Done():
						t.Fatal("call or pending capacity was not released")
					case <-time.After(5 * time.Millisecond):
					}
				}
			})
		}
	}
}
