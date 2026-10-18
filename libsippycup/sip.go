package libsippycup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

type sipTransport struct {
	engine     *Engine
	node       *Node
	ua         *sipgo.UserAgent
	server     *sipgo.Server
	client     *sipgo.Client
	servers    map[string]*sipgo.DialogServerCache
	clients    map[string]*sipgo.DialogClientCache
	contacts   map[string]sip.ContactHeader
	mu         sync.Mutex
	legs       map[string]*channelLeg
	usedNonces map[string]map[string]time.Time
	nonceKey   [32]byte
	nonceOnce  sync.Once
	closers    []io.Closer
	closeOnce  sync.Once
}

type sipNonce struct {
	realm, algorithm string
	expires          time.Time
}

type readyPacketConn struct {
	net.PacketConn
	once  sync.Once
	ready chan struct{}
}

// ReadFrom signals listener registration before reading a datagram.
func (c *readyPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.once.Do(func() {
		close(c.ready)
	})

	return c.PacketConn.ReadFrom(b)
}

type readyListener struct {
	net.Listener
	once  sync.Once
	ready chan struct{}
}

// Accept signals listener readiness before waiting for a connection.
func (l *readyListener) Accept() (net.Conn, error) {
	l.once.Do(func() {
		close(l.ready)
	})

	return l.Listener.Accept()
}

// Close releases signaling listeners and the SIP user agent exactly once.
func (t *sipTransport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		for _, c := range t.closers {
			_ = c.Close()
		}

		err = t.ua.Close()
	})

	return err
}

func localIP(bind, remote string) (net.IP, error) {
	if ip := net.ParseIP(bind); ip != nil && !ip.IsUnspecified() {
		return ip, nil
	}

	host := remote
	if h, _, e := net.SplitHostPort(remote); e == nil {
		host = h
	}

	if host == "" {
		host = "192.0.2.1"
	}

	conn, e := net.Dial("udp", net.JoinHostPort(host, "9"))
	if e != nil {
		return nil, e
	}

	defer conn.Close()

	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}

func advertisedIP(bind, remote string, a *AdvertiseConfig) (net.IP, error) {
	if a != nil {
		switch a.Mode {
		case "explicit":
			ip := net.ParseIP(a.Address)
			if ip == nil {
				return nil, errors.New("RTP advertised address must be an IP")
			}
			return ip, nil
		case "bind":
			ip := net.ParseIP(bind)
			if ip == nil || ip.IsUnspecified() {
				return nil, errors.New("advertise bind requires concrete IP")
			}
			return ip, nil
		case "", "auto":
		default:
			return nil, errors.New("invalid advertise mode")
		}
	}

	return localIP(bind, remote)
}

func sipCodecs(n *Node) []sipCodec {
	names := n.Codecs
	if len(names) == 0 {
		names = []string{"pcmu", "pcma"}
	}

	if n.Transcode != nil && n.Transcode.Codec != "" {
		names = []string{n.Transcode.Codec}
	}

	codecs := []sipCodec{}
	for _, name := range names {
		switch name {
		case "pcmu":
			codecs = append(codecs, codecPCMU)
		case "pcma":
			codecs = append(codecs, codecPCMA)
		case "opus":
			codecs = append(codecs, codecOpus)
		}
	}

	if receives(n, "rfc4733") || sends(n) == "rfc4733" {
		codecs = append(codecs, codecTelephoneEvent)
	}

	return codecs
}

