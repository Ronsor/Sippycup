package libsippycup

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/sergeymakinen/go-crypt/sha256"
)

func TestSymmetricRTPDefaultsAndOptOut(t *testing.T) {
	for _, tc := range []struct {
		block string
		want  bool
	}{
		{"", true},
		{"rtp {}", true},
		{"rtp { symmetric=true }", true},
		{"rtp { symmetric=false }", false},
	} {
		c, err := fixtureConfig(
			[]byte("source \"sip\" {\n type=\"sip\"\n bind=\"127.0.0.1:0\"\n"+tc.block+"\n}\n"),
			"symmetric.hcl",
		)
		if err != nil {
			t.Fatal(err)
		}

		if got := c.source("sip").RTP.symmetricEnabled(); got != tc.want {
			t.Errorf("%q: symmetric=%t, want %t", tc.block, got, tc.want)
		}
	}
}

func TestHTTPReservedSessionPaths(t *testing.T) {
	for _, path := range []string{"/", "/calls"} {
		nodes := []*Node{
			{Name: "rtc", Type: "webrtc_http", Bind: ":8080", Path: path},
			{
				Name: "audio",
				Type: "audiosocket_ws",
				Bind: ":8080",
				Path: strings.TrimSuffix(path, "/") + "/sessions/other",
			},
		}
		if err := validateHTTPGroups(nodes); err == nil {
			t.Fatalf("accepted path inside reserved session subtree for %q", path)
		}
	}
}

func TestRoutesExtractAndDefault(t *testing.T) {
	text := `source "audio" {
 type = "audiosocket"
 bind = "127.0.0.1:0"
 extended = true
}
endpoint "upstream" {
 type = "sip"
 uri = "sip:localhost"
 auth { credential = request.auth.credential }
}
route "dial" {
 match {
  source = source.audio
  capture "number" {
   value = request.identity.destination
   pattern = "^\\+(?P<digits>[1-9][0-9]+)$"
  }
 }
 connect {
  endpoint = endpoint.upstream
  destination = capture.number.digits
 }
}
route "deny" {
 reject { code = 403 }
}
`
	c, err := fixtureConfig([]byte(text), "routes.hcl")
	if err != nil {
		t.Fatal(err)
	}

	req := &Request{
		Source:   "audio",
		Identity: map[string]any{"destination": "+12345"},
		Auth:     &AuthIdentity{Username: "a", Password: "b"},
	}
	r, ctx, err := c.selectRoute(req)
	if err != nil || r.Name != "dial" {
		t.Fatal(r, err)
	}

	v, err := exprString(r.Connect.Destination, ctx)
	if err != nil || v != "12345" {
		t.Fatal(v, err)
	}

	cr, err := c.outboundCredential(c.endpoint("upstream"), ctx, req.Auth)
	if err != nil || cr.Username != "a" || *cr.Password != "b" {
		t.Fatal(cr, err)
	}

	req.Identity = "00000000000000000000000000000000"
	r, _, err = c.selectRoute(req)
	if err != nil || r.Name != "deny" {
		t.Fatal(r, err)
	}
}

func TestHashedBasicCredentials(t *testing.T) {
	hash, hashErr := sha256.NewHash("secret", 1000)
	if hashErr != nil {
		t.Fatal(hashErr)
	}

	text := fmt.Sprintf(`credential "a" {
 username="a"
 password_hash=%q
}
credential_set "users" { credentials=[credential.a] }
`, hash)
	c, err := fixtureConfig([]byte(text), "auth.hcl")
	if err != nil {
		t.Fatal(err)
	}

	a := &AuthConfig{Mode: "verify", CredentialSet: "users"}
	id := &AuthIdentity{Username: "a", Password: "secret"}
	if err = c.verifyBasic(a, id); err != nil || !id.Verified {
		t.Fatal(err)
	}

	id.Password = "bad"
	if c.verifyBasic(a, id) == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestMediaTranscodingAndDTMF(t *testing.T) {
	for _, codec := range []string{"pcmu", "pcma", "pcm_s16le"} {
		format, _ := formatFor(codec, 8000, 1)
		samples := tone('5', 8000)
		decoded := decode(Frame{Format: format, Data: encode(samples, format)})
		var d detector
		digits := d.process(decoded, 8000)
		if len(digits) != 1 || digits[0] != '5' {
			t.Fatalf("%s: digits %q", codec, digits)
		}
	}

	var r resampler
	input := make([]int16, 48000)
	for i := range input {
		input[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}

	out := []int16{}
	for i := 0; i < len(input); i += 960 {
		out = append(out, r.process(input[i:i+960], 48000, 8000)...)
	}

	if len(out) < 7990 || len(out) > 8000 {
		t.Fatal(len(out))
	}

	energy := 0.0
	for _, v := range out {
		energy += float64(v) * float64(v)
	}

	if rms := math.Sqrt(energy / float64(len(out))); rms < 6500 || rms > 7500 {
		t.Fatal(rms)
	}
}

func TestAllDTMFDigits(t *testing.T) {
	for _, digit := range []byte(dtmfKeys) {
		var detector detector
		got := detector.process(tone(digit, 16000), 16000)
		if string(got) != string(digit) {
			t.Fatalf("%c: %q", digit, got)
		}
	}
}

func TestNoSecretsInRequest(t *testing.T) {
	r := &Request{Auth: &AuthIdentity{Username: "user", Password: "secret"}}
	if strings.Contains(r.value().GoString(), "secret") {
		t.Fatal("secret in expression context")
	}
}
