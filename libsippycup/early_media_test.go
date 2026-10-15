package libsippycup

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/ronsor/sippycup/audiosocket"
)

func TestWebRTCHTTPEarlyMedia(t *testing.T) {
	for _, outcome := range []string{"answer", "reject", "delete", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			peer, final, canceled := earlySIPPeer(t, 183, "pcmu")
			address := freeAddress(t)
			dial := "5s"
			if outcome == "timeout" {
				dial = "2s"
			}

			e := testEngine(t, fmt.Sprintf(`source "web" {
 type = "webrtc_http"
 bind = %q
 path = "/calls"
 early_media = "answer"
 codecs = ["pcmu"]
 ice { bind = "127.0.0.1" }
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
 timeouts { dial = %q }
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, address, peer, dial))
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			leg, err := e.webEndpoint(ctx, ctx, &Node{
				Name: "browser", Type: "webrtc_http", URL: "http://" + address + "/calls",
				Codecs: []string{"pcmu"}, ICE: &ICEConfig{Bind: "127.0.0.1"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}

			defer leg.Close()
			data := encode(tone('5', 8000)[:160], leg.Format())
			if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
				t.Fatal(err)
			}

			frame, err := leg.Read(ctx)
			if err != nil || !bytes.Equal(frame.Data, data) {
				t.Fatalf("missing early WebRTC audio: %v", err)
			}

			e.mu.Lock()
			pending := e.use("global").pending
			e.mu.Unlock()
			if pending != 1 {
				t.Fatal("early HTTP answer activated the downstream call")
			}

			switch outcome {
			case "answer":
				final <- 200
				waitEarlyUsage(t, ctx, e, 1, 0)
				if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
					t.Fatal(err)
				}

				frame, err := leg.Read(ctx)
				if err != nil || !bytes.Equal(frame.Data, data) {
					t.Fatalf("missing answered WebRTC audio: %v", err)
				}

				leg.Close()
			case "reject":
				final <- 486
			case "delete":
				leg.Close() // Sends authenticated session DELETE even while dialing.
			}

			waitEarlyUsage(t, ctx, e, 0, 0)
			for {
				e.webMu.Lock()
				remaining := len(e.web)
				e.webMu.Unlock()
				if remaining == 0 {
					break
				}

				select {
				case <-ctx.Done():
					t.Fatal("HTTP session leaked after call ended")
				case <-time.After(5 * time.Millisecond):
				}
			}

			if outcome == "delete" || outcome == "timeout" {
				select {
				case <-canceled:
				case <-ctx.Done():
					t.Fatal("early HTTP session teardown did not cancel SIP")
				}
			}
		})
	}
}

func waitEarlyUsage(t *testing.T, ctx context.Context, e *Engine, calls, pending int) {
	t.Helper()
	for {
		e.mu.Lock()
		u := *e.use("global")
		e.mu.Unlock()
		if u.calls == calls && u.pending == pending {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("call accounting did not settle: %+v", u)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestEarlyMediaDisabled(t *testing.T) {
	for _, disabled := range []string{"source", "endpoint"} {
		t.Run(disabled, func(t *testing.T) {
			progress := make(chan struct{}, 2)
			peer, final, _ := earlySIPPeer(t, 183, "pcmu", func(res *sip.Response) {
				if res.StatusCode == 183 {
					progress <- struct{}{}
				}
			})
			address := freeAddress(t)
			sourceMode, endpointMode := "answer", "native"
			if disabled == "source" {
				sourceMode = "disabled"
			} else {
				endpointMode = "disabled"
			}

			e := testEngine(t, fmt.Sprintf(`source "web" {
 type = "webrtc_http"
 bind = %q
 path = "/calls"
 early_media = %q
 ice { bind = "127.0.0.1" }
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
 early_media = %q
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, address, sourceMode, peer, endpointMode))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				leg, err := e.webEndpoint(ctx, ctx, &Node{
					Type: "webrtc_http", URL: "http://" + address + "/calls",
					ICE: &ICEConfig{Bind: "127.0.0.1"},
				}, nil)
				if leg != nil {
					leg.Close()
				}
				done <- err
			}()

			select {
			case <-progress:
			case <-ctx.Done():
				t.Fatal("SIP setup never started")
			}

			select {
			case err := <-done:
				t.Fatalf("disabled early media returned an early HTTP answer: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			final <- 200
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("final answer was not returned")
			}
		})
	}
}

