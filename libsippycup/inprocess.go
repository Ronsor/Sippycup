package libsippycup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// CallState distinguishes provisional media from an answered call.
type CallState uint8

const (
	// StateEarly means media is available before the call is answered.
	StateEarly CallState = iota + 1
	// StateAnswered means call setup has completed.
	StateAnswered
)

// StatefulLeg adds native early/answered signaling to a Leg. State operations
// must honor context cancellation and Close, independently of media I/O.
type StatefulLeg interface {
	Leg
	ReadState(context.Context) (CallState, error)
	WriteState(context.Context, CallState) error
}

// EndpointRequest contains evaluated routing information for a Go endpoint.
// Request and its nested values are read-only. Format is the requested media
// format; returning a different valid format permits automatic transcoding.
// CallContext lasts for the entire call, unlike the callback's setup context.
type EndpointRequest struct {
	Request     Request
	Destination string
	Metadata    any
	Identity    any
	Format      Format
	CallContext context.Context
}

// EndpointFunc creates a fresh leg per call. The context limits setup; the
// callback must return promptly when it is canceled. Use CallContext for the
// returned leg and application workers. Successful return transfers ownership
// of the leg to the engine. For a StatefulLeg, send StateEarly (optional), then
// StateAnswered from its peer; the configured policy controls early audio. A plain
// Leg is treated as immediately answered. Callbacks may run concurrently.
type EndpointFunc func(context.Context, EndpointRequest) (Leg, error)

// Accept routes and runs one call through a named inproc source, blocking until
// it ends. Call it after Start. It takes ownership of a non-nil leg even on
// failure and closes it on return. Close must be safe concurrently and unblock
// all I/O. Context
// cancellation and Engine.Close terminate the call. A leg with Done() also
// interrupts pending setup when its peer closes.
//
// Metadata must remain immutable until Accept returns. Source is set from name;
// a missing ID is generated. Auth is trusted application-supplied identity,
// never independently verified. Setup errors are returned; established-call
// media errors are logged by the bridge. Native early media requires StatefulLeg.
func (e *Engine) Accept(ctx context.Context, name string, request Request, leg Leg) error {
	if leg == nil {
		return errors.New("inproc source requires a leg")
	}

	if !e.beginWork() {
		leg.Close()

		return e.ctx.Err()
	}

	defer func() {
		leg.Close()
		e.wg.Done()
	}()

	snapshot := e.active()
	n := snapshot.config.source(name)
	if n == nil || n.Type != "inproc" {
		return fmt.Errorf("unknown inproc source %q", name)
	}

	if err := validateInProcessFormat(n, leg); err != nil {
		return err
	}

	states, hasStates := leg.(StatefulLeg)
	if earlyMediaMode(n) == "native" && !hasStates {
		return errors.New("native inproc source requires StatefulLeg")
	}

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopEngine := context.AfterFunc(e.ctx, cancel)
	defer stopEngine()
	stopLeg := context.AfterFunc(callCtx, func() { leg.Close() })
	defer stopLeg()

	request.Source = name
	if request.ID == "" {
		request.ID = randomID()
	}

	answered := false
	request.earlyMedia = func() error {
		if !hasStates {
			return nil
		}

		state := StateEarly
		if earlyMediaMode(n) == "answer" {
			state = StateAnswered
			answered = true
		}
		return states.WriteState(callCtx, state)
	}

	call, err := snapshot.prepare(callCtx, n, &request, &inProcessLeg{Leg: leg, node: n})
	if err != nil {
		return err
	}

	defer call.Close()

	if hasStates && !answered {
		if err := states.WriteState(call.ctx, StateAnswered); err != nil {
			return err
		}
	}

	call.Run()

	return callCtx.Err()
}

func validateLegFormat(leg Leg) error {
	f := leg.Format()
	normalized, err := formatFor(f.Codec, f.Rate, f.Channels)
	if err != nil {
		return err
	}

	if normalized != f {
		return errors.New("leg format must specify codec, sample rate, and channels")
	}

	return nil
}

func inProcessValue(expr hcl.Expression, eval *hcl.EvalContext) (any, error) {
	if expr == nil {
		return nil, nil
	}

	value, diagnostics := expr.Value(eval)
	if diagnostics.HasErrors() {
		return nil, errors.New("inproc metadata expression failed")
	}

	data, err := ctyjson.Marshal(value, value.Type())
	if err != nil {
		return nil, err
	}

	var result any
	err = json.Unmarshal(data, &result)

	return result, err
}

func (e *Engine) inProcessEndpoint(
	ctx, dialCtx context.Context,
	n *Node,
	req *Request,
	eval *hcl.EvalContext,
	action *Connect,
	destination string,
	fallback Format,
	onEarly func(Leg) (func(), error),
) (Leg, error) {
	metadata, err := inProcessValue(action.Metadata, eval)
	if err != nil {
		return nil, err
	}

	identity, err := inProcessValue(action.Identity, eval)
	if err != nil {
		return nil, err
	}

	input := EndpointRequest{
		Request:     *req,
		Destination: destination,
		Metadata:    metadata,
		Identity:    identity,
		Format:      nodeFormat(n, fallback),
		CallContext: ctx,
	}
	leg, err := n.InProcess(dialCtx, input)
	if err != nil {
		if leg != nil {
			leg.Close()
		}
		return nil, err
	}

	if leg == nil {
		return nil, errors.New("inproc endpoint returned a nil leg")
	}

	keep := false
	defer func() {
		if !keep {
			leg.Close()
		}
	}()
	if err := validateInProcessFormat(n, leg); err != nil {
		return nil, err
	}

	if err := dialCtx.Err(); err != nil {
		return nil, err
	}

	states, ok := leg.(StatefulLeg)
	if !ok {
		keep = true
		return &inProcessLeg{Leg: leg, node: n}, nil
	}

	// Cancellation closes transports even when a bridge is blocked in Write.
	stopClose := context.AfterFunc(dialCtx, func() { leg.Close() })
	defer stopClose()
	stopEarly := func() {}
	defer func() { stopEarly() }()
	early := false
	for {
		state, err := states.ReadState(dialCtx)
		if err != nil {
			leg.Close()
			return nil, err
		}
		switch state {
		case StateAnswered:
			keep = true
			return &inProcessLeg{Leg: leg, node: n}, nil
		case StateEarly:
			if !early {
				stopEarly, err = onEarly(&inProcessLeg{Leg: leg, node: n})
				if err != nil {
					stopEarly = func() {}
					return nil, err
				}
				early = true
			}
		default:
			return nil, errors.New("invalid inproc call state")
		}
	}
}

// inProcessLeg applies the source's native DTMF receive policy before bridging.
type inProcessLeg struct {
	Leg
	node *Node
}

func (l *inProcessLeg) Read(ctx context.Context) (Frame, error) {
	for {
		frame, err := l.Leg.Read(ctx)
		if err != nil || frame.Digit == 0 || receives(l.node, "native_frame") {
			return frame, err
		}
	}
}

func (l *inProcessLeg) Done() <-chan struct{} {
	if leg, ok := l.Leg.(interface{ Done() <-chan struct{} }); ok {
		return leg.Done()
	}

	return nil
}

func validateInProcessFormat(n *Node, leg Leg) error {
	if err := validateLegFormat(leg); err != nil {
		return err
	}

	if n.Transcode != nil && n.Transcode.Mode == "always" &&
		nodeFormat(n, leg.Format()) != leg.Format() {
		return errors.New("inproc leg does not match forced transcode format")
	}

	return nil
}
