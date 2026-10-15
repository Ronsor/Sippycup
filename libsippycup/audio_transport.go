package libsippycup

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/hcl/v2"
	"github.com/ronsor/sippycup/audiosocket"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

func (e *Engine) audioLeg(
	ctx context.Context,
	n *Node,
	c *audiosocket.Conn,
	onState ...func(audiosocket.CallState) error,
) *channelLeg {
	f := nodeFormat(n, Format{"pcm_s16le", 8000, 1})
	l := newChannelLeg(ctx, f, e.depth(n))
	l.closeFn = c.Close
	l.writeFn = func(ctx context.Context, f Frame) error {
		if f.Digit != 0 {
			return c.WriteDTMF(f.Digit)
		}

		return c.WriteAudio(f.Format.Rate, f.Data)
	}
	go func() {
		defer l.fail(io.EOF)
		var lastState audiosocket.CallState
		haveState := false
		for {
			p, err := c.ReadPacket()
			if err != nil {
				l.fail(err)

				return
			}

			if p.Kind == audiosocket.KindHangup {
				return
			}

			if p.Kind == audiosocket.KindError {
				l.fail(errors.New("AudioSocket peer reported error"))

				return
			}
			if p.Kind == audiosocket.KindState {
				state, _ := audiosocket.ParseState(p) // Conn has validated the payload.
				if haveState && state < lastState {
					l.fail(errors.New("AudioSocket call returned to early media after answering"))

					return
				}
				if !haveState || state != lastState {
					for _, notify := range onState {
						if err := notify(state); err != nil {
							l.fail(err)

							return
						}
					}
				}
				lastState, haveState = state, true

				continue
			}

			if rate, ok := audiosocket.AudioRate(p.Kind); ok {
				if len(onState) > 0 && !haveState {
					l.fail(errors.New("native AudioSocket audio arrived before call state"))

					return
				}

				if !l.emit(Frame{Format: Format{"pcm_s16le", rate, 1}, Data: p.Payload}) {
					return
				}
			} else if p.Kind == audiosocket.KindDTMF && receives(n, "audiosocket") {
				if !l.emit(Frame{Digit: p.Payload[0]}) {
					return
				}
			} else if p.Kind == audiosocket.KindID {
				l.fail(errors.New("duplicate AudioSocket identity"))

				return
			}
		}
	}()

	return l
}

func (e *Engine) readAudioIdentity(
	ctx context.Context,
	n *Node,
	c *audiosocket.Conn,
	httpAuth *AuthIdentity,
) (any, *AuthIdentity, error) {
	timer := time.AfterFunc(duration(e.timeouts(n).Identity, 5*time.Second), func() {
		c.Close()
	})
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() {
		c.Close()
	})
	defer stop()
	p, err := c.ReadPacket()
	if err != nil {
		return nil, nil, err
	}

	if p.Kind != audiosocket.KindID || len(p.Payload) > e.limits(n).MaxIdentityBytes {
		return nil, nil, errors.New("invalid AudioSocket identity")
	}

	id, err := audiosocket.ParseIdentity(p.Payload, n.Extended)
	if err != nil {
		return nil, nil, err
	}

	var metadata any = hex.EncodeToString(id.UUID[:])
	var supplied *AuthIdentity
	if len(id.JSON) > 0 {
		var obj map[string]any
		if json.Unmarshal(id.JSON, &obj) != nil {
			return nil, nil, errors.New("invalid JSON identity")
		}

		a, err := id.BasicAuth()
		if err != nil {
			return nil, nil, err
		}

		if a != nil {
			supplied = &AuthIdentity{Username: a.Username, Password: a.Password}
		}

		delete(obj, "basic_auth")
		metadata = obj
	}

	auth := httpAuth
	if n.Auth != nil && n.Auth.Mode != "none" {
		if supplied != nil && slices.Contains(n.Auth.Sources, "basic_auth") {
			if auth != nil &&
				(!equalSecret(auth.Username, supplied.Username) ||
					!equalSecret(auth.Password, supplied.Password)) {
				return nil, nil, errUnauthorized
			}

			auth = supplied
		}

		if err := e.config.verifyBasic(n.Auth, auth); err != nil {
			return nil, nil, err
		}
	}

	return metadata, auth, nil
}

