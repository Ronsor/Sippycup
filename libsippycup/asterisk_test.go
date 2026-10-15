//go:build asterisk

package libsippycup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This interoperability test uses an isolated Asterisk configuration and never
// changes the installed PBX. Enable the asterisk build tag and set
// SIPPYCUP_ASTERISK to an executable path or name to run it.
// SIPPYCUP_ASTERISK_MODULE_DIR optionally overrides its module path.
func TestAsteriskInteroperability(t *testing.T) {
	if testing.Short() {
		t.Skip("external Asterisk integration")
	}

	binary := os.Getenv("SIPPYCUP_ASTERISK")
	if binary == "" {
		t.Skip("set SIPPYCUP_ASTERISK to run external Asterisk integration")
	}

	binary, err := exec.LookPath(binary)
	if err != nil {
		t.Fatal(err)
	}

	moduleDirectory := ""
	if path := os.Getenv("SIPPYCUP_ASTERISK_MODULE_DIR"); path != "" {
		if strings.ContainsAny(path, "\r\n") {
			t.Fatal("Asterisk module directory must not contain newlines")
		}

		moduleDirectory = "astmoddir => " + path
	}

	dir := t.TempDir()
	address := freeAddress(t)
	audioAddress := freeAddress(t)
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("asterisk.conf", fmt.Sprintf(`[directories]
astetcdir => %[1]s
%[2]s
astvarlibdir => %[1]s
astdbdir => %[1]s
astkeydir => %[1]s
astlogdir => %[1]s
astspooldir => %[1]s
astrundir => %[1]s
[options]
systemname = sippycup-test
`, dir, moduleDirectory))
	write("modules.conf", `[modules]
autoload=no
load=res_sorcery_config.so
load=res_sorcery_memory.so
load=res_sorcery_astdb.so
load=res_pjproject.so
load=res_rtp_asterisk.so
load=res_pjsip.so
load=res_pjsip_pubsub.so
load=res_pjsip_authenticator_digest.so
load=res_pjsip_endpoint_identifier_ip.so
load=res_pjsip_endpoint_identifier_user.so
load=res_pjsip_outbound_authenticator_digest.so
load=res_pjsip_registrar.so
load=res_pjsip_session.so
load=res_pjsip_sdp_rtp.so
load=chan_pjsip.so
load=codec_ulaw.so
load=codec_alaw.so
load=codec_resample.so
load=pbx_config.so
load=app_echo.so
load=res_audiosocket.so
load=app_audiosocket.so
load=app_dial.so
load=bridge_simple.so
`)
	write("pjsip.conf", fmt.Sprintf(`[transport]
type=transport
protocol=udp
bind=%s
[gateway]
type=endpoint
transport=transport
context=testing
disallow=all
allow=ulaw,alaw
auth=gateway_auth
aors=gateway
direct_media=no
dtmf_mode=rfc4733
[gateway_auth]
type=auth
auth_type=userpass
username=gateway
password=secret
[gateway]
type=aor
max_contacts=1
remove_existing=yes
[identify]
type=identify
endpoint=gateway
match=127.0.0.1
`, address))
	write("extensions.conf", fmt.Sprintf(`[testing]
exten => echo,1,Answer()
 same => n,Echo()
exten => audio,1,Answer()
 same => n,AudioSocket(11111111-2222-3333-4444-555555555555,%s)
exten => registered,1,Dial(PJSIP/gateway,5)
`, audioAddress))
	write("logger.conf", "[logfiles]\nconsole => error,warning,notice\n")
	log, err := os.Create(filepath.Join(dir, "output.log"))
	if err != nil {
		t.Fatal(err)
	}

	defer log.Close()
	cmd := exec.Command(binary, "-f", "-g", "-C", filepath.Join(dir, "asterisk.conf"))
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}

		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "output.log"))
			if len(b) > 12000 {
				b = b[len(b)-12000:]
			}

			t.Log(string(b))
		}
	})
	deadline := time.Now().Add(8 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(dir, "output.log"))
		if bytes.Contains(b, []byte("Asterisk Ready")) {
			ready = true
			break
		}

		select {
		case err := <-done:
			t.Fatalf("Asterisk exited: %v", err)
		default:
		}

		time.Sleep(50 * time.Millisecond)
	}

	if !ready {
		t.Fatal("Asterisk did not become ready")
	}

	sourceAddress := freeAddress(t)
	_, port, _ := net.SplitHostPort(address)
	text := fmt.Sprintf(`credential "provider" {
 username="gateway"
 password="secret"
}
source "registered" {
 type="sip"
 bind=%q
 registration {
  registrar="sip:127.0.0.1:%s"
  aor="sip:gateway@127.0.0.1"
  credential=credential.provider
  expires="60s"
 }
}
source "audio" {
 type="audiosocket"
 bind=%q
}
`, sourceAddress, port, audioAddress) + echoEndpoint
	e := testEngine(t, text)
	// Wait for the registrar to publish our Contact, then dial it through Asterisk.
	registered := false
	for i := 0; i < 60; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		probe := exec.CommandContext(
			ctx, binary, "-C", filepath.Join(dir, "asterisk.conf"), "-rx", "pjsip show contacts",
		)
		out, _ := probe.CombinedOutput()
		cancel()
		if strings.Contains(string(out), sourceAddress) {
			registered = true
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if !registered {
		t.Fatal("SIP registration was not accepted")
	}

	for _, destination := range []string{"echo", "audio", "registered"} {
		t.Run(destination, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			node := &Node{
				Name:   "asterisk-out",
				Type:   "sip",
				URI:    "sip:" + address,
				Bind:   "127.0.0.1:0",
				Codecs: []string{"pcmu"},
			}
			leg, err := e.sipEndpoint(ctx, ctx, node, destination, e.config.credential("provider"))
			if err != nil {
				t.Fatal(err)
			}

			defer leg.Close()
			format := leg.Format()
			data := encode(tone('5', 8000)[:160], format)
			// Asterisk starts its dialplan just after answering; repeat paced media until echo arrives.
			go func() {
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for i := 0; i < 100; i++ {
					if err := leg.Write(ctx, Frame{Format: format, Data: data}); err != nil {
						return
					}

					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
			for {
				frame, err := leg.Read(ctx)
				if err != nil && err != io.EOF {
					t.Fatal(err)
				}

				if err != nil {
					t.Fatal("call ended before echo")
				}

				if bytes.Equal(frame.Data, data) {
					break
				}
			}
		})
	}
}
