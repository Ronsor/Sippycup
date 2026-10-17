package libsippycup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// Request contains protocol metadata used to match routes and build call legs.
// Its HCL representation excludes reusable credentials and authorization headers.
type Request struct {
	ID       string
	Source   string
	Remote   string
	Identity any
	SIP      map[string]any
	HTTP     map[string]any
	Auth     *AuthIdentity

	earlyMedia func() error
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128

	return hex.EncodeToString(b[:])
}

func ctyValue(v any) cty.Value {
	b, e := json.Marshal(v)
	if e != nil {
		return cty.NullVal(cty.DynamicPseudoType)
	}

	var j ctyjson.SimpleJSONValue
	if json.Unmarshal(b, &j) != nil {
		return cty.NullVal(cty.DynamicPseudoType)
	}

	return j.Value
}

func (r *Request) value() cty.Value {
	auth := map[string]any{"username": "", "verified": false, "credential": ""}
	if r.Auth != nil {
		auth["username"] = r.Auth.Username
		auth["verified"] = r.Auth.Verified
		auth["credential"] = "@request"
	}

	identity := r.Identity
	if identity == nil {
		identity = map[string]any{}
	}

	return ctyValue(map[string]any{
		"id":       r.ID,
		"source":   r.Source,
		"remote":   r.Remote,
		"identity": identity,
		"sip":      r.SIP,
		"http":     r.HTTP,
		"auth":     auth,
	})
}

func (c *Config) selectRoute(req *Request) (*Route, *hcl.EvalContext, error) {
	for _, r := range c.Routes {
		ctx := c.EvalContext.NewChild()
		ctx.Variables = map[string]cty.Value{"request": req.value(), "capture": cty.EmptyObjectVal}
		caps := map[string]cty.Value{}
		if m := r.Match; m != nil {
			if m.Source != "" && m.Source != req.Source {
				continue
			}

			if len(m.Sources) > 0 && !slices.Contains(m.Sources, req.Source) {
				continue
			}

			matched := true
			for _, cap := range m.Captures {
				s, e := exprString(cap.Value, ctx)
				if e != nil {
					matched = false
					break
				}

				parts := cap.re.FindStringSubmatch(s)
				if parts == nil {
					matched = false
					break
				}

				v := map[string]cty.Value{"value": cty.StringVal(parts[0])}
				for i, name := range cap.re.SubexpNames() {
					if i > 0 && name != "" {
						v[name] = cty.StringVal(parts[i])
					}
				}

				caps[cap.Name] = cty.ObjectVal(v)
				ctx.Variables["capture"] = cty.ObjectVal(caps)
			}

			if !matched {
				continue
			}

			if m.When != nil {
				v, d := m.When.Value(ctx)
				if d.HasErrors() || v.IsNull() || v.Type() != cty.Bool || !v.IsKnown() || !v.True() {
					continue
				}
			}
		}

		return r, ctx, nil
	}

	return nil, nil, &callError{404, "no matching route"}
}

type callError struct {
	code   int
	reason string
}

// Error returns the routing or capacity failure reason.
func (e *callError) Error() string {
	return e.reason
}

func statusError(e error) (int, string) {
	var ce *callError
	if errors.As(e, &ce) {
		return ce.code, ce.reason
	}

	if errors.Is(e, errUnauthorized) {
		return 403, "authentication failed"
	}

	return 502, "endpoint connection failed"
}

type usage struct{ calls, pending, connections, processes int }

// Engine owns protocol listeners, sessions, and shared resource accounting.
// Create it from a validated Config, then call Start and eventually Close.
type Engine struct {
	config *Config
	*engineState
}

// engineState is shared by immutable per-call configuration snapshots.
// Transports, accounting, and session ownership survive a configuration reload.
type engineState struct {
	current  atomic.Pointer[Engine]
	reloadMu sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	log      *slog.Logger
	mu       sync.Mutex
	usage    map[string]*usage
	closers  map[io.Closer]struct{}
	wg       sync.WaitGroup
	sip      map[string]*sipTransport
	webMu    sync.Mutex
	web      map[string]*webSession
}