func (e *Engine) handleAudio(n *Node, c *audiosocket.Conn, remote string, auth *AuthIdentity, info map[string]any) {
	defer c.Close()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	identity, auth, err := e.readAudioIdentity(ctx, n, c, auth)
	if err != nil {
		return
	}

	req := &Request{
		ID:       randomID(),
		Source:   n.Name,
		Remote:   remote,
		Identity: identity,
		Auth:     auth,
		HTTP:     info,
	}
	in := e.audioLeg(ctx, n, c)
	defer in.Close()
	if earlyMediaMode(n) == "native" {
		sent := false
		req.earlyMedia = func() error {
			if sent {
				return nil
			}

			err := c.WriteState(audiosocket.StateEarly)
			sent = err == nil

			return err
		}
	}
	call, err := e.prepare(ctx, n, req, in)
	if err != nil {
		e.logFailure(req, err)
		_ = c.WritePacket(audiosocket.Packet{Kind: audiosocket.KindError, Payload: []byte{1}})

		return
	}
	if earlyMediaMode(n) == "native" {
		if err := c.WriteState(audiosocket.StateAnswered); err != nil {
			call.Close()

			return
		}
	}

	call.Run()
}

func (e *Engine) startAudio(n *Node) error {
	raw, err := net.Listen("tcp", n.Bind)
	if err != nil {
		return err
	}

	var l net.Listener = &countedListener{raw, e, n, n.Bind}
	conf, err := makeTLS(n.TLS, true)
	if err != nil {
		raw.Close()

		return err
	}

	if conf != nil {
		l = tls.NewListener(l, conf)
	}

	e.addCloser(l)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			snapshot := e.active()
			node := snapshot.config.source(n.Name)
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				ac := audiosocket.Wrap(c, audiosocket.WithExtended(node.Extended))
				snapshot.handleAudio(node, ac, c.RemoteAddr().String(), nil, nil)
			}()
		}
	}()
	e.log.Info("AudioSocket listening", "address", l.Addr().String())

	return nil
}

func (e *Engine) serveAudioWS(n *Node, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != n.Path {
		http.NotFound(w, r)

		return
	}

	auth, ok := e.config.httpAuth(n, w, r)
	if !ok {
		return
	}

	if serveBrowser(n, w, r) {
		return
	}

	ws, err := websocket.Accept(
		w,
		r,
		&websocket.AcceptOptions{
			Subprotocols:       []string{audiosocket.Subprotocol},
			InsecureSkipVerify: true,
		},
	)
	if err != nil {
		return
	}

	e.wg.Add(1)
	defer e.wg.Done()
	e.handleAudio(
		n,
		audiosocket.WrapWebSocket(ws, audiosocket.WithExtended(n.Extended)),
		r.RemoteAddr,
		auth,
		httpRequestInfo(r),
	)
}