func (e *Engine) startSIP(n *Node) (*sipTransport, error) {
	conf, err := makeTLS(n.TLS, e.config.source(n.Name) == n)
	if err != nil {
		return nil, err
	}

	if conf == nil && e.config.source(n.Name) != n {
		conf = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sippycup"), sipgo.WithUserAgenTLSConfig(conf))
	if err != nil {
		return nil, err
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		ua.Close()

		return nil, err
	}

	t := &sipTransport{
		engine:     e,
		node:       n,
		ua:         ua,
		server:     srv,
		servers:    map[string]*sipgo.DialogServerCache{},
		clients:    map[string]*sipgo.DialogClientCache{},
		contacts:   map[string]sip.ContactHeader{},
		legs:       map[string]*channelLeg{},
		usedNonces: map[string]map[string]time.Time{},
	}
	bind := n.Bind
	if bind == "" {
		bind = "0.0.0.0:0"
	}

	host, port, err := splitAddress(bind)
	if err != nil {
		t.Close()

		return nil, err
	}

	if host == "" {
		host = "0.0.0.0"
	}

	if net.ParseIP(host) == nil {
		resolved, err := net.ResolveIPAddr("ip", host)
		if err != nil {
			t.Close()

			return nil, err
		}

		host = resolved.IP.String()
	}

	advHost := host
	advPort := port
	if n.Advertise != nil && n.Advertise.Mode == "explicit" {
		advHost, advPort, err = splitAddress(n.Advertise.Address)
		if err != nil {
			t.Close()

			return nil, err
		}
	} else {
		if n.Advertise != nil &&
			n.Advertise.Mode == "bind" &&
			(net.ParseIP(host) == nil ||
				net.ParseIP(host).IsUnspecified()) {
			t.Close()

			return nil, errors.New("SIP advertise bind requires concrete IP")
		}

		ip, err := localIP(host, "")
		if err != nil {
			t.Close()

			return nil, err
		}

		advHost = ip.String()
	}

	transports := n.Transports
	if len(transports) == 0 {
		transport := sipNetwork(n)
		transports = []string{transport}
	}

	type bound struct {
		kind string
		udp  net.PacketConn
		tcp  net.Listener
	}
	var sockets []bound
	for _, kind := range transports {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		switch kind {
		case "udp":
			l, err := net.ListenPacket("udp", addr)
			if err != nil {
				t.Close()

				return nil, err
			}
			if port == 0 {
				_, port, _ = splitAddress(l.LocalAddr().String())
			}
			t.closers = append(t.closers, l)
			sockets = append(sockets, bound{kind: kind, udp: l})
		case "tcp", "tls":
			l, err := net.Listen("tcp", addr)
			if err != nil {
				t.Close()

				return nil, err
			}
			if port == 0 {
				_, port, _ = splitAddress(l.Addr().String())
			}
			var listener net.Listener = &countedListener{l, e, n, bind}
			if kind == "tls" {
				if conf == nil {
					l.Close()
					t.Close()

					return nil, errors.New("SIP TLS requires TLS configuration")
				}

				listener = tls.NewListener(listener, conf)
			}
			t.closers = append(t.closers, listener)
			sockets = append(sockets, bound{kind: kind, tcp: listener})
		default:
			t.Close()
			return nil, fmt.Errorf("unsupported SIP transport %q", kind)
		}
	}

	if advPort == 0 {
		advPort = port
	}

	for _, kind := range transports {
		localPort := port
		if kind != "udp" {
			localPort = 0
		}

		connectionAddr := net.JoinHostPort(host, strconv.Itoa(localPort))
		if kind == "udp" {
			for _, socket := range sockets {
				if socket.udp != nil {
					// sipgo indexes listeners by their actual local address. Go
					// may normalize an IPv4 wildcard bind to a dual-stack [::]
					// socket; the configured spelling would miss that entry.
					connectionAddr = socket.udp.LocalAddr().String()
					break
				}
			}
		}

		client, err := sipgo.NewClient(
			ua,
			sipgo.WithClientHostname(advHost),
			sipgo.WithClientPort(advPort),
			sipgo.WithClientConnectionAddr(connectionAddr),
			sipgo.WithClientNAT(),
		)
		if err != nil {
			t.Close()

			return nil, err
		}

		if t.client == nil || kind == sipNetwork(n) {
			t.client = client
		}

		uri := sip.Uri{
			Scheme:    "sip",
			Host:      advHost,
			Port:      advPort,
			UriParams: sip.NewParams(),
		}
		uri.UriParams.Add("transport", kind)
		contact := sip.ContactHeader{Address: uri}
		t.contacts[kind] = contact
		t.servers[kind] = sipgo.NewDialogServerCache(client, contact)
		t.clients[kind] = sipgo.NewDialogClientCache(client, contact)
	}

	srv.OnInvite(t.invite)
	srv.OnAck(func(r *sip.Request, tx sip.ServerTransaction) {
		if cache := t.servers[strings.ToLower(r.Transport())]; cache != nil {
			_ = cache.ReadAck(r, tx)
		}
	})
	srv.OnBye(func(r *sip.Request, tx sip.ServerTransaction) {
		kind := strings.ToLower(r.Transport())
		if cache := t.servers[kind]; cache != nil {
			if cache.ReadBye(r, tx) == nil {
				e.log.Debug("SIP remote BYE received", "node", n.Name, "call_id", r.CallID().Value())

				return
			}
		}

		if cache := t.clients[kind]; cache != nil {
			if cache.ReadBye(r, tx) == nil {
				e.log.Debug("SIP remote BYE received", "node", n.Name, "call_id", r.CallID().Value())

				return
			}
		}

		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 481, "Call Does Not Exist", nil))
	})
	srv.OnOptions(func(r *sip.Request, tx sip.ServerTransaction) {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 200, "OK", nil))
	})
	srv.OnInfo(t.info)
	srv.OnCancel(func(r *sip.Request, tx sip.ServerTransaction) {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 481, "Call Does Not Exist", nil))
	})
	for _, b := range sockets {
		b := b
		ready := make(chan struct{})
		if b.udp != nil {
			b.udp = &readyPacketConn{PacketConn: b.udp, ready: ready}
		} else {
			b.tcp = &readyListener{Listener: b.tcp, ready: ready}
		}

		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			var err error
			switch b.kind {
			case "udp":
				err = srv.ServeUDP(b.udp)
			case "tcp":
				err = srv.ServeTCP(b.tcp)
			case "tls":
				err = srv.ServeTLS(b.tcp)
			}

			if err != nil && e.ctx.Err() == nil {
				e.log.Debug("SIP transport stopped", "error", err)
			}
		}()
		select {
		case <-ready:
		case <-e.ctx.Done():
			t.Close()
			return nil, e.ctx.Err()
		}
	}

	e.addCloser(t)
	if e.config.source(n.Name) == n {
		e.setSIP(n.Name, t)
	}

	if n.Registration != nil {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			t.registerLoop()
		}()
	}

	e.log.Info(
		"SIP listening",
		"source",
		n.Name,
		"bind",
		net.JoinHostPort(host, strconv.Itoa(port)),
	)

	return t, nil
}

func sipNetwork(n *Node) string {
	if n.Transport != "" {
		return n.Transport
	}

	if len(n.Transports) > 0 && n.OutboundProxy == "" {
		return n.Transports[0]
	}

	uri := n.URI
	if n.Registration != nil {
		uri = n.Registration.Registrar
	}

	if n.OutboundProxy != "" {
		uri = n.OutboundProxy
	}

	var u sip.Uri
	if sip.ParseUri(uri, &u) == nil {
		if u.Scheme == "sips" {
			return "tls"
		}

		if transport, ok := u.UriParams.Get("transport"); ok {
			return strings.ToLower(transport)
		}
	}

	return "udp"
}

