// Package audiosocket implements Asterisk AudioSocket over TCP and WebSocket.
// Each WebSocket binary message contains exactly one complete AudioSocket packet.
// Extended mode permits a JSON object in KindID when the payload starts with '{',
// exceeds 16 bytes, and contains a uuid string. All 16-byte identities are binary UUIDs.
// KindState is an opt-in extension for peers that distinguish early and answered
// audio. WithExtended permits it but does not automatically send state messages.
package audiosocket

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	// KindHangup ends a session and has an empty payload.
	KindHangup byte = 0x00
	// KindID carries a binary UUID, or JSON in extended mode.
	KindID byte = 0x01
	// KindState carries one CallState byte in the opt-in extended signaling profile.
	// It is a sippycup extension, not part of the Asterisk AudioSocket protocol.
	KindState byte = 0x02
	// KindDTMF carries one ASCII digit.
	KindDTMF byte = 0x03
	// KindError carries a peer-defined error payload.
	KindError byte = 0xff
	// MaxPayload is the largest payload representable by the wire length field.
	MaxPayload = 65535
	// Subprotocol is the AudioSocket WebSocket subprotocol name.
	Subprotocol = "audiosocket"
)

// CallState distinguishes pre-answer audio from an answered call. Peers must
// explicitly agree to state signaling; enabling JSON identities alone does not
// require them to send these messages.
type CallState byte

const (
	// StateEarly starts early media while the downstream call remains pending.
	StateEarly CallState = 0
	// StateAnswered ends early media, or answers directly without an early phase.
	StateAnswered CallState = 1
)

// ParseState validates and decodes a KindState packet.
func ParseState(p Packet) (CallState, error) {
	if p.Kind != KindState || len(p.Payload) != 1 || p.Payload[0] > byte(StateAnswered) {
		return 0, errors.New("audiosocket: invalid call state")
	}

	return CallState(p.Payload[0]), nil
}

// Packet is an AudioSocket frame. Payload excludes the three-byte wire header.
// Read operations return payload storage owned by the caller.
type Packet struct {
	Kind    byte
	Payload []byte
}

// ReadPacket reads a single bounded, length-prefixed packet. The returned payload
// belongs to the caller. max <= 0 means the protocol maximum.
func ReadPacket(r io.Reader, max int) (Packet, error) {
	var h [3]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Packet{}, err
	}

	n := int(binary.BigEndian.Uint16(h[1:]))
	if max > 0 && n > max {
		return Packet{}, fmt.Errorf("audiosocket: payload %d exceeds limit %d", n, max)
	}

	p := Packet{Kind: h[0], Payload: make([]byte, n)}
	_, err := io.ReadFull(r, p.Payload)

	return p, err
}

// WritePacket writes one complete frame, handling short writes. It validates
// the wire length only; use Conn methods for identity and media validation.
func WritePacket(w io.Writer, p Packet) error {
	if len(p.Payload) > MaxPayload {
		return errors.New("audiosocket: payload exceeds 65535 bytes")
	}

	b := make([]byte, 3+len(p.Payload))
	b[0] = p.Kind
	binary.BigEndian.PutUint16(b[1:3], uint16(len(p.Payload)))
	copy(b[3:], p.Payload)
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}

		if n == 0 {
			return io.ErrShortWrite
		}

		b = b[n:]
	}

	return nil
}

var rates = [...]int{8000, 12000, 16000, 24000, 32000, 44100, 48000, 96000, 192000}

// AudioKind returns the packet kind for a supported mono PCM sample rate.
func AudioKind(rate int) (byte, error) {
	for i, r := range rates {
		if rate == r {
			return byte(0x10 + i), nil
		}
	}

	return 0, fmt.Errorf("audiosocket: unsupported sample rate %d", rate)
}

// AudioRate returns the PCM sample rate for an audio packet kind. The boolean
// is false for identity, DTMF, control, and unknown packet kinds.
func AudioRate(kind byte) (int, bool) {
	i := int(kind) - 0x10
	if i < 0 || i >= len(rates) {
		return 0, false
	}

	return rates[i], true
}

