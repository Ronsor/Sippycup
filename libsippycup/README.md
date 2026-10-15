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
