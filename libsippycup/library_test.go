package libsippycup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ronsor/sippycup/libsippycup"
)

func TestPublicEngineLifecycle(t *testing.T) {
	config := &libsippycup.Config{}
	err := config.Validate()
	if err != nil {
		t.Fatal(err)
	}

	engine := libsippycup.NewEngine(config)
	defer engine.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- engine.Serve(ctx)
	}()
	cancel()

	select {
	case err := <-finished:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after cancellation")
	}
}

func TestDialCommandPreservesConfig(t *testing.T) {
	config := &libsippycup.Config{
		Endpoints: []*libsippycup.Node{{Name: "echo", Type: "exec", Command: []string{"unused"}}},
		Routes:    []*libsippycup.Route{{Name: "echo", Connect: &libsippycup.Connect{Endpoint: "echo"}}},
	}
	err := config.Validate()
	if err != nil {
		t.Fatal(err)
	}

	route := config.Routes[0]
	// Invalid PCM format fails before executing the placeholder command, but
	// after the helper has installed its private outbound route.
	err = libsippycup.DialCommand(context.Background(), config, "echo", "", -1, []string{"unused"})
	if err == nil {
		t.Fatal("expected invalid sample rate to fail")
	}

	if len(config.Routes) != 1 || config.Routes[0] != route {
		t.Fatal("DialCommand modified the caller's routes")
	}
}