// BasicAuth carries credentials in an extended identity's basic_auth field.
// The password is plaintext; use a protected transport when sending credentials.
type BasicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Identity holds either a standard binary UUID or an extended JSON object.
// When JSON is nonempty, WriteIdentity sends it instead of UUID.
type Identity struct {
	UUID [16]byte
	JSON json.RawMessage
}

// ParseIdentity decodes a 16-byte binary UUID or, when extended is true, a JSON
// object longer than 16 bytes starting with '{' and containing a canonical UUID
// string in uuid. JSON data is copied and UUID is populated for either format.
func ParseIdentity(b []byte, extended bool) (Identity, error) {
	var id Identity
	if len(b) == 16 {
		copy(id.UUID[:], b)

		return id, nil
	}

	if !extended || len(b) <= 16 || b[0] != '{' {
		return id, errors.New("audiosocket: expected a 16-byte UUID or extended JSON identity")
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return id, fmt.Errorf("audiosocket: invalid JSON identity: %w", err)
	}

	var uuid string
	if err := json.Unmarshal(obj["uuid"], &uuid); err != nil || len(uuid) != 36 {
		return id, errors.New("audiosocket: JSON identity requires a uuid string in canonical format")
	}

	if uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' {
		return id, errors.New("audiosocket: invalid uuid format")
	}

	compact := uuid[:8] + uuid[9:13] + uuid[14:18] + uuid[19:23] + uuid[24:]
	if _, err := hex.Decode(id.UUID[:], []byte(compact)); err != nil {
		return Identity{}, fmt.Errorf("audiosocket: invalid uuid: %w", err)
	}

	id.JSON = append(json.RawMessage(nil), b...)

	return id, nil
}

// BasicAuth returns credentials from a JSON identity, or nil when absent.
// It does not verify the credentials.
func (id Identity) BasicAuth() (*BasicAuth, error) {
	if len(id.JSON) == 0 {
		return nil, nil
	}

	var obj struct {
		Auth *BasicAuth `json:"basic_auth"`
	}
	if err := json.Unmarshal(id.JSON, &obj); err != nil {
		return nil, err
	}

	return obj.Auth, nil
}

// Option configures an AudioSocket connection or listener.
type Option func(*options)

type options struct {
	extended   bool
	maxPayload int
	tls        *tls.Config
	header     http.Header
	client     *http.Client
	ctx        context.Context
}

// WithExtended enables JSON identities alongside standard UUIDs. It is off
// by default and does not change the three-byte AudioSocket frame header.
func WithExtended(v bool) Option {
	return func(o *options) {
		o.extended = v
	}
}

// WithMaxPayload bounds received and written frame payloads. Values outside
// 1 through MaxPayload select the protocol maximum.
func WithMaxPayload(n int) Option {
	return func(o *options) {
		o.maxPayload = n
	}
}

// WithTLS supplies TLS settings for tls and wss transports. For outgoing
// WebSockets, an explicit WithHTTPClient takes precedence. The configuration
// is not cloned and must not be mutated while in use.
func WithTLS(c *tls.Config) Option {
	return func(o *options) {
		o.tls = c
	}
}

// WithHTTPHeader sets headers on an outgoing WebSocket handshake. Headers
// are cloned when the option is applied; servers ignore this option.
func WithHTTPHeader(h http.Header) Option {
	return func(o *options) {
		o.header = h.Clone()
	}
}

// WithHTTPClient selects the HTTP client for outgoing WebSocket handshakes.
// It has no effect on TCP connections or listeners.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) {
		o.client = c
	}
}

// WithContext sets the lifetime context for connections and listener accepts.
// Without it, connections use context.Background. Dial's context controls
// connection establishment separately. Call Close to release owned resources,
// including listener sockets, even after cancellation.
func WithContext(ctx context.Context) Option {
	return func(o *options) {
		o.ctx = ctx
	}
}

func getOptions(opts []Option) options {
	o := options{maxPayload: MaxPayload, ctx: context.Background()}
	for _, f := range opts {
		f(&o)
	}

	if o.maxPayload <= 0 || o.maxPayload > MaxPayload {
		o.maxPayload = MaxPayload
	}

	return o
}