func (t *sipTransport) authorize(e *Engine, n *Node, r *sip.Request, tx sip.ServerTransaction) (*AuthIdentity, bool) {
	a := n.Auth
	if a == nil || a.Mode == "none" {
		return nil, true
	}

	set := e.config.credentialSet(a.CredentialSet)
	challenge := func() {
		res := sip.NewResponseFromRequest(r, 401, "Unauthorized", nil)
		algs := map[string]bool{}
		for _, name := range set.Credentials {
			cr := e.config.credential(name)
			if cr.Digest != nil {
				algs[strings.ToUpper(cr.Digest.Algorithm)] = true
			} else {
				algs["SHA-256"] = true
				algs["MD5"] = true
			}
		}

		for _, alg := range []string{"SHA-256", "MD5"} {
			if !algs[alg] {
				continue
			}

			nonce := t.issueNonce(realm(a), alg, r.Source(), time.Now())
			ch := digest.Challenge{
				Realm:     realm(a),
				Nonce:     nonce,
				Algorithm: alg,
				QOP:       []string{"auth"},
			}
			res.AppendHeader(sip.NewHeader("WWW-Authenticate", ch.String()))
		}

		_ = respondSIP(tx, res)
	}
	h := r.GetHeader("Authorization")
	if h == nil {
		challenge()

		return nil, false
	}

	got, err := digest.ParseCredentials(h.Value())
	if err != nil {
		challenge()

		return nil, false
	}

	nonce, ok := t.verifyNonce(got.Nonce, realm(a), got.Algorithm, r.Source(), time.Now())
	if !ok ||
		time.Now().After(nonce.expires) ||
		got.Realm != nonce.realm ||
		!strings.EqualFold(got.Algorithm, nonce.algorithm) ||
		got.URI != r.Recipient.Addr() ||
		got.QOP != "auth" ||
		got.Nc < 1 ||
		got.Cnonce == "" {
		challenge()

		return nil, false
	}

	for _, name := range set.Credentials {
		cr := e.config.credential(name)
		if cr.Username != got.Username {
			continue
		}

		opts := digest.Options{
			Method:   string(r.Method),
			URI:      got.URI,
			Username: cr.Username,
			Cnonce:   got.Cnonce,
			Count:    got.Nc,
		}
		if cr.Password != nil {
			opts.Password = *cr.Password
		} else if cr.Digest != nil && strings.EqualFold(cr.Digest.Algorithm, nonce.algorithm) {
			opts.A1 = cr.Digest.HA1
		} else {
			continue
		}

		want, err := digest.Digest(
			&digest.Challenge{
				Realm:     nonce.realm,
				Nonce:     got.Nonce,
				Algorithm: nonce.algorithm,
				QOP:       []string{"auth"},
			},
			opts,
		)
		if err == nil && equalSecret(want.Response, got.Response) {
			ok = t.consumeNonce(cr.Username, got.Nonce, nonce.expires, time.Now())
			if ok {
				return &AuthIdentity{Username: cr.Username, Verified: true, DigestOnly: true}, true
			}
		}
	}

	_ = respondSIP(tx, sip.NewResponseFromRequest(r, 403, "Forbidden", nil))

	return nil, false
}

func (t *sipTransport) invite(r *sip.Request, tx sip.ServerTransaction) {
	if !t.engine.beginWork() {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 503, "Shutting Down", nil))
		return
	}

	defer t.engine.wg.Done()

	if t.engine.config.source(t.node.Name) != t.node {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 403, "Inbound Calls Disabled", nil))

		return
	}

	kind := strings.ToLower(r.Transport())
	cache := t.servers[kind]
	if cache == nil {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 503, "Unavailable", nil))

		return
	}

	if r.To() == nil || r.From() == nil {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 400, "Bad Request", nil))

		return
	}

	if _, ok := r.To().Params.Get("tag"); ok {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 488, "Renegotiation Not Supported", nil))

		return
	}

	e := t.engine.active()
	n := e.config.source(t.node.Name)
	auth, ok := t.authorize(e, n, r, tx)
	if !ok {
		return
	}

	d, err := cache.ReadInvite(r, tx)
	if err != nil {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 400, "Bad Request", nil))

		return
	}

	defer d.Close()
	_ = d.Respond(100, "Trying", nil)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	stop := context.AfterFunc(d.Context(), cancel)
	defer stop()
	sess, err := newSIPMediaContext(ctx, n, r.Source())
	if err != nil {
		_ = d.Respond(503, "Media Unavailable", nil)

		return
	}

	defer sess.Close()
	if err = sess.RemoteSDP(r.Body()); err != nil {
		_ = d.Respond(488, "Unsupported Media", nil)

		return
	}

	headers := map[string]any{}
	for _, h := range r.Headers() {
		name := strings.ToLower(h.Name())
		if name != "authorization" && name != "proxy-authorization" {
			headers[name] = h.Value()
		}
	}

	req := &Request{
		ID:     randomID(),
		Source: n.Name,
		Remote: r.Source(),
		Auth:   auth,
		SIP: map[string]any{
			"request_uri": map[string]any{"user": r.Recipient.User, "host": r.Recipient.Host},
			"from":        r.From().Address.User,
			"to":          r.To().Address.User,
			"headers":     headers,
		},
	}
	remoteTarget := r.From().Address
	if contact := r.Contact(); contact != nil {
		remoteTarget = contact.Address
	}

	in := t.makeLeg(e, ctx, n, sess, d.ID, func(ctx context.Context) error {
		if d.LoadState() < sip.DialogStateEstablished {
			return nil
		}

		return d.Bye(ctx)
	}, d.Do, remoteTarget, d.Context(), false)
	defer in.Close()
	var progressSent bool
	var answeredEarly bool
	req.earlyMedia = func() error {
		if progressSent {
			return nil
		}

		var err error
		if earlyMediaMode(n) == "answer" {
			err = d.RespondSDP(sess.LocalSDP())
			answeredEarly = err == nil
		} else {
			err = d.Respond(183, "Session Progress", sess.LocalSDP(), sip.NewHeader("Content-Type", "application/sdp"))
		}
		progressSent = err == nil

		return err
	}

	call, err := e.prepare(ctx, n, req, in)
	if err != nil {
		code, reason := statusError(err)
		if !answeredEarly {
			_ = d.Respond(code, reason, nil)
		}
		e.logFailure(req, err)

		return
	}

	defer call.Close()
	if !answeredEarly {
		if err = d.RespondSDP(sess.LocalSDP()); err != nil {
			return
		}
	}

	call.Run()
}