func (e *Engine) audioEndpoint(
	ctx, dialCtx context.Context,
	n *Node,
	req *Request,
	eval *hcl.EvalContext,
	action *Connect,
	cr *Credential,
	fallback Format,
	onEarly ...func(Leg) (func(), error),
) (Leg, error) {
	if cr != nil && cr.Password == nil {
		return nil, errors.New("AudioSocket authentication requires a plaintext password")
	}

	opts := []audiosocket.Option{
		audiosocket.WithExtended(n.Extended),
		audiosocket.WithContext(ctx),
	}
	headers := http.Header{}
	method := ""
	if n.Auth != nil {
		method = n.Auth.Method
	}

	if method == "" {
		if n.Type == "audiosocket_ws" {
			method = "http_basic"
		} else {
			method = "basic_auth"
		}
	}

	if cr != nil && method == "http_basic" {
		headers.Set(
			"Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(cr.Username+":"+*cr.Password)),
		)
	}

	var c *audiosocket.Conn
	var err error
	if n.Type == "audiosocket_ws" {
		client, err := httpClient(n, duration(e.timeouts(n).Connect, 5*time.Second))
		if err != nil {
			return nil, err
		}

		opts = append(opts, audiosocket.WithHTTPClient(client), audiosocket.WithHTTPHeader(headers))
		c, err = audiosocket.Dial(dialCtx, n.URL, opts...)
		if err != nil {
			return nil, err
		}
	} else {
		dial := &net.Dialer{Timeout: duration(e.timeouts(n).Connect, 5*time.Second)}
		if n.Bind != "" {
			addr, err := net.ResolveTCPAddr("tcp", n.Bind)
			if err != nil {
				return nil, err
			}

			dial.LocalAddr = addr
		}

		conf, err := makeTLS(n.TLS, false)
		if err != nil {
			return nil, err
		}

		var conn net.Conn
		address := strings.TrimPrefix(n.Address, "tcp://")
		if conf != nil {
			td := tls.Dialer{NetDialer: dial, Config: conf}
			conn, err = td.DialContext(dialCtx, "tcp", address)
		} else {
			conn, err = dial.DialContext(dialCtx, "tcp", address)
		}

		if err != nil {
			return nil, err
		}

		c = audiosocket.Wrap(conn, opts...)
	}

	stop := context.AfterFunc(dialCtx, func() {
		c.Close()
	})
	defer stop()
	id := audiosocket.Identity{}
	if n.UUID != "" {
		id.UUID, err = parseAudioUUID(n.UUID)
		if err != nil {
			c.Close()

			return nil, err
		}
	} else {
		_, _ = rand.Read(id.UUID[:])
		id.UUID[6] = id.UUID[6]&15 | 64
		id.UUID[8] = id.UUID[8]&63 | 128
	}
	if n.Extended {
		obj := map[string]any{"id": req.ID}
		if action.Identity != nil {
			v, d := action.Identity.Value(eval)
			if d.HasErrors() {
				c.Close()

				return nil, errors.New("identity expression failed")
			}

			b, err := ctyjson.Marshal(v, v.Type())
			if err != nil || json.Unmarshal(b, &obj) != nil || obj == nil {
				c.Close()

				return nil, errors.New("extended identity must be an object")
			}
		}

		// Reserve uuid for the same connection UUID used by standard AudioSocket.
		// Custom identity fields cannot replace it.
		obj["uuid"] = fmt.Sprintf("%x-%x-%x-%x-%x",
			id.UUID[:4], id.UUID[4:6], id.UUID[6:8], id.UUID[8:10], id.UUID[10:])

		if cr != nil && method == "basic_auth" {
			obj["basic_auth"] = audiosocket.BasicAuth{Username: cr.Username, Password: *cr.Password}
		}

		id.JSON, err = json.Marshal(obj)
		if err != nil {
			c.Close()

			return nil, err
		}
	}

	if err = c.WriteIdentity(id); err != nil {
		c.Close()

		return nil, err
	}

	if earlyMediaMode(n) != "native" {
		return e.audioLeg(ctx, n, c), nil
	}

	states := make(chan audiosocket.CallState, 2)
	leg := e.audioLeg(ctx, n, c, func(state audiosocket.CallState) error {
		states <- state // At most two distinct states; duplicates are filtered.

		return nil
	})
	stopEarly := func() {}
	defer func() { stopEarly() }()
	for {
		select {
		case state := <-states:
			if state == audiosocket.StateAnswered {
				return leg, nil
			}

			if len(onEarly) > 0 {
				stopEarly, err = onEarly[0](leg)
				if err != nil {
					stopEarly = func() {}
					leg.Close()

					return nil, err
				}
			} else {
				stopEarly = discardEarly(ctx, leg)
			}
		case <-leg.ended:
			leg.Close()

			return nil, errors.New("AudioSocket ended before answering")
		case <-dialCtx.Done():
			leg.Close()

			return nil, dialCtx.Err()
		}
	}
}