// NewEngine creates an idle engine from a validated configuration.
// It opens no listeners until Start.
func NewEngine(c *Config) *Engine {
	ctx, cancel := context.WithCancel(context.Background())

	e := &Engine{config: c, engineState: &engineState{
		ctx:     ctx,
		cancel:  cancel,
		log:     slog.Default(),
		usage:   map[string]*usage{},
		closers: map[io.Closer]struct{}{},
		sip:     map[string]*sipTransport{},
		web:     map[string]*webSession{},
	}}
	e.current.Store(e)

	return e
}

func (e *Engine) setSIP(name string, t *sipTransport) {
	e.mu.Lock()
	e.sip[name] = t
	e.mu.Unlock()
}

func (e *Engine) getSIP(name string) *sipTransport {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.sip[name]
}

func (e *Engine) timeouts(n *Node) TimeoutConfig {
	t := TimeoutConfig{
		Connect:         "5s",
		Handshake:       "10s",
		Identity:        "5s",
		Dial:            "30s",
		MediaIdle:       "60s",
		MaxCallDuration: "2h",
		Shutdown:        "5s",
	}
	for _, v := range []*TimeoutConfig{e.config.Runtime.Timeouts, func() *TimeoutConfig {
		if n != nil {
			return n.Timeouts
		}

		return nil
	}()} {
		if v == nil {
			continue
		}

		if v.Connect != "" {
			t.Connect = v.Connect
		}

		if v.Handshake != "" {
			t.Handshake = v.Handshake
		}

		if v.Identity != "" {
			t.Identity = v.Identity
		}

		if v.Dial != "" {
			t.Dial = v.Dial
		}

		if v.MediaIdle != "" {
			t.MediaIdle = v.MediaIdle
		}

		if v.MaxCallDuration != "" {
			t.MaxCallDuration = v.MaxCallDuration
		}

		if v.Shutdown != "" {
			t.Shutdown = v.Shutdown
		}
	}

	return t
}

func (e *Engine) limits(n *Node) LimitConfig {
	l := LimitConfig{
		MaxCalls:           100,
		MaxPendingCalls:    20,
		MaxConnections:     200,
		MaxProcesses:       100,
		MaxIdentityBytes:   8192,
		MaxHTTPBodyBytes:   65536,
		MediaQueueDuration: "200ms",
	}
	for _, v := range []*LimitConfig{e.config.Runtime.Limits, func() *LimitConfig {
		if n != nil {
			return n.Limits
		}

		return nil
	}()} {
		if v == nil {
			continue
		}

		if v.MaxCalls > 0 {
			l.MaxCalls = v.MaxCalls
		}

		if v.MaxPendingCalls > 0 {
			l.MaxPendingCalls = v.MaxPendingCalls
		}

		if v.MaxConnections > 0 {
			l.MaxConnections = v.MaxConnections
		}

		if v.MaxProcesses > 0 {
			l.MaxProcesses = v.MaxProcesses
		}

		if v.MaxIdentityBytes > 0 {
			l.MaxIdentityBytes = v.MaxIdentityBytes
		}

		if v.MaxHTTPBodyBytes > 0 {
			l.MaxHTTPBodyBytes = v.MaxHTTPBodyBytes
		}

		if v.MediaQueueDuration != "" {
			l.MediaQueueDuration = v.MediaQueueDuration
		}
	}

	return l
}

func (e *Engine) depth(n *Node) int {
	d := duration(e.limits(n).MediaQueueDuration, 200*time.Millisecond)

	return max(1, int(d/(20*time.Millisecond)))
}

func (e *Engine) use(key string) *usage {
	u := e.usage[key]
	if u == nil {
		u = &usage{}
		e.usage[key] = u
	}

	return u
}

