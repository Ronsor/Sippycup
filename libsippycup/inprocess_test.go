package libsippycup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/ronsor/sippycup/libsippycup"
	"github.com/zclconf/go-cty/cty"
)

var pcm = libsippycup.Format{Codec: "pcm_s16le", Rate: 8000, Channels: 1}

func memoryPair(t *testing.T, ctx context.Context) (*libsippycup.MemoryLeg, *libsippycup.MemoryLeg) {
	t.Helper()
	a, b, err := libsippycup.NewLegPair(ctx, pcm, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, b
}

func localConfig(fn libsippycup.EndpointFunc) *libsippycup.Config {
	return &libsippycup.Config{
		Sources: []*libsippycup.Node{{Name: "app", Type: "inproc"}},
		Endpoints: []*libsippycup.Node{{
			Name: "handler", Type: "inproc", InProcess: fn,
		}},
		Routes: []*libsippycup.Route{{
			Name: "local",
			Connect: &libsippycup.Connect{
				Endpoint:    "handler",
				Destination: hcl.StaticExpr(cty.StringVal("alice"), hcl.Range{}),
				Metadata: hcl.StaticExpr(cty.ObjectVal(map[string]cty.Value{
					"label": cty.StringVal("hello"),
				}), hcl.Range{}),
			},
		}},
	}
}

func localEngine(t *testing.T, c *libsippycup.Config) *libsippycup.Engine {
	t.Helper()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	e := libsippycup.NewEngine(c)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestInProcessRoutingAndReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	called := make(chan libsippycup.EndpointRequest, 1)
	engineSide, appSide := memoryPair(t, ctx)
	e := localEngine(t, localConfig(func(_ context.Context, r libsippycup.EndpointRequest) (libsippycup.Leg, error) {
		called <- r
		appSide.WriteState(ctx, libsippycup.StateAnswered)
		return engineSide, nil
	}))
	source, caller := memoryPair(t, ctx)
	done := make(chan error, 1)
	go func() {
		done <- e.Accept(ctx, "app", libsippycup.Request{Identity: map[string]any{"user": "bob"}}, source)
	}()
	var request libsippycup.EndpointRequest
	select {
	case request = <-called:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	if request.Destination != "alice" || request.Request.Source != "app" || request.Request.ID == "" ||
		request.Metadata.(map[string]any)["label"] != "hello" || request.Format != pcm {
		t.Fatalf("incorrect endpoint request: %+v", request)
	}

	if state, err := caller.ReadState(ctx); err != nil || state != libsippycup.StateAnswered {
		t.Fatalf("answer: %v, %v", state, err)
	}

	next := localConfig(func(context.Context, libsippycup.EndpointRequest) (libsippycup.Leg, error) {
		return nil, errors.New("replacement handler")
	})
	if err := e.Reload(next); err != nil {
		t.Fatal(err)
	}

	data := bytes.Repeat([]byte{1, 2}, 160)
	for _, direction := range [][2]*libsippycup.MemoryLeg{{caller, appSide}, {appSide, caller}} {
		if err := direction[0].Write(ctx, libsippycup.Frame{Format: pcm, Data: data}); err != nil {
			t.Fatal(err)
		}
		frame, err := direction[1].Read(ctx)
		if err != nil || !bytes.Equal(frame.Data, data) {
			t.Fatalf("audio after reload: %v", err)
		}
		for i := 0; i < 2; i++ {
			direction[0].Write(ctx, libsippycup.Frame{Digit: '5'})
			frame, err = direction[1].Read(ctx)
			if err != nil || frame.Digit != '5' {
				t.Fatalf("DTMF: %+v %v", frame, err)
			}
		}
	}
	newSource, _ := memoryPair(t, ctx)
	if err := e.Accept(ctx, "app", libsippycup.Request{}, newSource); err == nil ||
		err.Error() != "replacement handler" {
		t.Fatalf("new call did not use replacement: %v", err)
	}

	caller.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("call did not terminate")
	}

	select {
	case <-request.CallContext.Done():
	default:
		t.Fatal("endpoint lifetime was not canceled")
	}
}

func TestInProcessEarlyMedia(t *testing.T) {
	for _, mode := range []string{"native", "answer", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a, b := memoryPair(t, ctx)
			config := localConfig(func(context.Context, libsippycup.EndpointRequest) (libsippycup.Leg, error) {
				return a, nil
			})
			config.Sources[0].EarlyMedia = mode
			e := localEngine(t, config)
			source, caller := memoryPair(t, ctx)
			done := make(chan error, 1)
			go func() { done <- e.Accept(ctx, "app", libsippycup.Request{}, source) }()
			b.WriteState(ctx, libsippycup.StateEarly)
			if mode != "disabled" {
				want := libsippycup.StateEarly
				if mode == "answer" {
					want = libsippycup.StateAnswered
				}
				state, err := caller.ReadState(ctx)
				if err != nil || state != want {
					t.Fatalf("early state: %v %v", state, err)
				}
				b.Write(ctx, libsippycup.Frame{Format: pcm, Data: make([]byte, 320)})
				if _, err := caller.Read(ctx); err != nil {
					t.Fatal(err)
				}
			}
			b.WriteState(ctx, libsippycup.StateAnswered)
			if mode != "answer" {
				state, err := caller.ReadState(ctx)
				if err != nil || state != libsippycup.StateAnswered {
					t.Fatalf("answer state: %v %v", state, err)
				}
			}
			caller.Close()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("early call did not terminate")
			}
		})
	}
}

