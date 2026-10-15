package libsippycup

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"

	"github.com/ronsor/sippycup/audiosocket"
)

// earlySIPPeer echoes RTP before the test permits a final SIP response.
// Each media phase uses its own port, exercising the final SDP transition.
func earlySIPPeer(
	t *testing.T,
	status int,
	finalCodec string,
	mutate ...func(*sip.Response),
) (string, chan<- int, <-chan struct{}) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		pc.Close()
		t.Fatal(err)
	}

	server, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	final := make(chan int, 1)
	canceled := make(chan struct{}, 1)
	var workers sync.WaitGroup
	var media []*sipMedia
	for _, codec := range []string{"pcmu", finalCodec} {
		s, err := newSIPMedia(&Node{
			Codecs: []string{codec},
			RTP:    &RTPConfig{Bind: "127.0.0.1"},
		}, pc.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}

		media = append(media, s)
	}

	t.Cleanup(func() {
		cancel()
		pc.Close()
		ua.Close()
		for _, s := range media {
			s.Close()
		}

		workers.Wait()
	})

	server.OnInvite(func(r *sip.Request, tx sip.ServerTransaction) {
		workers.Add(1)
		defer workers.Done()
		tx.OnCancel(func(*sip.Request) {
			select {
			case canceled <- struct{}{}:
			default:
			}
		})
		for _, s := range media {
			if err := s.RemoteSDP(r.Body()); err != nil {
				t.Error(err)

				return
			}

			workers.Add(1)
			go func(s *sipMedia) {
				defer workers.Done()
				buf := make([]byte, 2048)
				for {
					var packet rtp.Packet
					if _, err := s.ReadRTP(buf, &packet); err != nil {
						return
					}

					if err := s.WriteRTP(&packet); err != nil {
						return
					}
				}
			}(s)
		}

		send := func(code int, body []byte) {
			res := sip.NewResponseFromRequest(r, code, "Test", body)
			res.To().Params.Add("tag", "early-peer")
			res.AppendHeader(sip.NewHeader("Contact", "<sip:peer@"+pc.LocalAddr().String()+">"))
			if len(body) > 0 {
				res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			}
			for _, change := range mutate {
				change(res)
			}

			_ = respondSIP(tx, res)
		}

		// Ringing without SDP must not create a media leg.
		send(180, nil)
		send(status, media[0].LocalSDP())
		// Retransmitted SDP must not restart readers or bridge goroutines.
		send(status, media[0].LocalSDP())
		select {
		case code := <-final:
			if code == 200 {
				send(code, media[1].LocalSDP())
			} else {
				send(code, nil)
			}
		case <-tx.Done():
		case <-ctx.Done():
		}
	})
	server.OnCancel(func(r *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(r, 200, "OK", nil))
		select {
		case canceled <- struct{}{}:
		default:
		}
	})
	server.OnBye(func(r *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(r, 200, "OK", nil))
	})
	server.OnAck(func(*sip.Request, sip.ServerTransaction) {})
	workers.Add(1)
	go func() {
		defer workers.Done()
		_ = server.ServeUDP(pc)
	}()

	return pc.LocalAddr().String(), final, canceled
}