func newSIPMedia(n *Node, remote string) (*sipMedia, error) {
	return newSIPMediaContext(context.Background(), n, remote)
}

func newSIPMediaContext(ctx context.Context, n *Node, remote string) (*sipMedia, error) {
	host := "0.0.0.0"
	if n.Bind != "" {
		host, _, _ = net.SplitHostPort(n.Bind)
	}

	var adv *AdvertiseConfig
	var ports []int
	if n.RTP != nil {
		if n.RTP.Bind != "" {
			host = n.RTP.Bind
		}

		adv = n.RTP.Advertise
		ports = n.RTP.PortRange
	}

	ip := net.ParseIP(host)
	if ip == nil {
		if host == "" {
			host = "0.0.0.0"
			ip = net.IPv4zero
		} else {
			resolved, err := net.ResolveIPAddr("ip", host)
			if err != nil {
				return nil, err
			}

			ip = resolved.IP
			host = ip.String()
		}
	}

	external, err := advertisedIP(host, remote, adv)
	if err != nil {
		return nil, err
	}

	start, end := 0, 0
	if len(ports) == 2 {
		start, end = ports[0], ports[1]-1
		if start%2 != 0 {
			start++
		}
	}

	for port := start; port <= end; port += 2 {
		s := &sipMedia{
			Codecs:     sipCodecs(n),
			Mode:       "sendrecv",
			Laddr:      net.UDPAddr{IP: ip, Port: port},
			ExternalIP: external,
			Symmetric:  n.RTP.symmetricEnabled(),
		}
		if n.RTP != nil {
			s.AllowPrivateDestinations = n.RTP.AllowPrivateDestinations
		}
		peerHost, _, _ := net.SplitHostPort(remote)
		s.SignalingIP = net.ParseIP(peerHost)
		err = s.Init()
		if err == nil {
			if n.RTP != nil && n.RTP.STUNServer != "" {
				if err := s.discoverMapping(ctx, n.RTP.STUNServer); err != nil {
					s.Close()

					return nil, fmt.Errorf("RTP STUN discovery: %w", err)
				}
			}

			return s, nil
		}

		_ = s.Close()
		if start == 0 {
			break
		}
	}

	return nil, fmt.Errorf("allocate RTP: %w", err)
}

type rtpOutput struct {
	audioRate        uint32
	eventTimestamp   uint32
	silence          mediaCodec
	mu               sync.Mutex
	seq              uint16
	timestamp, ssrc  uint32
	pt               uint8
	dtmf             int
	write            func(*rtp.Packet) error
	lastPacket       time.Time
	packets          uint64
	keepalivePackets atomic.Uint64
}

func newRTPOutput(pt uint8, dtmf int, write func(*rtp.Packet) error) *rtpOutput {
	var seed [10]byte
	_, _ = rand.Read(seed[:])

	return &rtpOutput{
		audioRate:      8000,
		eventTimestamp: binary.BigEndian.Uint32(seed[2:6]),
		seq:            binary.BigEndian.Uint16(seed[:2]),
		timestamp:      binary.BigEndian.Uint32(seed[2:6]),
		ssrc:           binary.BigEndian.Uint32(seed[6:]),
		pt:             pt,
		dtmf:           dtmf,
		write:          write,
	}
}

func (o *rtpOutput) send(ctx context.Context, f Frame) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.sendLocked(ctx, f)
}

func (o *rtpOutput) sendLocked(ctx context.Context, f Frame) error {
	write := func(pt uint8, b []byte, marker bool) error {
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: o.seq,
				Timestamp:      o.timestamp,
				SSRC:           o.ssrc,
				Marker:         marker,
			},
			Payload: b,
		}
		if f.Digit != 0 {
			p.Timestamp = o.eventTimestamp
		}
		o.seq++
		err := o.write(p)
		if err == nil {
			o.lastPacket = time.Now()
			o.packets++
		}

		return err
	}
	if f.Digit == 0 {
		samples, err := frameSamples(f)
		if err != nil {
			return err
		}
		o.audioRate = uint32(f.Format.Rate)
		err = write(o.pt, f.Data, false)
		o.timestamp += uint32(samples)
		o.eventTimestamp += uint32(uint64(samples) * 8000 / uint64(o.audioRate))

		return err
	}

	if o.dtmf < 0 {
		return errors.New("peer did not negotiate telephone-event")
	}

	event := strings.IndexByte("0123456789*#ABCD", f.Digit)
	if event < 0 {
		return errors.New("invalid DTMF digit")
	}

	for i := 1; i <= 8; i++ {
		end := i >= 5
		dur := min(i, 5) * 160
		b := []byte{byte(event), 10, byte(dur >> 8), byte(dur)}
		if end {
			b[1] |= 0x80
		}

		if err := write(uint8(o.dtmf), b, i == 1); err != nil {
			return err
		}

		if i < 8 {
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}

	o.timestamp += o.audioRate / 10
	o.eventTimestamp += 800

	return nil
}

func (o *rtpOutput) keepalive(ctx context.Context, format Format, interval time.Duration) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	age := time.Since(o.lastPacket)
	if !o.lastPacket.IsZero() && (age < 20*time.Millisecond || o.packets >= 2 && age < interval) {
		return nil
	}

	if !o.lastPacket.IsZero() && age > 20*time.Millisecond {
		o.timestamp += uint32((age - 20*time.Millisecond).Seconds() * float64(format.Rate))
		o.eventTimestamp += uint32((age - 20*time.Millisecond).Seconds() * 8000)
	}

	data, err := o.silence.encode(make([]int16, format.Rate/50), format)
	if err != nil {
		return err
	}
	err = o.sendLocked(ctx, Frame{Format: format, Data: data})
	if err == nil {
		o.keepalivePackets.Add(1)
	}

	return err
}