// MessageConn is satisfied by *websocket.Conn. Reads must yield complete messages.
type MessageConn interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
}

// Conn permits one reader and concurrent serialized writers. Close interrupts I/O.
// Wrapping a connection transfers ownership: Close closes the underlying transport.
type Conn struct {
	stream      net.Conn
	ws          MessageConn
	opts        options
	ctx         context.Context
	cancel      context.CancelFunc
	writeMu     sync.Mutex
	closeOnce   sync.Once
	stopContext func() bool
}

// Wrap takes ownership of a stream connection and adds AudioSocket framing.
// It attempts to enable TCP_NODELAY, including through TLS and wrappers exposing
// NetConn. Unsupported transports and socket-option errors are ignored.
// It does not read or write an identity automatically.
func Wrap(c net.Conn, opts ...Option) *Conn {
	setNoDelay(c)
	o := getOptions(opts)
	ctx, cancel := context.WithCancel(o.ctx)
	conn := &Conn{stream: c, opts: o, ctx: ctx, cancel: cancel}
	conn.stopContext = context.AfterFunc(ctx, func() {
		_ = c.Close()
	})

	return conn
}

// WrapWebSocket takes ownership of a message connection. Each binary message
// must contain exactly one AudioSocket frame. It sets the read limit when
// wrapping a coder/websocket connection.
func WrapWebSocket(c MessageConn, opts ...Option) *Conn {
	setNoDelay(c)
	o := getOptions(opts)
	ctx, cancel := context.WithCancel(o.ctx)
	if w, ok := c.(*websocket.Conn); ok {
		w.SetReadLimit(int64(o.maxPayload + 3))
	}

	return &Conn{ws: c, opts: o, ctx: ctx, cancel: cancel}
}

// setNoDelay uses optional transport capabilities without requiring TCP. The
// depth bound protects against connection wrappers that return themselves.
// Standard Go TCP sockets already default to no-delay; opaque WebSocket
// transports keep the socket settings selected by their HTTP implementation.
func setNoDelay(conn any) {
	for range 16 {
		if socket, ok := conn.(interface{ SetNoDelay(bool) error }); ok {
			_ = socket.SetNoDelay(true)

			return
		}

		wrapper, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			return
		}

		conn = wrapper.NetConn()
	}
}

// ReadPacket reads and validates one frame. It validates known identity,
// DTMF, audio, and hangup payloads; unknown packet kinds are returned unchanged.
// Only one goroutine may read at a time.
func (c *Conn) ReadPacket() (Packet, error) {
	var p Packet
	var err error
	if c.stream != nil {
		p, err = ReadPacket(c.stream, c.opts.maxPayload)
	} else {
		typ, b, e := c.ws.Read(c.ctx)
		if e != nil {
			return Packet{}, e
		}

		if typ != websocket.MessageBinary || len(b) < 3 {
			return Packet{}, errors.New("audiosocket: expected binary packet")
		}

		n := int(binary.BigEndian.Uint16(b[1:3]))
		if n != len(b)-3 || n > c.opts.maxPayload {
			return Packet{}, errors.New("audiosocket: invalid message length")
		}

		p = Packet{Kind: b[0], Payload: b[3:]}
	}

	if err != nil {
		return p, err
	}

	if p.Kind == KindID {
		_, err = ParseIdentity(p.Payload, c.opts.extended)
	}
	if p.Kind == KindState {
		if !c.opts.extended {
			return p, errors.New("audiosocket: call state requires extended mode")
		}

		_, err = ParseState(p)
	}

	if p.Kind == KindDTMF && (len(p.Payload) != 1 || !strings.ContainsRune("0123456789*#ABCD", rune(p.Payload[0]))) {
		err = errors.New("audiosocket: invalid DTMF")
	}

	if _, ok := AudioRate(p.Kind); ok && len(p.Payload)%2 != 0 {
		err = errors.New("audiosocket: odd PCM payload length")
	}

	if p.Kind == KindHangup && len(p.Payload) != 0 {
		err = errors.New("audiosocket: hangup must be empty")
	}

	return p, err
}

