package audiosocket

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPacketFraming(t *testing.T) {
	var wire bytes.Buffer
	packets := []Packet{
		{Kind: KindID, Payload: make([]byte, 16)},
		{Kind: 0x10, Payload: []byte{1, 0, 2, 0}},
		{Kind: KindDTMF, Payload: []byte{'#'}},
		{Kind: KindHangup},
	}
	for _, p := range packets {
		if err := WritePacket(&wire, p); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range packets {
		got, err := ReadPacket(&wire, MaxPayload)
		if err != nil || got.Kind != want.Kind || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("got %#v %v; want %#v", got, err, want)
		}
	}

	if _, err := ReadPacket(&wire, MaxPayload); err != io.EOF {
		t.Fatal(err)
	}

	if err := WritePacket(io.Discard, Packet{Payload: make([]byte, 65536)}); err == nil {
		t.Fatal("oversized write accepted")
	}

	if _, err := ReadPacket(bytes.NewReader([]byte{1, 255, 255}), 100); err == nil {
		t.Fatal("oversized read accepted")
	}

	if _, err := ReadPacket(bytes.NewReader([]byte{1, 0, 2, 1}), 100); err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
}

func TestIdentity(t *testing.T) {
	uuid := make([]byte, 16)
	uuid[0] = '{'
	if _, err := ParseIdentity(uuid, false); err != nil {
		t.Fatal(err)
	}

	if id, err := ParseIdentity(uuid, true); err != nil || !bytes.Equal(id.UUID[:], uuid) || len(id.JSON) != 0 {
		t.Fatal("16-byte UUID was not preserved", err)
	}

	data := []byte(`{
 "uuid":"550e8400-e29b-41d4-a716-446655440000",
 "destination":"123",
 "basic_auth":{"username":"a","password":"b"}
}`)
	id, err := ParseIdentity(data, true)
	if err != nil {
		t.Fatal(err)
	}

	auth, err := id.BasicAuth()
	if err != nil || auth.Username != "a" || auth.Password != "b" {
		t.Fatal(auth, err)
	}

	if _, err := ParseIdentity(data, false); err == nil {
		t.Fatal("standard mode accepted JSON")
	}

	if _, err := ParseIdentity([]byte(`{bad`), true); err == nil {
		t.Fatal("malformed JSON accepted")
	}

	if _, err := ParseIdentity([]byte(`[]`), true); err == nil {
		t.Fatal("array accepted")
	}
}