func (t *sipTransport) makeLeg(
	e *Engine,
	ctx context.Context,
	n *Node,
	sess *sipMedia,
	id string,
	bye func(context.Context) error,
	do func(context.Context, *sip.Request) (*sip.Response, error),
	remoteTarget sip.Uri,
	dialogCtx context.Context,
	early bool,
) *channelLeg {
	sess.log = t.engine.log.With("node", n.Name, "dialog", id)
	advertisedPort := sess.ExternalPort
	if advertisedPort == 0 {
		advertisedPort = sess.Laddr.Port
	}

	sess.log.Debug(
		"SIP media negotiated",
		"local",
		sess.Laddr.String(),
		"advertised_ip",
		sess.ExternalIP.String(),
		"advertised_port",
		advertisedPort,
		"remote",
		sess.remote.String(),
		"codecs",
		sess.CommonCodecs(),
	)
	var unknownPayload atomic.Uint64
	codec := sess.audioCodec()
	format, _ := formatFor(strings.ToLower(codec.Name), int(codec.SampleRate), 1)
	l := newChannelLeg(ctx, format, e.depth(n))
	l.node = n
	eventPT := -1
	formats := map[uint8]Format{}
	for _, c := range sess.CommonCodecs() {
		if c.Name == "telephone-event" {
			eventPT = int(c.PayloadType)
		} else if f, e := formatFor(strings.ToLower(c.Name), int(c.SampleRate), 1); e == nil {
			formats[c.PayloadType] = f
		}
	}

	out := newRTPOutput(codec.PayloadType, eventPT, sess.WriteRTP)
	out.audioRate = uint32(format.Rate)
	if n.Opus != nil {
		out.silence.bitrate = n.Opus.Bitrate
	}
	var workers sync.WaitGroup
	interval := 15 * time.Second
	if n.RTP != nil {
		interval = duration(n.RTP.Keepalive, interval)
	}

	if interval > 0 {
		l.startFn = func() {
			workers.Add(1)
			go func() {
				defer workers.Done()
				// Give application audio a chance to start, then independently
				// open the NAT path even if neither peer has sent audio yet.
				timer := time.NewTimer(100 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-l.ctx.Done():
					return
				case <-timer.C:
				}

				tick := time.NewTicker(20 * time.Millisecond)
				defer tick.Stop()
				for {
					if err := out.keepalive(l.ctx, format, interval); err != nil {
						l.fail(err)

						return
					}

					select {
					case <-l.ctx.Done():
						return
					case <-tick.C:
					}
				}
			}()
		}
	}

	if !early {
		t.mu.Lock()
		t.legs[id] = l
		t.mu.Unlock()
	}
	stop := context.AfterFunc(dialogCtx, func() {
		l.fail(io.EOF)
	})
	l.closeFn = func() error {
		stop()
		if early {
			// Wake readers without releasing the ports advertised in the INVITE.
			_ = sess.rtp.SetReadDeadline(time.Now())
			_ = sess.rtcp.SetReadDeadline(time.Now())
		} else {
			sess.Close()
		}

		workers.Wait()
		if early {
			_ = sess.rtp.SetReadDeadline(time.Time{})
			_ = sess.rtcp.SetReadDeadline(time.Time{})

			return nil
		}

		sess.log.Debug(
			"SIP media closed",
			"rtp_received",
			sess.received.Load(),
			"rtp_sent",
			sess.sent.Load(),
			"rtp_keepalives",
			out.keepalivePackets.Load(),
			"dropped_source",
			sess.wrongSource.Load(),
			"dropped_malformed",
			sess.malformed.Load(),
			"dropped_payload_type",
			unknownPayload.Load(),
		)
		t.mu.Lock()
		delete(t.legs, id)
		t.mu.Unlock()
		if dialogCtx.Err() == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = bye(ctx)
		}

		return nil
	}
	var info chan byte
	if sends(n) == "sip_info" && !early {
		info = make(chan byte, 32)
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-l.ctx.Done():
					return
				case digit := <-info:
					req := sip.NewRequest(sip.INFO, *remoteTarget.Clone())
					req.AppendHeader(sip.NewHeader("Content-Type", "application/dtmf-relay"))
					req.SetBody([]byte(fmt.Sprintf("Signal=%c\r\nDuration=100\r\n", digit)))
					infoCtx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
					res, err := do(infoCtx, req)
					cancel()
					if err == nil && res.StatusCode != 200 {
						err = fmt.Errorf("SIP INFO rejected: %d", res.StatusCode)
					}
					if err != nil && l.ctx.Err() == nil {
						sess.log.Warn("SIP INFO delivery failed", "error", err)
					}
				}
			}
		}()
	}
	l.writeFn = func(ctx context.Context, f Frame) error {
		if f.Digit != 0 && sends(n) == "sip_info" {
			if early {
				// sipgo establishes in-dialog routing only after the final answer.
				return nil
			}

			select {
			case info <- f.Digit:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-l.ctx.Done():
				return io.EOF
			default:
				return errors.New("SIP INFO queue full")
			}
		}

		return out.send(ctx, f)
	}
	workers.Add(3)
	go func() {
		defer workers.Done()
		buf := make([]byte, 65535)
		var lastTS uint32
		lastEvent := -1
		for {
			var p rtp.Packet
			_, err := sess.ReadRTP(buf, &p)
			if err != nil {
				l.fail(err)

				return
			}

			if int(p.PayloadType) == eventPT {
				if len(p.Payload) >= 4 &&
					p.Payload[1]&0x80 != 0 &&
					(p.Timestamp != lastTS ||
						int(p.Payload[0]) != lastEvent) {
					lastTS = p.Timestamp
					lastEvent = int(p.Payload[0])
					if lastEvent < 16 && receives(n, "rfc4733") {
						if !l.emit(Frame{Digit: "0123456789*#ABCD"[lastEvent]}) {
							return
						}
					}
				}

				continue
			}

			f, ok := formats[p.PayloadType]
			if !ok && unknownPayload.Add(1) == 1 {
				sess.log.Debug("SIP unnegotiated RTP payload type", "payload_type", p.PayloadType)
			}

			if ok && !l.emit(Frame{Format: f, Data: append([]byte(nil), p.Payload...)}) {
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		buf := make([]byte, 1500)
		for {
			_, _, err := sess.ReadRTCPRaw(buf)
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-l.ctx.Done():
				return
			case <-tick.C:
				_ = sess.WriteRTCP(&rtcp.ReceiverReport{SSRC: out.ssrc})
			}
		}
	}()

	return l
}

