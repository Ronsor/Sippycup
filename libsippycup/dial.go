package libsippycup

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// DialCommand connects a PCM subprocess to a configured endpoint and waits for
// the call to end. It starts only the SIP source needed by that endpoint.
// Config must be validated with Validate and remain immutable during
// the call. Its routes are not modified. Cancellation closes the call and child.
func DialCommand(ctx context.Context, config *Config, name, to string, rate int, command []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	copyConfig := *config
	e := NewEngine(&copyConfig)
	defer e.Close()
	stop := context.AfterFunc(ctx, e.cancel)
	defer stop()

	n := e.config.endpoint(name)
	if n == nil {
		return fmt.Errorf("unknown endpoint %q", name)
	}

	if len(command) == 0 {
		return errors.New("dial requires a command after --")
	}

	e.config.Routes = append(
		[]*Route{
			{
				Name:  "cli",
				Match: &Match{Source: "@cli"},
				Connect: &Connect{
					Endpoint:    name,
					Destination: hcl.StaticExpr(cty.StringVal(to), hcl.Range{}),
				},
			},
		},
		e.config.Routes...,
	)
	if n.Source != "" {
		source := e.config.source(n.Source)
		t, err := e.startSIP(source)
		if err != nil {
			return err
		}

		e.setSIP(source.Name, t)
	}

	source := &Node{
		Name:    "@cli",
		Type:    "exec",
		Command: command,
		IO:      "pcm",
		Transcode: &TranscodeConfig{
			Mode:       "always",
			Codec:      "pcm_s16le",
			SampleRate: rate,
			Channels:   1,
		},
	}
	if _, err := formatFor("pcm_s16le", rate, 1); err != nil {
		return err
	}

	request := &Request{ID: randomID(), Source: source.Name}
	eval := e.config.EvalContext.NewChild()
	eval.Variables = map[string]cty.Value{"request": request.value()}
	in, err := e.execEndpoint(e.ctx, source, request, eval, &Connect{}, Format{"pcm_s16le", rate, 1})
	if err != nil {
		return err
	}

	defer in.Close()
	call, err := e.prepare(e.ctx, source, request, in)
	if err != nil {
		return err
	}

	call.Run()

	return nil
}