func TestWrappedStream(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	left, right := Wrap(a, WithExtended(true)), Wrap(b, WithExtended(true))
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() {
		done <- left.WriteIdentity(Identity{
			JSON: json.RawMessage(`{"uuid":"550e8400-e29b-41d4-a716-446655440000","x":1}`),
		})
	}()
	p, err := right.ReadPacket()
	if err != nil || string(p.Payload) != `{"uuid":"550e8400-e29b-41d4-a716-446655440000","x":1}` {
		t.Fatal(p, err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTCPAndWebSocket(t *testing.T) {
	for _, scheme := range []string{"tcp", "ws"} {
		t.Run(scheme, func(t *testing.T) {
			l, err := Listen(scheme+"://127.0.0.1:0/audio", WithExtended(true))
			if err != nil {
				t.Fatal(err)
			}

			defer l.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				c, err := l.Accept(ctx)
				if err != nil {
					done <- err

					return
				}

				defer c.Close()
				p, err := c.ReadPacket()
				if err == nil {
					err = c.WritePacket(p)
				}

				done <- err
			}()
			c, err := Dial(ctx, scheme+"://"+l.Addr().String()+"/audio", WithExtended(true))
			if err != nil {
				t.Fatal(err)
			}

			defer c.Close()
			if err := c.WriteIdentity(Identity{
				JSON: json.RawMessage(`{"uuid":"550e8400-e29b-41d4-a716-446655440000","hello":"world"}`),
			}); err != nil {
				t.Fatal(err)
			}

			p, err := c.ReadPacket()
			if err != nil || string(p.Payload) != `{"uuid":"550e8400-e29b-41d4-a716-446655440000","hello":"world"}` {
				t.Fatal(p, err)
			}

			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPHandler(t *testing.T) {
	server := httptest.NewServer(Handler(func(c *Conn, _ *http.Request) {
		defer c.Close()
		p, err := c.ReadPacket()
		if err == nil {
			_ = c.WritePacket(p)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}

	defer c.Close()
	if err := c.WriteDTMF('5'); err != nil {
		t.Fatal(err)
	}

	p, err := c.ReadPacket()
	if err != nil || p.Kind != KindDTMF {
		t.Fatal(p, err)
	}
}

func FuzzPacket(f *testing.F) {
	f.Add([]byte{1, 0, 16})
	f.Add([]byte{3, 0, 1, '1'})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ReadPacket(bytes.NewReader(b), 65535)
		if err != nil {
			return
		}

		var out bytes.Buffer
		if err := WritePacket(&out, p); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(out.Bytes(), b[:len(out.Bytes())]) {
			t.Fatal("round trip changed packet")
		}
	})
}

func TestExtendedCallState(t *testing.T) {
	for _, scheme := range []string{"tcp", "ws"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := Listen(scheme+"://127.0.0.1:0/audio", WithExtended(true))
			if err != nil {
				t.Fatal(err)
			}

			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept(ctx)
				if err != nil {
					done <- err

					return
				}

				defer conn.Close()
				for _, state := range []CallState{StateEarly, StateAnswered} {
					if err := conn.WriteState(state); err != nil {
						done <- err

						return
					}
				}
				done <- nil
			}()

			conn, err := Dial(ctx, scheme+"://"+listener.Addr().String()+"/audio",
				WithExtended(true), WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}

			defer conn.Close()
			for _, want := range []CallState{StateEarly, StateAnswered} {
				packet, err := conn.ReadPacket()
				got, parseErr := ParseState(packet)
				if err != nil || parseErr != nil || got != want {
					t.Fatalf("state=%v, want %v: %v %v", got, want, err, parseErr)
				}
			}

			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, packet := range []Packet{
		{Kind: KindID, Payload: []byte{0}},
		{Kind: KindState},
		{Kind: KindState, Payload: []byte{2}},
		{Kind: KindState, Payload: []byte{0, 1}},
	} {
		if _, err := ParseState(packet); err == nil {
			t.Errorf("accepted invalid state: %+v", packet)
		}
	}

	left, right := net.Pipe()
	defer right.Close()
	conn := Wrap(left)
	defer conn.Close()
	if err := conn.WriteState(StateEarly); err == nil {
		t.Fatal("standard mode accepted an extended state message")
	}
}

func TestExtendedIdentityDiscriminator(t *testing.T) {
	for _, payload := range [][]byte{
		append([]byte{'{'}, make([]byte, 15)...),
		[]byte(`{"uuid":"12345"}`),
	} {
		if len(payload) != 16 {
			t.Fatalf("bad test fixture: %d bytes", len(payload))
		}

		id, err := ParseIdentity(payload, true)
		if err != nil || len(id.JSON) != 0 || !bytes.Equal(id.UUID[:], payload) {
			t.Fatalf("16-byte identity must remain binary: %#v, %v", id, err)
		}
	}

	for _, payload := range []string{
		`{}`, `{"uuid":null}`, `{"destination":"1234"}`,
		`{"uuid":12345678901234567890}`,
		`{"uuid":"not-a-valid-uuid"}`,
		`{"uuid":"550e8400e29b41d4a716446655440000"}`,
		`{"uuid":"550e8400-e29b-41d4-a716-44665544000z"}`,
		` {"uuid":"550e8400-e29b-41d4-a716-446655440000"}`,
		`{"uuid":"550e8400-e29b-41d4-a716-446655440000"`,
	} {
		if _, err := ParseIdentity([]byte(payload), true); err == nil {
			t.Errorf("accepted invalid extended identity %q", payload)
		}
	}

	payload := []byte(`{"uuid":"550E8400-E29B-41D4-A716-446655440000","extra":true}`)
	id, err := ParseIdentity(payload, true)
	if err != nil {
		t.Fatal(err)
	}

	want := [16]byte{0x55, 0x0e, 0x84, 0, 0xe2, 0x9b, 0x41, 0xd4, 0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0, 0}
	if id.UUID != want || !bytes.Equal(id.JSON, payload) {
		t.Fatalf("incorrect parsed identity: %#v", id)
	}

	payload[1] = 'x'
	if id.JSON[1] != '"' {
		t.Fatal("JSON identity aliases caller memory")
	}
}

func TestWriteIdentityRejectsInvalidJSON(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := Wrap(left, WithExtended(true))
	_ = left.SetWriteDeadline(time.Now().Add(time.Second))
	for _, payload := range []string{`{}`, `{"uuid":"12345"}`, `{"destination":"1234"}`} {
		if err := conn.WriteIdentity(Identity{JSON: json.RawMessage(payload)}); err == nil {
			t.Errorf("accepted invalid JSON identity %q", payload)
		}
	}
}

func TestStereoPCM(t *testing.T) {
	for _, rate := range rates {
		kind, err := PCMKind(rate, 2)
		if err != nil {
			t.Fatal(err)
		}
		gotRate, channels, ok := PCMFormat(kind)
		if !ok || gotRate != rate || channels != 2 {
			t.Fatalf("format: %d %d %v", gotRate, channels, ok)
		}
	}
	for _, extended := range []bool{false, true} {
		for _, payload := range [][]byte{{1, 0, 2, 0}, {1, 0}} {
			a, b := net.Pipe()
			c := Wrap(a, WithExtended(extended))
			done := make(chan error, 1)
			go func() { defer b.Close(); done <- WritePacket(b, Packet{Kind: 0x26, Payload: payload}) }()
			_, err := c.ReadPacket()
			c.Close()
			if wantErr := !extended || len(payload)%4 != 0; (err != nil) != wantErr {
				t.Fatalf("extended=%v payload=%v: %v", extended, payload, err)
			}
			<-done
		}
	}
	server := httptest.NewServer(Handler(func(c *Conn, _ *http.Request) {
		defer c.Close()
		p, err := c.ReadPacket()
		if err == nil {
			_ = c.WritePacket(p)
		}
	}, WithExtended(true)))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), WithExtended(true))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	if err := c.WritePCM(48000, 2, pcm); err != nil {
		t.Fatal(err)
	}
	p, err := c.ReadPacket()
	if err != nil || p.Kind != 0x26 || !bytes.Equal(p.Payload, pcm) {
		t.Fatalf("stereo WS: %v %v", p, err)
	}
}