func (e *Engine) reserve(s, n *Node) (func(), func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	keys := []string{"global", "source:" + s.Name, "endpoint:" + n.Name}
	nodes := []*Node{nil, s, n}
	for i, key := range keys {
		u, l := e.use(key), e.limits(nodes[i])
		if u.calls >= l.MaxCalls ||
			u.pending >= l.MaxPendingCalls ||
			(n.Type == "exec" &&
				u.processes >= l.MaxProcesses) {
			return nil, nil, &callError{503, "capacity exhausted"}
		}
	}

	for _, key := range keys {
		u := e.use(key)
		u.calls++
		u.pending++
		if n.Type == "exec" {
			u.processes++
		}
	}

	pending := true
	var once sync.Once
	activate := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if pending {
			for _, key := range keys {
				e.use(key).pending--
			}

			pending = false
		}
	}
	release := func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			for _, key := range keys {
				u := e.use(key)
				u.calls--
				if pending {
					u.pending--
				}

				if n.Type == "exec" {
					u.processes--
				}
			}

			pending = false
		})
	}

	return activate, release, nil
}

func (e *Engine) connection(key string, n *Node) (func(), bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	g, u := e.use("global"), e.use("connection:"+key)
	if g.connections >= e.limits(nil).MaxConnections || u.connections >= e.limits(n).MaxConnections {
		return nil, false
	}

	g.connections++
	u.connections++
	var once sync.Once

	return func() {
		once.Do(func() {
			e.mu.Lock()
			g.connections--
			u.connections--
			e.mu.Unlock()
		})
	}, true
}

// Call owns a two-leg media bridge and its reserved capacity.
// Run manages media transfer and always closes both legs on completion.
type Call struct {
	e                 *Engine
	ctx               context.Context
	cancel            context.CancelFunc
	req               *Request
	source, endpoint  *Node
	in, out           Leg
	release, activate func()
	once              sync.Once
}

func (e *Engine) prepare(ctx context.Context, s *Node, req *Request, in Leg) (*Call, error) {
	r, eval, err := e.config.selectRoute(req)
	if err != nil {
		return nil, err
	}

	if r.Reject != nil {
		code := r.Reject.Code
		if code == 0 {
			code = 403
		}

		return nil, &callError{code, r.Reject.Reason}
	}

	n := e.config.endpoint(r.Connect.Endpoint)
	activate, release, err := e.reserve(s, n)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithCancel(ctx)
	if d := duration(e.timeouts(s).MaxCallDuration, 0); d > 0 {
		cancel()
		callCtx, cancel = context.WithTimeout(ctx, d)
	}

	setupDone := make(chan struct{})
	defer close(setupDone)
	var ended <-chan struct{}
	if leg, ok := in.(*channelLeg); ok {
		ended = leg.ended
	} else if leg, ok := in.(interface{ Done() <-chan struct{} }); ok {
		ended = leg.Done()
	}

	if ended != nil {
		go func() {
			select {
			case <-ended:
				cancel()
			case <-setupDone:
			case <-callCtx.Done():
			}
		}()
	}

	timeout := duration(r.Connect.Timeout, duration(e.timeouts(n).Dial, 30*time.Second))
	dialCtx, dialCancel := context.WithTimeout(callCtx, timeout)
	defer dialCancel()
	destination, err := exprString(r.Connect.Destination, eval)
	if err != nil {
		cancel()
		release()

		return nil, err
	}

	cr, err := e.config.outboundCredential(n, eval, req.Auth)
	if err != nil {
		cancel()
		release()

		return nil, err
	}

	startEarly := func(early Leg) (func(), error) {
		if earlyMediaMode(s) == "disabled" || earlyMediaMode(n) == "disabled" {
			return discardEarly(callCtx, early), nil
		}

		if err := compatibleFormats(s, n, in, early); err != nil {
			return nil, err
		}

		if req.earlyMedia != nil {
			if err := req.earlyMedia(); err != nil {
				return nil, err
			}
		}

		bridgeCtx, stop := context.WithCancel(callCtx)
		var wg sync.WaitGroup
		bridge := func(from, to Leg, sn, dn *Node) {
			defer wg.Done()
			if err := pump(bridgeCtx, from, to, sn, dn, 0); err != nil && bridgeCtx.Err() == nil {
				e.log.Debug("early media ended", "id", req.ID, "error", err)
				cancel()
			}
		}

		wg.Add(2)
		go bridge(in, early, s, n)
		go bridge(early, in, n, s)

		return func() {
			stop()
			// A transport writer may be blocked in an uncancellable socket
			// write. Bound the handoff rather than hanging call setup forever.
			closed := make(chan struct{})
			timer := time.AfterFunc(time.Second, func() {
				defer close(closed)
				cancel()
				in.Close()
			})
			wg.Wait()
			if !timer.Stop() {
				<-closed
			}
		}, nil
	}

	var out Leg
	switch n.Type {
	case "inproc":
		out, err = e.inProcessEndpoint(callCtx, dialCtx, n, req, eval,
			r.Connect, destination, in.Format(), startEarly)
	case "exec":
		out, err = e.execEndpoint(callCtx, n, req, eval, r.Connect, in.Format())
	case "audiosocket", "audiosocket_ws":
		out, err = e.audioEndpoint(callCtx, dialCtx, n, req, eval, r.Connect, cr, in.Format(), startEarly)
	case "sip":
		out, err = e.sipEndpoint(callCtx, dialCtx, n, destination, cr, startEarly)
	case "webrtc_http":
		out, err = e.webEndpoint(callCtx, dialCtx, n, cr)
	}

	if err != nil {
		cancel()
		release()

		return nil, err
	}

	if err := compatibleFormats(s, n, in, out); err != nil {
		out.Close()
		cancel()
		release()

		return nil, err
	}

	return &Call{
		e:        e,
		ctx:      callCtx,
		cancel:   cancel,
		req:      req,
		source:   s,
		endpoint: n,
		in:       in,
		out:      out,
		release:  release,
		activate: activate,
	}, nil
}