func (t *sipTransport) info(r *sip.Request, tx sip.ServerTransaction) {
	kind := strings.ToLower(r.Transport())
	id := ""
	if c := t.servers[kind]; c != nil {
		if d, e := c.MatchDialogRequest(r); e == nil {
			id = d.ID
		}
	}

	if id == "" {
		if c := t.clients[kind]; c != nil {
			if d, e := c.MatchRequestDialog(r); e == nil {
				id = d.ID
			}
		}
	}

	t.mu.Lock()
	l := t.legs[id]
	t.mu.Unlock()
	if l == nil {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 481, "Call Does Not Exist", nil))

		return
	}

	ct := r.GetHeader("Content-Type")
	if ct == nil || !strings.HasPrefix(strings.ToLower(ct.Value()), "application/dtmf-relay") {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 415, "Unsupported Media Type", nil))

		return
	}

	digit := byte(0)
	for _, line := range strings.Split(string(r.Body()), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "signal") {
			v = strings.TrimSpace(v)
			if len(v) == 1 && strings.ContainsRune("0123456789*#ABCD", rune(v[0])) {
				digit = v[0]
			}
		}
	}

	if digit == 0 {
		_ = respondSIP(tx, sip.NewResponseFromRequest(r, 400, "Invalid Digit", nil))

		return
	}

	if receives(l.node, "sip_info") {
		select {
		case l.frames <- Frame{Digit: digit}:
		default:
			_ = respondSIP(tx, sip.NewResponseFromRequest(r, 503, "Busy", nil))
			return
		}
	}

	_ = respondSIP(tx, sip.NewResponseFromRequest(r, 200, "OK", nil))
}

