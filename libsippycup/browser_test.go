package libsippycup

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserNegotiation(t *testing.T) {
	for _, test := range []struct {
		name, method, accept, contentType, upgrade string
		want                                       bool
	}{
		{"browser", "GET", "text/html,application/xhtml+xml,*/*;q=0.8", "", "", true},
		{"head", "HEAD", "text/html", "", "", true},
		{"generic", "GET", "*/*", "", "", false},
		{"missing", "GET", "", "", "", false},
		{"api", "GET", "application/sdp,text/html;q=0.5", "", "", false},
		{"excluded", "GET", "text/html;q=0", "", "", false},
		{"invalid", "GET", "text/html;q=NaN", "", "", false},
		{"sdp", "POST", "text/html", "application/sdp", "", false},
		{"sdp get", "GET", "text/html", "application/sdp", "", false},
		{"upgrade", "GET", "text/html", "", "websocket", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "/calls", nil)
			r.Header.Set("Accept", test.accept)
			r.Header.Set("Content-Type", test.contentType)
			r.Header.Set("Upgrade", test.upgrade)
			if got := prefersHTML(r); got != test.want {
				t.Fatalf("prefersHTML=%t, want %t", got, test.want)
			}
		})
	}
}

func TestBrowserPageAndAssets(t *testing.T) {
	node := &Node{Name: "<script>unsafe</script>", Type: "audiosocket_ws", Path: "/audio", Extended: true}
	for _, method := range []string{"GET", "HEAD"} {
		r := httptest.NewRequest(method, "/audio?destination=123", nil)
		r.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		if !serveBrowser(node, w, r) || w.Code != 200 {
			t.Fatal("page not served")
		}

		if strings.Contains(w.Body.String(), "<script>unsafe") {
			t.Fatal("unescaped configuration in page")
		}

		if method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}

	for _, name := range []string{"call.js", "audio-worklet.js"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/audio?sippycup_asset="+name, nil)
		if !serveBrowser(node, w, r) || w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("missing asset %s", name)
		}
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/audio/sessions/secret", nil)
	r.Header.Set("Accept", "text/html")
	if serveBrowser(node, w, r) {
		t.Fatal("page intercepted session resource")
	}
}

func TestBrowserHTTPAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	listener.Close()
	config, err := fixtureConfig([]byte(fmt.Sprintf(`credential "user" {
 username = "alice"
 password = "secret"
}
credential_set "users" { credentials = [credential.user] }
source "rtc" {
 type = "webrtc_http"
 bind = %q
 path = "/calls"
 auth {
  mode = "verify"
  credential_set = credential_set.users
 }
}
source "ws" {
 type = "audiosocket_ws"
 bind = %q
 path = "/audio"
 auth {
  mode = "verify"
  credential_set = credential_set.users
 }
}
`, address, address)), "browser.hcl")
	if err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(config)
	defer engine.Close()
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/calls", "/audio", "/calls?sippycup_asset=call.js"} {
		for _, authenticated := range []bool{false, true} {
			r, err := http.NewRequest("GET", "http://"+address+path, nil)
			if err != nil {
				t.Fatal(err)
			}

			r.Header.Set("Accept", "text/html")
			if authenticated {
				r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
			}

			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}

			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			want := http.StatusUnauthorized
			if authenticated {
				want = http.StatusOK
			}

			if response.StatusCode != want {
				t.Fatalf("%s auth=%t: %d, %s", path, authenticated, response.StatusCode, body)
			}
		}
	}
}
