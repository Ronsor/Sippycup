# libsippycup

[← Sippycup](../README.md)

Import `github.com/ronsor/sippycup/libsippycup`. Build a `Config`, call `Validate`
to check it and prepare defaults, then create an engine. For example, this forwards
incoming AudioSocket calls to a second AudioSocket server:

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"github.com/ronsor/sippycup/libsippycup"
)

func main() {
	config := &libsippycup.Config{
		Sources: []*libsippycup.Node{
			{Name: "in", Type: "audiosocket", Bind: "127.0.0.1:9019"},
		},
		Endpoints: []*libsippycup.Node{
			{Name: "out", Type: "audiosocket", Address: "127.0.0.1:9020"},
		},
		Routes: []*libsippycup.Route{
			{Name: "forward", Connect: &libsippycup.Connect{Endpoint: "out"}},
		},
	}
	if err := config.Validate(); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := libsippycup.NewEngine(config).Serve(ctx); err != nil {
		log.Fatal(err)
	}
}
```

`Serve` handles startup and cleanup until cancellation. Alternatively, use `Start`
and `Close` explicitly. `DialCommand` connects a PCM subprocess to a configured
endpoint. HTTP sources include the browser call page automatically. HCL file
loading and CLI flag parsing live in the root command, not the library.

The configuration must remain immutable while the engine is running.
After `Start`, call `Reload` with a fresh, independent configuration to update new
calls while preserving existing calls and transports. A rejected reload leaves the
running configuration intact. See [reload restrictions](../docs/cli.md#lifecycle).
Do not call `Reload` concurrently with `Start` or `Close`.

Use `Done` to observe cancellation and `Close` to wait for cleanup.

See [configuration types](config.go), [package documentation](doc.go),
[subprocess endpoints](../docs/applications.md), and the independent
[AudioSocket library](../audiosocket/README.md).

## In-process calls

Use `Type: "inproc"` for a source controlled by your Go application or an
endpoint implemented by a Go callback. Sources need no bind address. Set
`Node.InProcess` on each in-process endpoint before calling `Config.Validate`.
The callback is a Go-only field; an embedding frontend can decode HCL nodes
and attach callbacks before validation. The standalone CLI cannot supply them.

An endpoint callback has this signature:

```go
func(ctx context.Context, request libsippycup.EndpointRequest) (libsippycup.Leg, error)
```

The request includes the incoming request, evaluated destination, metadata and
identity expressions, requested format, and a `CallContext`. The callback's
`ctx` limits setup and is canceled when setup ends; use `CallContext` for
returned legs and application workers. Callbacks must honor cancellation and
may run concurrently. Returning a leg successfully transfers ownership to the
engine. A returned leg must use an explicit, supported format; a forced
`transcode` format must match it.

To originate through a named source, call
`engine.Accept(ctx, sourceName, request, leg)` after `Start`, usually in a
goroutine. It routes and bridges the call until hangup. The engine owns the
supplied leg even if setup fails. Metadata stays immutable for the call;
application-supplied authentication identity is trusted. Setup errors are
returned; established-call media errors are logged.

`NewLegPair(ctx, format, capacity)` provides two connected `MemoryLeg` values.
Give one to the engine and use the other in your application. Each direction
has a bounded frame queue with backpressure. Writes copy audio buffers, and
readers own received buffers. Closing either end or canceling the pair's
context interrupts both ends. Custom legs must make `Close` safe concurrently
and unblock I/O; an optional `Done() <-chan struct{}` lets peer closure cancel
pending endpoint setup.

In-process nodes default to native early-media signaling. `MemoryLeg` implements
`StatefulLeg`: state messages travel independently from audio and DTMF.
An endpoint returning a StatefulLeg writes `StateEarly` (optional), then
`StateAnswered`, from its application end. A plain endpoint `Leg` is treated
as immediately answered. An in-process source using native mode requires a
`StatefulLeg`; the application reads its early/answered notifications.
The existing `disabled` and `answer` policies also apply. Hangup is represented
by closure, not an additional state message.

Normal routes, transcoding, DTMF policies, call limits, timeouts, shutdown, and
reload snapshots apply. New calls use replacement callbacks after reload;
existing calls retain their original legs. As with network sources, changing
the configured source set currently requires a restart.

See the runnable [in-process echo example](inprocess_example_test.go).