func (e *Engine) sipEndpoint(
	ctx, dialCtx context.Context,
	n *Node,
	destination string,
	cr *Credential,
	onEarly ...func(Leg) (func(), error),
) (Leg, error) {
	if n.Source != "" && n.OutboundProxy == "" {
		if source := e.config.source(n.Source); source != nil {
			copy := *n
			copy.OutboundProxy = source.OutboundProxy
			n = &copy
		}
	}

	var t *sipTransport
	owned := false
	if n.Source != "" {
		t = e.getSIP(n.Source)
		if t == nil {
			return nil, errors.New("SIP source is not running")
		}
	} else {
		var err error
		t, err = e.startSIP(n)
		if err != nil {
			return nil, err
		}

		owned = true
	}

	fail := func() {
		if owned {
			e.removeCloser(t)
			t.Close()
		}
	}
	uri := sip.Uri{}
	if err := sip.ParseUri(n.URI, &uri); err != nil {
		fail()

		return nil, err
	}

	if destination != "" {
		if strings.ContainsAny(destination, "\r\n@;<> ") || strings.Contains(destination, ":") {
			fail()

			return nil, errors.New("destination must be a SIP user, not an arbitrary URI")
		}

		uri.User = destination
	}

	transport := sipNetwork(n)
	cache := t.clients[transport]
	if cache == nil {
		fail()

		return nil, errors.New("requested SIP transport is unavailable")
	}

	request := sip.NewRequest(sip.INVITE, uri)
	if err := routeSIPRequest(request, n); err != nil {
		fail()

		return nil, err
	}

	sess, err := newSIPMediaContext(dialCtx, n, request.Destination())
	if err != nil {
		fail()

		return nil, err
	}

	if cr != nil {
		from := &sip.FromHeader{
			Address: sip.Uri{Scheme: "sip", User: cr.Username, Host: uri.Host, Port: uri.Port},
			Params:  sip.NewParams(),
		}
		from.Params.Add("tag", randomID())
		request.AppendHeader(from)
	}

	request.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	request.SetBody(sess.LocalSDP())
	d, err := cache.WriteInvite(dialCtx, request)
	if err != nil {
		sess.Close()
		fail()

		return nil, err
	}

	var earlyBody []byte
	var earlyTag string
	stopEarly := func() {}
	defer func() { stopEarly() }()
	progress := func(res *sip.Response) error {
		if !res.IsProvisional() || res.StatusCode == 100 || len(res.Body()) == 0 || len(onEarly) == 0 {
			return nil
		}

		ct := res.GetHeader("Content-Type")
		if ct == nil || !strings.EqualFold(strings.TrimSpace(strings.Split(ct.Value(), ";")[0]), "application/sdp") {
			return nil
		}

		for _, h := range res.GetHeaders("Require") {
			for _, token := range strings.Split(h.Value(), ",") {
				if strings.EqualFold(strings.TrimSpace(token), "100rel") {
					return errors.New("reliable SIP provisional responses (100rel/PRACK) are unsupported")
				}
			}
		}

		if res.To() == nil {
			return errors.New("SIP provisional SDP lacks a To header")
		}

		tag, _ := res.To().Params.Get("tag")
		if tag == "" {
			return errors.New("SIP provisional SDP lacks a dialog tag")
		}
		if tag == earlyTag && bytes.Equal(earlyBody, res.Body()) {
			return nil
		}

		stopEarly()
		stopEarly = func() {}
		if err := sess.RemoteSDP(res.Body()); err != nil {
			return fmt.Errorf("SIP provisional SDP: %w", err)
		}

		peerHost, _, _ := net.SplitHostPort(res.Source())
		sess.SignalingIP = net.ParseIP(peerHost)
		earlyLeg := t.makeLeg(e, ctx, n, sess, request.CallID().Value(), nil, nil, uri, ctx, true)
		stopBridge, err := onEarly[0](earlyLeg)
		if err != nil {
			earlyLeg.Close()

			return err
		}

		stopEarly = func() {
			stopBridge()
			earlyLeg.Close()
		}
		earlyBody = append([]byte(nil), res.Body()...)
		earlyTag = tag
		e.log.Debug("SIP early media started", "node", n.Name, "status", res.StatusCode)

		return nil
	}

	for attempt := 0; attempt < 3; attempt++ {
		err = waitSIPAnswer(dialCtx, d, d.UA.Client, progress)
		stopEarly()
		stopEarly = func() {}
		if err == nil {
			break
		}

		var response *sipgo.ErrDialogResponse
		if cr == nil ||
			!errors.As(err, &response) ||
			(response.Res.StatusCode != 401 &&
				response.Res.StatusCode != 407) ||
			attempt == 2 {
			break
		}
		// The previous transaction retains its request for non-2xx ACKs.
		earlyBody = nil
		earlyTag = ""
		d.InviteRequest = d.InviteRequest.Clone()
		if err = applySIPCredential(d.InviteRequest, response.Res, cr); err != nil {
			break
		}

		d.InviteRequest.RemoveHeader("Via")
		d.InviteRequest.CSeq().SeqNo++
		err = d.Invite(dialCtx)
		if err != nil {
			break
		}
	}

	if err != nil {
		d.Close()
		sess.Close()
		fail()
		var response *sipgo.ErrDialogResponse
		if errors.As(err, &response) &&
			(response.Res.StatusCode == 401 ||
				response.Res.StatusCode == 403 ||
				response.Res.StatusCode == 407) {
			return nil, errUnauthorized
		}

		return nil, err
	}

	if err = dialCtx.Err(); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = d.Ack(cleanup)
		_ = d.Bye(cleanup)
		d.Close()
		sess.Close()
		fail()

		return nil, err
	}

	if err = d.Ack(dialCtx); err != nil {
		d.Close()
		sess.Close()
		fail()

		return nil, err
	}

	answer := d.InviteResponse.Body()
	answerTag, _ := d.InviteResponse.To().Params.Get("tag")
	if len(earlyBody) == 0 || answerTag != earlyTag || (len(answer) > 0 && !bytes.Equal(answer, earlyBody)) {
		err = sess.RemoteSDP(answer)
	}
	if err != nil {
		_ = d.Bye(dialCtx)
		d.Close()
		sess.Close()
		fail()

		return nil, fmt.Errorf("SIP answer SDP: %w", err)
	}

	peerHost, _, _ := net.SplitHostPort(d.InviteResponse.Source())
	sess.SignalingIP = net.ParseIP(peerHost)
	remoteTarget := uri
	if contact := d.InviteResponse.Contact(); contact != nil {
		remoteTarget = contact.Address
	}

	l := t.makeLeg(e, ctx, n, sess, d.ID, d.Bye, d.Do, remoteTarget, d.Context(), false)
	closeFn := l.closeFn
	l.closeFn = func() error {
		err := closeFn()
		d.Close()
		fail()

		return err
	}

	return l, nil
}

// Use a preloaded loose route, preserving the destination and digest URI.
// Established dialogs follow the Record-Route set returned by the proxy.
func routeSIPRequest(req *sip.Request, n *Node) error {
	req.SetTransport(sipNetwork(n))
	if n.OutboundProxy == "" {
		return nil
	}

	var proxy sip.Uri
	if err := sip.ParseUri(n.OutboundProxy, &proxy); err != nil {
		return err
	}

	if proxy.UriParams == nil {
		proxy.UriParams = sip.NewParams()
	}

	proxy.UriParams.Add("lr", "")
	req.AppendHeader(&sip.RouteHeader{Address: proxy})

	return nil
}

// sipgo normally waits for a provisional response before honoring cancellation.
// Enforce our setup deadline, while giving a ringing peer a bounded CANCEL exchange.
func waitSIPAnswer(
	ctx context.Context,
	d *sipgo.DialogClientSession,
	client *sipgo.Client,
	onResponse ...func(*sip.Response) error,
) error {
	request := d.InviteRequest.Clone()
	answerCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	var provisional atomic.Bool
	done := make(chan struct{})
	var cancelOnce sync.Once
	sendCancel := func() {
		cancelOnce.Do(func() {
			if provisional.Load() {
				cancelRequest := request.Clone()
				cancelRequest.Method = sip.CANCEL
				cancelRequest.CSeq().MethodName = sip.CANCEL
				cancelRequest.RemoveHeader("Content-Type")
				cancelRequest.RemoveHeader("Authorization")
				cancelRequest.RemoveHeader("Proxy-Authorization")
				cancelRequest.SetBody(nil)
				cleanup, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
				defer cleanupCancel()
				_, _ = client.Do(cleanup, cancelRequest)
			}
		})
	}
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		defer cancel(sipgo.WaitAnswerForceCancelErr)
		sendCancel()
	})
	err := d.WaitAnswer(answerCtx, sipgo.AnswerOptions{OnResponse: func(res *sip.Response) error {
		slog.Debug(
			"SIP response",
			"call_id",
			request.CallID().Value(),
			"status",
			res.StatusCode,
			"reason",
			res.Reason,
			"has_sdp",
			len(res.Body()) > 0,
		)
		if res.IsProvisional() {
			provisional.Store(true)
		}
		for _, response := range onResponse {
			if err := response(res); err != nil {
				sendCancel()

				return err
			}
		}

		return nil
	}})
	if !stop() {
		<-done
	}

	cancel(nil)
	if ctx.Err() != nil {
		if err == nil {
			cleanup, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
			defer cleanupCancel()
			_ = d.Ack(cleanup)
			_ = d.Bye(cleanup)
		}

		return ctx.Err()
	}

	return err
}

