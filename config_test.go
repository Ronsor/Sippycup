package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestExampleConfiguration(t *testing.T) {
	entries, err := exampleConfigs.ReadDir("examples/config")
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".hcl")
		t.Run(name, func(t *testing.T) {
			config, err := exampleConfig(name)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := ParseConfig(config, entry.Name()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidConfigurations(t *testing.T) {
	for _, text := range []string{
		`source "x" { type="unknown" }`,
		`runtime { timeouts { dial = "bad" } }`,
		`source "x" {
 type="audiosocket"
 bind=":9019"
 auth {
 mode="passthrough"
 sources=["basic_auth"]
 }
}`,
		"source \"x\" {\n type=\"sip\"\n bind=\":5060\"\n auth { mode=\"passthrough\" }\n}",
		`source "x" {
 type="webrtc_http"
 bind=":8080"
 path="/calls"
}
source "y" {
 type="audiosocket_ws"
 bind=":8080"
 path="/calls"
}`,
		"endpoint \"x\" {\n type=\"exec\"\n command=[\"cat\"]\n transcode { codec=\"opus\" }\n}",
	} {
		if _, err := ParseConfig([]byte(text), "invalid.hcl"); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
}

func TestEarlyMediaConfiguration(t *testing.T) {
	for _, tc := range []struct {
		protocol, extra, mode string
		valid                 bool
	}{
		{"sip", "", "native", true},
		{"sip", "", "answer", true},
		{"sip", "", "disabled", true},
		{"webrtc_http", `path = "/calls"`, "answer", true},
		{"webrtc_http", `path = "/calls"`, "native", false},
		{"audiosocket", "", "answer", true},
		{"audiosocket", "", "native", false},
		{"audiosocket", "extended = true", "native", true},
		{"audiosocket_ws", "path = \"/calls\"\nextended = true", "native", true},
		{"sip", "", "typo", false},
	} {
		text := fmt.Sprintf("source \"in\" {\n type=%q\n bind=\"127.0.0.1:0\"\n %s\n early_media=%q\n}\n",
			tc.protocol, tc.extra, tc.mode)
		_, err := ParseConfig([]byte(text), "early.hcl")
		if (err == nil) != tc.valid {
			t.Errorf("%s/%s/%s: expected valid=%t, got %v", tc.protocol, tc.extra, tc.mode, tc.valid, err)
		}
	}
}

func TestParseConfigEnvironmentAndReferences(t *testing.T) {
	t.Setenv("SIPPYCUP_PARSE_TEST_PASSWORD", "secret")
	text := []byte(`credential "provider" {
  username = "user"
  password = env("SIPPYCUP_PARSE_TEST_PASSWORD")
}
endpoint "provider" {
  type = "sip"
  uri = "sip:provider.example"
  auth { credential = credential.provider }
}
route "dial" {
  connect {
    endpoint = endpoint.provider
    destination = request.identity.destination
  }
}
`)
	config, err := ParseConfig(text, "environment.hcl")
	if err != nil {
		t.Fatal(err)
	}

	if got := config.Credentials[0].Password; got == nil || *got != "secret" {
		t.Fatal("environment credential was not resolved")
	}

	if config.Routes[0].Connect.Endpoint != "provider" {
		t.Fatal("endpoint reference was not resolved")
	}

	if config.Routes[0].Connect.Identity != nil {
		t.Fatal("omitted identity expression was not normalized")
	}
}

func TestParseConfigRejectsDuplicateNames(t *testing.T) {
	_, err := ParseConfig([]byte(`endpoint "echo" {
  type = "exec"
  command = ["unused"]
}
endpoint "echo" {
  type = "exec"
  command = ["unused"]
}
`), "duplicate.hcl")
	if err == nil || !strings.Contains(err.Error(), `duplicate endpoint "echo"`) {
		t.Fatalf("expected duplicate name diagnostic, got %v", err)
	}
}

func TestFixedUUIDConfiguration(t *testing.T) {
	for _, id := range []string{"550e8400-e29b-41d4-a716-446655440000", "invalid",
		"550e8400e29b41d4a716446655440000", "550e8400-e29b-41d4-a716-44665544000z"} {
		_, err := ParseConfig([]byte(fmt.Sprintf(`endpoint "audio" {
 type = "audiosocket"
 address = "127.0.0.1:9019"
 uuid = %q
}`, id)), "uuid.hcl")
		if (err == nil) != (id == "550e8400-e29b-41d4-a716-446655440000") {
			t.Errorf("uuid %q: unexpected validation result: %v", id, err)
		}
	}

	for _, text := range []string{
		"source \"audio\" {\n type=\"audiosocket\"\n bind=\":9019\"\n",
		"endpoint \"sip\" {\n type=\"sip\"\n uri=\"sip:example.net\"\n",
	} {
		_, err := ParseConfig([]byte(text+"uuid=\"550e8400-e29b-41d4-a716-446655440000\"\n}"), "uuid.hcl")
		if err == nil {
			t.Fatal("accepted uuid on unsupported node")
		}
	}
}