func TestAudioSocketNativeEarlyMedia(t *testing.T) {
	for _, protocol := range []string{"audiosocket", "audiosocket_ws"} {
		t.Run(protocol, func(t *testing.T) {
			peer, final, _ := earlySIPPeer(t, 183, "pcmu")
			address := freeAddress(t)
			path, url := "", "tcp://"+address
			if protocol == "audiosocket_ws" {
				path, url = "path=\"/calls\"", "ws://"+address+"/calls"
			}

			testEngine(t, fmt.Sprintf(`source "audio" {
 type = %q
 bind = %q
 %s
 extended = true
 early_media = "native"
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, protocol, address, path, peer))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := audiosocket.Dial(ctx, url, audiosocket.WithExtended(true), audiosocket.WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}

			defer conn.Close()
			if err := conn.WriteIdentity(audiosocket.Identity{UUID: [16]byte{1}}); err != nil {
				t.Fatal(err)
			}

			packet, err := conn.ReadPacket()
			state, parseErr := audiosocket.ParseState(packet)
			if err != nil || parseErr != nil || state != audiosocket.StateEarly {
				t.Fatalf("missing early marker: %v %v", err, parseErr)
			}

			data := encode(tone('5', 8000)[:160], Format{"pcm_s16le", 8000, 1})
			if err := conn.WriteAudio(8000, data); err != nil {
				t.Fatal(err)
			}

			packet, err = conn.ReadPacket()
			if err != nil || packet.Kind != 0x10 {
				t.Fatalf("missing native early audio: %v", err)
			}

			final <- 200
			for {
				packet, err = conn.ReadPacket()
				if err != nil || packet.Kind != 0x10 {
					break
				}
			}

			state, parseErr = audiosocket.ParseState(packet)
			if err != nil || parseErr != nil || state != audiosocket.StateAnswered {
				t.Fatalf("missing answered marker: %v %v", err, parseErr)
			}
		})
	}
}

func TestNativeAudioSocketGatewayChain(t *testing.T) {
	for _, protocol := range []string{"audiosocket", "audiosocket_ws"} {
		t.Run(protocol, func(t *testing.T) {
			peer, final, _ := earlySIPPeer(t, 183, "pcmu")
			backAddress, frontAddress := freeAddress(t), freeAddress(t)
			path, target := "", fmt.Sprintf("address = %q", backAddress)
			if protocol == "audiosocket_ws" {
				path = `path = "/audio"`
				target = fmt.Sprintf("url = %q", "ws://"+backAddress+"/audio")
			}

			back := testEngine(t, fmt.Sprintf(`source "audio" {
 type = %q
 bind = %q
 %s
 extended = true
 early_media = "native"
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, protocol, backAddress, path, peer))
			front := testEngine(t, fmt.Sprintf(`source "web" {
 type = "webrtc_http"
 bind = %q
 path = "/calls"
 early_media = "answer"
 codecs = ["pcmu"]
 ice { bind = "127.0.0.1" }
}
endpoint "audio" {
 type = %q
 %s
 extended = true
 early_media = "native"
}
route "outbound" {
 connect { endpoint = endpoint.audio }
}
`, frontAddress, protocol, target))
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			leg, err := front.webEndpoint(ctx, ctx, &Node{
				Type: "webrtc_http", URL: "http://" + frontAddress + "/calls",
				Codecs: []string{"pcmu"}, ICE: &ICEConfig{Bind: "127.0.0.1"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}

			defer leg.Close()
			data := encode(tone('5', 8000)[:160], leg.Format())
			if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
				t.Fatal(err)
			}

			frame, err := leg.Read(ctx)
			if err != nil || len(frame.Data) != len(data) || bytes.Equal(frame.Data, bytes.Repeat([]byte{255}, 160)) {
				t.Fatalf("early audio did not traverse native AudioSocket gateway: %v", err)
			}

			waitEarlyUsage(t, ctx, front, 1, 1)
			waitEarlyUsage(t, ctx, back, 1, 1)
			final <- 200
			waitEarlyUsage(t, ctx, front, 1, 0)
			waitEarlyUsage(t, ctx, back, 1, 0)
			if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
				t.Fatal(err)
			}

			if _, err := leg.Read(ctx); err != nil {
				t.Fatalf("audio stopped at the native answer transition: %v", err)
			}
		})
	}
}

func TestSIPEarlyAnswerPolicy(t *testing.T) {
	peer, final, _ := earlySIPPeer(t, 183, "pcmu")
	address := freeAddress(t)
	e := testEngine(t, fmt.Sprintf(`source "phone" {
 type = "sip"
 bind = %q
 early_media = "answer"
}
endpoint "provider" {
 type = "sip"
 bind = "127.0.0.1:0"
 uri = "sip:%s"
}
route "outbound" {
 connect { endpoint = endpoint.provider }
}
`, address, peer))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	leg, err := e.sipEndpoint(ctx, ctx, &Node{
		Name: "caller", Type: "sip", Bind: "127.0.0.1:0", URI: "sip:" + address,
	}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer leg.Close()
	waitEarlyUsage(t, ctx, e, 1, 1)
	data := encode(tone('5', 8000)[:160], leg.Format())
	if err := leg.Write(ctx, Frame{Format: leg.Format(), Data: data}); err != nil {
		t.Fatal(err)
	}

	frame, err := leg.Read(ctx)
	if err != nil || !bytes.Equal(frame.Data, data) {
		t.Fatalf("early answer did not carry provider audio: %v", err)
	}

	// Failure after the source was answered must terminate with BYE, not a
	// second final response to the already completed incoming INVITE.
	final <- 486
	waitEarlyUsage(t, ctx, e, 0, 0)
	select {
	case <-leg.(*channelLeg).ended:
	case <-ctx.Done():
		t.Fatal("answered SIP source did not receive BYE after downstream failure")
	}
}