// WritePacket serializes a frame and enforces payload limits and identity
// validation. Use WriteAudio and WriteDTMF to validate those payload types.
func (c *Conn) WritePacket(p Packet) error {
	if len(p.Payload) > MaxPayload || len(p.Payload) > c.opts.maxPayload {
		return errors.New("audiosocket: payload too large")
	}

	if p.Kind == KindID {
		if _, err := ParseIdentity(p.Payload, c.opts.extended); err != nil {
			return err
		}
	}
	if p.Kind == KindState {
		if !c.opts.extended {
			return errors.New("audiosocket: call state requires extended mode")
		}

		if _, err := ParseState(p); err != nil {
			return err
		}
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stream != nil {
		return WritePacket(c.stream, p)
	}

	b := make([]byte, 3+len(p.Payload))
	b[0] = p.Kind
	binary.BigEndian.PutUint16(b[1:3], uint16(len(p.Payload)))
	copy(b[3:], p.Payload)

	return c.ws.Write(c.ctx, websocket.MessageBinary, b)
}

// WriteState sends an extended call-state message. Native-signaling servers
// send StateEarly before early audio and StateAnswered before answered audio.
// Sending StateAnswered directly is valid; transitioning back to early is not.
func (c *Conn) WriteState(state CallState) error {
	return c.WritePacket(Packet{Kind: KindState, Payload: []byte{byte(state)}})
}

// WriteIdentity sends JSON when present, otherwise the 16-byte UUID. Sending
// JSON requires WithExtended(true), a leading brace, more than 16 bytes, and a
// valid uuid string. Protocol users should send identity first.
func (c *Conn) WriteIdentity(id Identity) error {
	b := id.UUID[:]
	if len(id.JSON) > 0 {
		if len(id.JSON) <= 16 || id.JSON[0] != '{' {
			return errors.New("audiosocket: JSON identity must start with '{' and exceed 16 bytes")
		}

		b = id.JSON
	}

	return c.WritePacket(Packet{Kind: KindID, Payload: b})
}

// WriteAudio sends mono signed 16-bit little-endian PCM at the given rate.
// The payload must contain whole samples and fit in a single frame.
func (c *Conn) WriteAudio(rate int, pcm []byte) error {
	k, e := AudioKind(rate)
	if e != nil {
		return e
	}

	if len(pcm)%2 != 0 {
		return errors.New("audiosocket: odd PCM payload length")
	}

	return c.WritePacket(Packet{Kind: k, Payload: pcm})
}

// WriteDTMF sends one digit: 0-9, *, #, or A-D.
func (c *Conn) WriteDTMF(d byte) error {
	if !strings.ContainsRune("0123456789*#ABCD", rune(d)) {
		return errors.New("audiosocket: invalid digit")
	}

	return c.WritePacket(Packet{Kind: KindDTMF, Payload: []byte{d}})
}

// SetDeadline sets stream read and write deadlines. A zero time clears them.
// WebSocket connections return an error; configure their lifetime with
// WithContext instead.
func (c *Conn) SetDeadline(t time.Time) error {
	if c.stream != nil {
		return c.stream.SetDeadline(t)
	}

	return errors.New("audiosocket: WebSocket deadlines use context")
}

// Close interrupts I/O and closes the underlying transport. It is safe to
// call concurrently and repeatedly. It does not send an AudioSocket hangup frame.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.stopContext != nil {
			c.stopContext()
		}

		c.cancel()
		if c.stream != nil {
			err = c.stream.Close()
		} else {
			err = c.ws.Close(websocket.StatusNormalClosure, "")
		}
	})

	return err
}