func TestAudioSocketSIPEarlyMedia(t *testing.T) {
	for _, protocol := range []string{"audiosocket", "audiosocket_ws"} {
		for _, outcome := range []string{
			"answer", "answer_same_sdp", "answer_without_sdp", "reject", "timeout", "disconnect",
		} {
			t.Run(protocol+"/"+outcome, func(t *testing.T) {
				var earlySDP []byte
				peer, final, canceled := earlySIPPeer(t, 183, "pcma", func(res *sip.Response) {
					if res.StatusCode == 183 {
						earlySDP = append([]byte(nil), res.Body()...)
					}
					if res.StatusCode == 200 {
						switch outcome {
						case "answer_same_sdp":
							res.SetBody(earlySDP)
						case "answer_without_sdp":
							res.SetBody(nil)
							res.RemoveHeader("Content-Type")
						}
					}
				})
				address := freeAddress(t)
				path, url := "", "tcp://"+address
				if protocol == "audiosocket_ws" {
					path, url = "path=\"/audio\"", "ws://"+address+"/audio"
				}

				dialTimeout := "4s"
				if outcome == "timeout" {
					dialTimeout = "500ms"
				}

				gateway := testEngine(t, fmt.Sprintf(`source "audio" {
 type = %q
 bind = %q
 %s
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
 codecs = ["pcmu", "pcma"]
 timeouts { dial = %q }
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, protocol, address, path, peer, dialTimeout))
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				conn, err := audiosocket.Dial(ctx, url, audiosocket.WithContext(ctx))
				if err != nil {
					t.Fatal(err)
				}

				defer conn.Close()
				if err := conn.WriteIdentity(audiosocket.Identity{UUID: [16]byte{1}}); err != nil {
					t.Fatal(err)
				}

				// A non-silent payload proves media traverses both directions
				// before any final answer has been sent.
				pcm := make([]int16, 160)
				for i := range pcm {
					pcm[i] = 1000
				}

				payload := encode(pcm, Format{"pcm_s16le", 8000, 1})
				if err := conn.WriteAudio(8000, payload); err != nil {
					t.Fatal(err)
				}

				packet, err := conn.ReadPacket()
				if err != nil || packet.Kind != 0x10 || bytes.Equal(packet.Payload, make([]byte, 320)) {
					t.Fatalf("missing early audio: kind=%x err=%v", packet.Kind, err)
				}

				gateway.mu.Lock()
				u := *gateway.use("global")
				gateway.mu.Unlock()
				if u.calls != 1 || u.pending != 1 {
					t.Fatalf("early media answered the call: %+v", u)
				}

				switch outcome {
				case "answer", "answer_same_sdp", "answer_without_sdp":
					final <- 200
					// Wait for activation so the next frame uses the final codec/port.
					for {
						gateway.mu.Lock()
						active := gateway.use("global").pending == 0
						gateway.mu.Unlock()
						if active {
							break
						}

						select {
						case <-ctx.Done():
							t.Fatal("call did not activate")
						case <-time.After(5 * time.Millisecond):
						}
					}

					if err := conn.WriteAudio(8000, payload); err != nil {
						t.Fatal(err)
					}

					packet, err = conn.ReadPacket()
					if err != nil || packet.Kind != 0x10 || bytes.Equal(packet.Payload, make([]byte, 320)) {
						t.Fatalf("missing answered audio: kind=%x err=%v", packet.Kind, err)
					}

					conn.Close()
				case "reject":
					final <- 486
				case "disconnect":
					conn.Close()
				}

				if outcome == "reject" || outcome == "timeout" {
					for {
						packet, err = conn.ReadPacket()
						if err != nil || packet.Kind != 0x10 {
							break
						}
					}

					if err != nil || packet.Kind != audiosocket.KindError {
						t.Fatalf("missing setup failure: kind=%x err=%v", packet.Kind, err)
					}

					if _, err := conn.ReadPacket(); err == nil || ctx.Err() != nil {
						t.Fatalf("connection did not close promptly: %v", err)
					}
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
						t.Fatal("early media leaked call capacity")
					case <-time.After(5 * time.Millisecond):
					}
				}

				if outcome == "timeout" || outcome == "disconnect" {
					select {
					case <-canceled:
					case <-ctx.Done():
						t.Fatal("pending SIP call did not receive CANCEL")
					}
				}
			})
		}
	}
}

func TestSIPToSIPEarlyMedia(t *testing.T) {
	peer, final, _ := earlySIPPeer(t, 180, "pcma")
	address := freeAddress(t)
	gateway := testEngine(t, fmt.Sprintf(`source "phone" {
 type = "sip"
 bind = %q
 codecs = ["pcmu"]
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
 codecs = ["pcmu", "pcma"]
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, address, peer))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	early := make(chan Leg, 1)
	type result struct {
		leg Leg
		err error
	}

	answered := make(chan result, 1)
	go func() {
		leg, err := gateway.sipEndpoint(ctx, ctx, &Node{
			Name:   "caller",
			Type:   "sip",
			Bind:   "127.0.0.1:0",
			URI:    "sip:" + address,
			Codecs: []string{"pcmu"},
		}, "test", nil, func(leg Leg) (func(), error) {
			early <- leg

			return func() {}, nil
		})
		answered <- result{leg, err}
	}()

	var leg Leg
	select {
	case leg = <-early:
	case got := <-answered:
		t.Fatalf("answered without forwarding provisional SDP: %v", got.err)
	case <-ctx.Done():
		t.Fatal("incoming SIP caller never received early SDP")
	}

	data := encode(tone('5', 8000)[:160], leg.Format())
	if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
		t.Fatal(err)
	}

	frame, err := leg.Read(ctx)
	if err != nil || !bytes.Equal(data, frame.Data) {
		t.Fatalf("SIP early-media echo failed: %v", err)
	}

	select {
	case <-answered:
		t.Fatal("gateway answered before the provider")
	default:
	}

	final <- 200
	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatal(got.err)
		}

		defer got.leg.Close()
		if err := got.leg.Write(ctx, Frame{Format: got.leg.Format(), Data: data}); err != nil {
			t.Fatal(err)
		}

		frame, err := got.leg.Read(ctx)
		if err != nil || len(frame.Data) != len(data) {
			t.Fatalf("answered SIP media failed after codec change: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("gateway did not relay the final answer")
	}
}

func TestSIPEarlyMediaSetupErrors(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want string
	}{
		{"invalid_sdp", "SIP provisional SDP"},
		{"100rel", "100rel/PRACK"},
		{"no_transcoding", "transcode never"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			peer, _, canceled := earlySIPPeer(t, 183, "pcmu", func(res *sip.Response) {
				if res.StatusCode != 183 {
					return
				}

				switch scenario.name {
				case "invalid_sdp":
					res.SetBody([]byte("invalid SDP"))
				case "100rel":
					res.AppendHeader(sip.NewHeader("Require", "100rel"))
				}
			})
			e := testEngine(t, fmt.Sprintf(`source "audio" {
 type = "audiosocket"
 bind = %q
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, freeAddress(t), peer))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			source := e.config.source("audio")
			if scenario.name == "no_transcoding" {
				source.Transcode = &TranscodeConfig{Mode: "never"}
			}

			in := newChannelLeg(ctx, Format{"pcm_s16le", 8000, 1}, 1)
			defer in.Close()
			call, err := e.prepare(ctx, source, &Request{ID: randomID(), Source: "audio"}, in)
			if call != nil {
				call.Close()
			}

			if err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("expected %q, got %v", scenario.want, err)
			}

			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("failed early negotiation did not cancel the SIP call")
			}
		})
	}
}