func compatibleFormats(source, endpoint *Node, in, out Leg) error {
	strict := source.Transcode != nil && source.Transcode.Mode == "never"
	strict = strict || endpoint.Transcode != nil && endpoint.Transcode.Mode == "never"
	if strict && in.Format() != out.Format() {
		return errors.New("incompatible formats with transcode never")
	}

	return nil
}

// Close cancels the bridge, closes both legs, and releases capacity exactly once.
func (c *Call) Close() {
	c.once.Do(func() {
		c.cancel()
		c.in.Close()
		c.out.Close()
		c.release()
	})
}

// Run bridges media until a leg ends, a deadline expires, or Close is called.
// It closes both legs and releases capacity before returning. Call Run once.
func (c *Call) Run() {
	defer c.Close()
	c.activate()
	c.e.log.Info(
		"call connected",
		"id",
		c.req.ID,
		"source",
		c.source.Name,
		"endpoint",
		c.endpoint.Name,
	)
	type result struct {
		leg string
		err error
	}
	errorsCh := make(chan result, 2)
	go func() {
		errorsCh <- result{
			"source",
			pump(c.ctx, c.in, c.out, c.source, c.endpoint, duration(c.e.timeouts(c.source).MediaIdle, 0)),
		}
	}()
	go func() {
		errorsCh <- result{
			"endpoint",
			pump(c.ctx, c.out, c.in, c.endpoint, c.source, duration(c.e.timeouts(c.endpoint).MediaIdle, 0)),
		}
	}()
	ended := <-errorsCh
	c.e.log.Debug(
		"call ended",
		"id",
		c.req.ID,
		"source",
		c.source.Name,
		"endpoint",
		c.endpoint.Name,
		"direction",
		ended.leg+" to peer",
		"error",
		ended.err,
	)
	c.Close()
	<-errorsCh
}

// beginWork admits externally dispatched work before shutdown starts waiting.
// The mutex pairs with Close's resource snapshot; accepted workers may add child
// workers while their own WaitGroup reference remains held.
func (e *Engine) beginWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ctx.Err() != nil {
		return false
	}

	e.wg.Add(1)

	return true
}