func respondSIP(tx sip.ServerTransaction, res *sip.Response) error {
	err := tx.Respond(res)
	if err == nil && res.StatusCode >= 300 && res.CSeq() != nil && res.CSeq().MethodName == sip.INVITE {
		go func() {
			select {
			case <-tx.Acks():
			case <-tx.Done():
			}
		}()
	}

	return err
}

func applySIPCredential(req *sip.Request, res *sip.Response, cr *Credential) error {
	challengeName, header := "WWW-Authenticate", "Authorization"
	if res.StatusCode == 407 {
		challengeName = "Proxy-Authenticate"
		header = "Proxy-Authorization"
	}

	for _, h := range res.GetHeaders(challengeName) {
		ch, err := digest.ParseChallenge(h.Value())
		if err != nil {
			continue
		}

		opts := digest.Options{
			Username: cr.Username,
			Method:   string(req.Method),
			URI:      req.Recipient.Addr(),
			Count:    1,
		}
		if cr.Password != nil {
			opts.Password = *cr.Password
		} else if cr.Digest != nil &&
			cr.Digest.Realm == ch.Realm &&
			strings.EqualFold(cr.Digest.Algorithm, ch.Algorithm) {
			opts.A1 = cr.Digest.HA1
		} else {
			continue
		}

		got, err := digest.Digest(ch, opts)
		if err != nil {
			continue
		}

		req.RemoveHeader(header)
		req.AppendHeader(sip.NewHeader(header, got.String()))

		return nil
	}

	return errors.New("no compatible SIP authentication challenge")
}

func (t *sipTransport) registerLoop() {
	e := t.engine
	r := t.node.Registration
	expiry := duration(r.Expires, 5*time.Minute)
	delay := time.Duration(0)
	var previous *sip.Request
	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-e.ctx.Done():
				timer.Stop()
				t.unregister(previous)
				return
			case <-timer.C:
			}
		}

		if e.ctx.Err() != nil {
			t.unregister(previous)

			return
		}

		ctx, cancel := context.WithTimeout(e.ctx, duration(e.timeouts(t.node).Handshake, 10*time.Second))
		req, expires, err := t.register(ctx, previous, expiry)
		cancel()
		if err != nil {
			e.log.Warn("SIP registration failed", "source", t.node.Name)
			delay = 5 * time.Second
		} else {
			previous = req
			delay = expires * 4 / 5
			e.log.Info("SIP registered", "source", t.node.Name)
		}
	}
}

func (t *sipTransport) register(
	ctx context.Context,
	previous *sip.Request,
	expires time.Duration,
) (*sip.Request, time.Duration, error) {
	r := t.node.Registration
	uri := sip.Uri{}
	if err := sip.ParseUri(r.Registrar, &uri); err != nil {
		return nil, 0, err
	}

	aor := sip.Uri{}
	if err := sip.ParseUri(r.AOR, &aor); err != nil {
		return nil, 0, err
	}

	req := sip.NewRequest(sip.REGISTER, uri)
	transport := sipNetwork(t.node)
	if err := routeSIPRequest(req, t.node); err != nil {
		return nil, 0, err
	}

	from := &sip.FromHeader{Address: aor, Params: sip.NewParams()}
	from.Params.Add("tag", randomID())
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: aor, Params: sip.NewParams()})
	contact := t.contacts[transport]
	contact.Address.User = aor.User
	req.AppendHeader(&contact)
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(int(expires.Seconds()))))
	if previous != nil {
		req.AppendHeader(sip.HeaderClone(previous.CallID()))
		seq := *previous.CSeq()
		seq.SeqNo++
		req.AppendHeader(&seq)
	}

	cr := t.engine.active().config.credential(r.Credential)
	for i := 0; i < 5; i++ {
		res, err := t.client.Do(ctx, req)
		if err != nil {
			return nil, 0, err
		}

		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if h := res.GetHeader("Expires"); h != nil {
				if n, err := strconv.Atoi(h.Value()); err == nil && n > 0 {
					expires = time.Duration(n) * time.Second
				}
			}

			if contact := res.Contact(); contact != nil {
				if value, ok := contact.Params.Get("expires"); ok {
					if n, err := strconv.Atoi(value); err == nil && n > 0 {
						expires = time.Duration(n) * time.Second
					}
				}
			}

			return req, expires, nil
		}

		if res.StatusCode == 423 {
			if h := res.GetHeader("Min-Expires"); h != nil {
				if seconds, err := strconv.Atoi(h.Value()); err == nil && seconds > 0 && seconds <= 86400 {
					expires = time.Duration(seconds) * time.Second
					req = req.Clone()
					req.RemoveHeader("Expires")
					req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(seconds)))
					req.RemoveHeader("Authorization")
					req.RemoveHeader("Proxy-Authorization")
					req.RemoveHeader("Via")
					req.CSeq().SeqNo++
					continue
				}
			}
		}

		if res.StatusCode != 401 && res.StatusCode != 407 {
			return nil, 0, fmt.Errorf("REGISTER returned %d", res.StatusCode)
		}

		if err = applySIPCredential(req, res, cr); err != nil {
			return nil, 0, err
		}

		req.RemoveHeader("Via")
		req.CSeq().SeqNo++
	}

	return nil, 0, errUnauthorized
}

func (t *sipTransport) unregister(previous *sip.Request) {
	if previous == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _ = t.register(ctx, previous, 0)
}