func TestInProcessShutdownDuringSetup(t *testing.T) {
	entered := make(chan struct{})
	e := localEngine(t, localConfig(func(ctx context.Context, _ libsippycup.EndpointRequest) (libsippycup.Leg, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	source, _ := memoryPair(t, ctx)
	done := make(chan error, 1)
	go func() { done <- e.Accept(ctx, "app", libsippycup.Request{}, source) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("callback did not start")
	}

	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("shutdown failed to cancel setup")
	}

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("setup cancellation: %v", err)
	}
}

func TestMemoryLegOwnershipAndBackpressure(t *testing.T) {
	ctx := context.Background()
	a, b, err := libsippycup.NewLegPair(ctx, pcm, 1)
	if err != nil {
		t.Fatal(err)
	}

	defer a.Close()
	data := []byte{1, 2}
	a.Write(ctx, libsippycup.Frame{Format: pcm, Data: data})
	data[0] = 9
	f, err := b.Read(ctx)
	if err != nil || f.Data[0] != 1 {
		t.Fatal("Write did not copy audio")
	}
	a.Write(ctx, libsippycup.Frame{Digit: '1'})
	timeout, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := a.Write(timeout, libsippycup.Frame{Digit: '2'}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queue did not apply backpressure: %v", err)
	}
	a.WriteState(ctx, libsippycup.StateAnswered)
	if err := a.WriteState(ctx, libsippycup.StateEarly); err == nil {
		t.Fatal("accepted backwards call state")
	}
	a.Close()
	if _, err := b.Read(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("peer did not close: %v", err)
	}
}

func TestInProcessLimitsAndPeerClosureDuringSetup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	config := localConfig(func(ctx context.Context, _ libsippycup.EndpointRequest) (libsippycup.Leg, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	config.Runtime = &libsippycup.RuntimeConfig{Limits: &libsippycup.LimitConfig{MaxCalls: 1}}
	e := localEngine(t, config)
	source, caller := memoryPair(t, ctx)
	done := make(chan error, 1)
	go func() { done <- e.Accept(ctx, "app", libsippycup.Request{}, source) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second, _ := memoryPair(t, ctx)
	if err := e.Accept(ctx, "app", libsippycup.Request{}, second); err == nil {
		t.Fatal("call limit not enforced")
	}

	select {
	case <-entered:
		t.Fatal("rejected call invoked callback")
	default:
	}
	caller.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("peer closure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("peer closure did not interrupt setup")
	}
}

func TestInProcessTranscoding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	g711 := libsippycup.Format{Codec: "pcmu", Rate: 8000, Channels: 1}
	a, b, err := libsippycup.NewLegPair(ctx, g711, 4)
	if err != nil {
		t.Fatal(err)
	}

	defer b.Close()
	config := localConfig(func(_ context.Context, r libsippycup.EndpointRequest) (libsippycup.Leg, error) {
		if r.Format != g711 {
			return nil, errors.New("incorrect forced format")
		}
		b.WriteState(ctx, libsippycup.StateAnswered)
		return a, nil
	})
	config.Endpoints[0].Transcode = &libsippycup.TranscodeConfig{Mode: "always", Codec: "pcmu"}
	e := localEngine(t, config)
	source, caller := memoryPair(t, ctx)
	done := make(chan error, 1)
	go func() { done <- e.Accept(ctx, "app", libsippycup.Request{}, source) }()
	if _, err := caller.ReadState(ctx); err != nil {
		t.Fatal(err)
	}
	caller.Write(ctx, libsippycup.Frame{Format: pcm, Data: make([]byte, 320)})
	frame, err := b.Read(ctx)
	if err != nil || frame.Format != g711 || len(frame.Data) != 160 {
		t.Fatalf("PCM to G.711: %+v %v", frame, err)
	}
	b.Write(ctx, frame)
	frame, err = caller.Read(ctx)
	if err != nil || frame.Format != pcm || len(frame.Data) != 320 {
		t.Fatalf("G.711 to PCM: %+v %v", frame, err)
	}
	b.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("transcoding call did not close")
	}
}

func TestInProcessConfigurationValidation(t *testing.T) {
	for _, mutate := range []func(*libsippycup.Config){
		func(c *libsippycup.Config) { c.Endpoints[0].InProcess = nil },
		func(c *libsippycup.Config) { c.Sources[0].Bind = "127.0.0.1:1234" },
		func(c *libsippycup.Config) { c.Sources[0].InProcess = c.Endpoints[0].InProcess },
		func(c *libsippycup.Config) { c.Endpoints[0].Auth = &libsippycup.AuthConfig{} },
	} {
		c := localConfig(func(context.Context, libsippycup.EndpointRequest) (libsippycup.Leg, error) {
			return nil, nil
		})
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Fatal("invalid inproc configuration accepted")
		}
	}
}
