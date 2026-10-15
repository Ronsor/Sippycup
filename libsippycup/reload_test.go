package libsippycup

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronsor/sippycup/audiosocket"
)

func reloadFixture(t *testing.T, e *Engine, text string) {
	t.Helper()
	config, err := fixtureConfig([]byte(text), t.Name()+".hcl")
	if err != nil {
		t.Fatal(err)
	}

	if err := e.Reload(config); err != nil {
		t.Fatal(err)
	}
}

func TestReloadKeepsAudioSocketCalls(t *testing.T) {
	for _, protocol := range []string{"audiosocket", "audiosocket_ws"} {
		t.Run(protocol, func(t *testing.T) {
			address := freeAddress(t)
			path, url := "", "tcp://"+address
			if protocol == "audiosocket_ws" {
				path, url = "path=\"/audio\"", "ws://"+address+"/audio"
			}

			source := fmt.Sprintf("source \"audio\" {\n type=%q\n bind=%q\n %s\n}\n", protocol, address, path)
			e := testEngine(t, source+echoEndpoint)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			dial := func() *audiosocket.Conn {
				t.Helper()
				c, err := audiosocket.Dial(ctx, url, audiosocket.WithContext(ctx))
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { c.Close() })
				if err := c.WriteIdentity(audiosocket.Identity{UUID: [16]byte{1}}); err != nil {
					t.Fatal(err)
				}

				return c
			}

			c := dial()
			echo := func() {
				t.Helper()
				data := bytes.Repeat([]byte{1, 2}, 160)
				if err := c.WriteAudio(8000, data); err != nil {
					t.Fatal(err)
				}

				packet, err := c.ReadPacket()
				if err != nil || !bytes.Equal(packet.Payload, data) {
					t.Fatalf("echo failed: packet=%+v error=%v", packet, err)
				}
			}

			echo()
			reloadFixture(t, e, source+"route \"deny\" {\n reject { code=403 }\n}\n")
			echo()

			e.mu.Lock()
			calls := e.use("global").calls
			e.mu.Unlock()
			if calls != 1 {
				t.Fatalf("reload lost active call accounting: %d", calls)
			}

			packet, err := dial().ReadPacket()
			if err != nil || packet.Kind != audiosocket.KindError {
				t.Fatalf("new call did not use new route: kind=%x error=%v", packet.Kind, err)
			}
		})
	}
}

func TestReloadKeepsWebRTCSession(t *testing.T) {
	address := freeAddress(t)
	source := fmt.Sprintf(`source "web" {
 type="webrtc_http"
 bind=%q
 path="/calls"
 codecs=["pcmu"]
 ice { bind="127.0.0.1" }
}
`, address)
	e := testEngine(t, source+echoEndpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	leg, err := e.webEndpoint(ctx, ctx, &Node{
		Name: "client", Type: "webrtc_http", URL: "http://" + address + "/calls",
		Codecs: []string{"pcmu"}, ICE: &ICEConfig{Bind: "127.0.0.1"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer leg.Close()

	e.webMu.Lock()
	var id string
	for key := range e.web {
		id = key
	}
	e.webMu.Unlock()
	if id == "" {
		t.Fatal("missing session")
	}

	reloadFixture(t, e, strings.Replace(source, "/calls", "/new", 1)+
		"route \"deny\" {\n reject { code=403 }\n}\n")
	format := leg.Format()
	data := encode(tone('5', 8000)[:160], format)
	if err := leg.Write(ctx, Frame{Format: format, Data: data}); err != nil {
		t.Fatal(err)
	}

	frame, err := leg.Read(ctx)
	if err != nil || !bytes.Equal(frame.Data, data) {
		t.Fatalf("WebRTC audio stopped after reload: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://"+address+"/calls/sessions/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("old session URL failed: %s", resp.Status)
	}
}