func (e *Engine) launch(c *Call) {
	if !e.beginWork() {
		c.Close()

		return
	}

	go func() {
		defer e.wg.Done()
		c.Run()
	}()
}

func (e *Engine) addCloser(c io.Closer) {
	e.mu.Lock()
	if e.ctx.Err() != nil {
		e.mu.Unlock()
		_ = c.Close()

		return
	}

	e.closers[c] = struct{}{}
	e.mu.Unlock()
}

func (e *Engine) removeCloser(c io.Closer) {
	e.mu.Lock()
	delete(e.closers, c)
	e.mu.Unlock()
}

// Done closes when the engine is canceled or a listener failure stops it.
// Call Close to release resources and wait for workers after Done closes.
func (e *Engine) Done() <-chan struct{} {
	return e.ctx.Done()
}

// Close cancels calls, closes listeners, and waits for all workers to exit.
// It has no overall deadline. Repeated calls are safe.
func (e *Engine) Close() error {
	e.cancel()
	e.mu.Lock()
	closers := make([]io.Closer, 0, len(e.closers))
	for c := range e.closers {
		closers = append(closers, c)
	}

	e.mu.Unlock()
	for _, c := range closers {
		_ = c.Close()
	}

	e.webMu.Lock()
	for _, s := range e.web {
		s.cancel()
	}

	e.webMu.Unlock()
	e.wg.Wait()

	return nil
}

// Serve starts the engine and waits until ctx is canceled or Close is called.
// It closes all resources before returning. Normal shutdown returns nil.
// Call Serve at most once, and do not also call Start on the same engine.
func (e *Engine) Serve(ctx context.Context) error {
	defer e.Close()
	if err := ctx.Err(); err != nil {
		return err
	}

	stop := context.AfterFunc(ctx, e.cancel)
	defer stop()
	if err := e.Start(); err != nil {
		return err
	}

	<-e.ctx.Done()

	return nil
}

// Start binds sources and starts registrations. Startup failure closes all
// resources already opened. Call Start at most once.
func (e *Engine) Start() error {
	groups := map[string][]*Node{}
	for _, s := range e.config.Sources {
		switch s.Type {
		case "audiosocket":
			if err := e.startAudio(s); err != nil {
				e.Close()

				return err
			}
		case "audiosocket_ws", "webrtc_http":
			groups[s.Bind] = append(groups[s.Bind], s)
		case "sip":
			t, err := e.startSIP(s)
			if err != nil {
				e.Close()

				return err
			}
			e.setSIP(s.Name, t)
		}
	}

	for addr, nodes := range groups {
		if err := e.startHTTP(addr, nodes); err != nil {
			e.Close()

			return err
		}
	}

	return nil
}

func (e *Engine) logFailure(req *Request, err error) {
	code, _ := statusError(err)
	e.log.Warn("call rejected", "id", req.ID, "source", req.Source, "status", code, "error", err)
}

func httpRequestInfo(r *http.Request) map[string]any {
	headers := map[string]any{}
	for k, v := range r.Header {
		name := http.CanonicalHeaderKey(k)
		if name != "Authorization" && name != "Proxy-Authorization" && name != "Cookie" {
			headers[name] = v
		}
	}

	query := map[string]any{}
	for k, v := range r.URL.Query() {
		if len(v) == 1 {
			query[k] = v[0]
		} else {
			query[k] = v
		}
	}

	return map[string]any{
		"path":    r.URL.Path,
		"host":    r.Host,
		"headers": headers,
		"query":   query,
	}
}

func splitAddress(s string) (string, int, error) {
	host, port, e := net.SplitHostPort(s)
	if e != nil {
		return "", 0, e
	}

	var p int
	_, e = fmt.Sscanf(port, "%d", &p)
	if e != nil || p < 0 || p > 65535 {
		return "", 0, fmt.Errorf("invalid port")
	}

	return host, p, nil
}