// Dial establishes tcp://, tls://, ws://, or wss:// connections. The address
// must include a host and appropriate port; WebSocket addresses may have paths.
// The context bounds dialing, not the returned connection lifetime; use
// WithContext for that. Call WriteIdentity after dialing.
func Dial(ctx context.Context, address string, opts ...Option) (*Conn, error) {
	o := getOptions(opts)
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}

	switch u.Scheme {
	case "tcp", "tls":
		d := net.Dialer{}
		var c net.Conn
		if u.Scheme == "tls" {
			td := tls.Dialer{NetDialer: &d, Config: o.tls}
			c, err = td.DialContext(ctx, "tcp", u.Host)
		} else {
			c, err = d.DialContext(ctx, "tcp", u.Host)
		}
		if err != nil {
			return nil, err
		}
		return Wrap(c, opts...), nil
	case "ws", "wss":
		client := o.client
		if client == nil && o.tls != nil {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = o.tls
			client = &http.Client{Transport: tr}
		}
		w, _, e := websocket.Dial(
			ctx,
			address,
			&websocket.DialOptions{
				HTTPHeader:   o.header,
				HTTPClient:   client,
				Subprotocols: []string{Subprotocol},
			},
		)
		if e != nil {
			return nil, e
		}
		return WrapWebSocket(w, opts...), nil
	default:
		return nil, fmt.Errorf("audiosocket: unsupported address scheme %q", u.Scheme)
	}
}

// Handler mounts AudioSocket on an existing HTTP server. The callback owns conn.
// Authenticate requests in middleware before calling this handler if desired.
func Handler(accept func(*Conn, *http.Request), opts ...Option) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}

		accept(WrapWebSocket(c, opts...), r)
	})
}

// Listener accepts framed AudioSocket connections over TCP, TLS, or WebSocket.
// Accepted connections belong to the caller and survive listener closure.
type Listener struct {
	raw      net.Listener
	server   *http.Server
	incoming chan *Conn
	ctx      context.Context
	cancel   context.CancelFunc
	opts     []Option
	once     sync.Once
}

// Listen binds a tcp://, tls://, ws://, or wss:// address. Port zero selects
// an available port. TLS listeners require WithTLS with a server certificate.
// WebSocket addresses select a path, defaulting to /. Close releases the listener.
func Listen(address string, opts ...Option) (*Listener, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}

	o := getOptions(opts)
	if u.Scheme != "tcp" && u.Scheme != "tls" && u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("audiosocket: unsupported scheme %q", u.Scheme)
	}

	raw, err := net.Listen("tcp", u.Host)
	if err != nil {
		return nil, err
	}

	if u.Scheme == "tls" || u.Scheme == "wss" {
		if o.tls == nil {
			raw.Close()

			return nil, errors.New("audiosocket: TLS configuration required")
		}

		raw = tls.NewListener(raw, o.tls)
	}

	ctx, cancel := context.WithCancel(o.ctx)
	l := &Listener{
		raw:      raw,
		ctx:      ctx,
		cancel:   cancel,
		opts:     opts,
		incoming: make(chan *Conn),
	}
	if u.Scheme == "ws" || u.Scheme == "wss" {
		path := u.Path
		if path == "" {
			path = "/"
		}

		mux := http.NewServeMux()
		mux.Handle(path, Handler(func(c *Conn, r *http.Request) {
			select {
			case l.incoming <- c:
			case <-ctx.Done():
				c.Close()
			}
		}, opts...))
		l.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			_ = l.server.Serve(raw)
			cancel()
		}()
	} else {
		go func() {
			for {
				c, e := raw.Accept()
				if e != nil {
					cancel()

					return
				}

				ac := Wrap(c, opts...)
				select {
				case l.incoming <- ac:
				case <-ctx.Done():
					ac.Close()
					return
				}
			}
		}()
	}

	return l, nil
}

// Addr returns the actual bound address, including an automatically chosen port.
func (l *Listener) Addr() net.Addr {
	return l.raw.Addr()
}

// Accept waits for a connection, context cancellation, or listener closure.
// It does not consume the peer's identity packet. Concurrent calls are supported.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close stops accepting connections and releases the listening socket.
// It is safe to call repeatedly; accepted connections must be closed separately.
func (l *Listener) Close() error {
	var e error
	l.once.Do(func() {
		l.cancel()
		if l.server != nil {
			e = l.server.Close()
		} else {
			e = l.raw.Close()
		}
	})

	return e
}
