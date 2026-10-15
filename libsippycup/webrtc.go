package libsippycup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// webrtc_http is a bidirectional HTTP SDP profile, not WHEP. POST exchanges full
// SDP; PATCH accepts trickled remote candidates; DELETE tears down the session.
type webSession struct {
	engine *Engine
	node   *Node
	source string
	auth   *AuthIdentity
	pc     *webrtc.PeerConnection
	leg    Leg
	cancel context.CancelFunc
}

type webTrack struct {
	mu      sync.Mutex
	writer  webrtc.TrackLocalWriter
	ssrc    webrtc.SSRC
	codec   webrtc.RTPCodecParameters
	eventPT int
	ready   chan struct{}
	once    sync.Once
}

// ID returns the stable track identifier used in SDP.
func (t *webTrack) ID() string {
	return "audio"
}

// RID returns an empty identifier because simulcast is unsupported.
func (t *webTrack) RID() string {
	return ""
}

// StreamID identifies the call audio stream.
func (t *webTrack) StreamID() string {
	return "call"
}

// Kind identifies this as an audio track.
func (t *webTrack) Kind() webrtc.RTPCodecType {
	return webrtc.RTPCodecTypeAudio
}

// Bind selects a negotiated G.711 codec and records its RTP writer and SSRC.
func (t *webTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.eventPT = -1
	var codec webrtc.RTPCodecParameters
	for _, c := range ctx.CodecParameters() {
		if strings.EqualFold(c.MimeType, "audio/telephone-event") {
			t.eventPT = int(c.PayloadType)
		} else if codec.MimeType == "" &&
			(strings.EqualFold(c.MimeType, "audio/PCMU") ||
				strings.EqualFold(c.MimeType, "audio/PCMA")) {
			codec = c
		}
	}

	if codec.MimeType == "" {
		return codec, errors.New("no supported WebRTC audio codec")
	}

	t.codec = codec
	t.writer = ctx.WriteStream()
	t.ssrc = ctx.SSRC()
	t.once.Do(func() {
		close(t.ready)
	})

	return codec, nil
}

// Unbind detaches the negotiated RTP writer.
func (t *webTrack) Unbind(webrtc.TrackLocalContext) error {
	t.mu.Lock()
	t.writer = nil
	t.mu.Unlock()

	return nil
}

func (t *webTrack) write(p *rtp.Packet) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.writer == nil {
		return io.ErrClosedPipe
	}

	p.Header.SSRC = uint32(t.ssrc)
	_, err := t.writer.WriteRTP(&p.Header, p.Payload)

	return err
}

type webPeer struct {
	pc        *webrtc.PeerConnection
	leg       *channelLeg
	track     *webTrack
	connected chan struct{}
	once      sync.Once
	node      *Node
}

func (e *Engine) newPeer(ctx context.Context, n *Node) (*webPeer, error) {
	m := &webrtc.MediaEngine{}
	codecs := sipCodecs(n)
	for _, c := range codecs {
		cap := webrtc.RTPCodecCapability{
			MimeType:  "audio/" + c.Name,
			ClockRate: c.SampleRate,
			Channels:  1,
		}
		if c.Name == "telephone-event" {
			cap.SDPFmtpLine = "0-15"
		}

		if err := m.RegisterCodec(
			webrtc.RTPCodecParameters{
				RTPCodecCapability: cap,
				PayloadType:        webrtc.PayloadType(c.PayloadType),
			},
			webrtc.RTPCodecTypeAudio,
		); err != nil {
			return nil, err
		}
	}

	settings := webrtc.SettingEngine{}
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	includeLoopback := false
	if c := n.ICE; c != nil {
		includeLoopback = c.IncludeLoopback || net.ParseIP(c.Bind).IsLoopback()
	}
	settings.SetIncludeLoopbackCandidate(includeLoopback)
	config := webrtc.Configuration{}
	if c := n.ICE; c != nil {
		if len(c.PortRange) == 2 {
			if err := settings.SetEphemeralUDPPortRange(uint16(c.PortRange[0]), uint16(c.PortRange[1])); err != nil {
				return nil, err
			}
		}

		if len(c.Interfaces) > 0 {
			settings.SetInterfaceFilter(func(name string) bool {
				return slices.Contains(c.Interfaces, name)
			})
		}

		if c.Bind != "" {
			ip := net.ParseIP(c.Bind)
			if ip == nil {
				return nil, errors.New("ICE bind must be an IP")
			}

			if !ip.IsUnspecified() {
				settings.SetIPFilter(func(other net.IP) bool {
					return ip.Equal(other)
				})
			}
		}

		if c.Policy == "relay" {
			config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		}

		for _, s := range c.Servers {
			server := webrtc.ICEServer{URLs: s.URLs}
			if s.Credential != "" {
				cr := e.config.credential(s.Credential)
				server.Username = cr.Username
				server.Credential = *cr.Password
			}

			config.ICEServers = append(config.ICEServers, server)
		}

		if len(c.NATMappings) > 0 {
			rules := []webrtc.ICEAddressRewriteRule{}
			for _, v := range c.NATMappings {
				rules = append(
					rules,
					webrtc.ICEAddressRewriteRule{
						External:        []string{v.Public},
						Local:           v.Local,
						AsCandidateType: webrtc.ICECandidateTypeHost,
					},
				)
			}

			if err := settings.SetICEAddressRewriteRules(rules...); err != nil {
				return nil, err
			}
		}
	}

	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, registry); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithSettingEngine(settings),
		webrtc.WithInterceptorRegistry(registry),
	)
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	f := Format{"pcmu", 8000, 1}
	if len(codecs) > 0 {
		f.Codec = strings.ToLower(codecs[0].Name)
	}

	l := newChannelLeg(ctx, f, e.depth(n))
	track := &webTrack{ready: make(chan struct{}), eventPT: -1}
	p := &webPeer{
		pc:        pc,
		leg:       l,
		track:     track,
		connected: make(chan struct{}),
		node:      n,
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		pc.Close()

		return nil, err
	}

	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			p.once.Do(func() {
				close(p.connected)
			})
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			l.fail(io.EOF)
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		formats := map[uint8]Format{}
		eventPT := -1
		for _, c := range receiver.GetParameters().Codecs {
			name := strings.TrimPrefix(strings.ToLower(c.MimeType), "audio/")
			if name == "telephone-event" {
				eventPT = int(c.PayloadType)
			} else if f, err := formatFor(name, int(c.ClockRate), 1); err == nil {
				formats[uint8(c.PayloadType)] = f
			}
		}

		go func() {
			var lastTS uint32
			lastEvent := -1
			for {
				packet, _, err := remote.ReadRTP()
				if err != nil {
					l.fail(err)

					return
				}

				if int(packet.PayloadType) == eventPT {
					if len(packet.Payload) >= 4 &&
						packet.Payload[1]&0x80 != 0 &&
						(packet.Timestamp != lastTS ||
							int(packet.Payload[0]) != lastEvent) {
						lastTS = packet.Timestamp
						lastEvent = int(packet.Payload[0])
						if lastEvent < 16 && receives(n, "rfc4733") {
							if !l.emit(Frame{Digit: "0123456789*#ABCD"[lastEvent]}) {
								return
							}
						}
					}

					continue
				}

				f, ok := formats[packet.PayloadType]
				if ok && !l.emit(Frame{Format: f, Data: append([]byte(nil), packet.Payload...)}) {
					return
				}
			}
		}()
	})
	l.closeFn = pc.Close
	var out *rtpOutput
	var writeMu sync.Mutex
	l.writeFn = func(ctx context.Context, f Frame) error {
		select {
		case <-track.ready:
		case <-ctx.Done():
			return ctx.Err()
		}

		select {
		case <-p.connected:
		case <-ctx.Done():
			return ctx.Err()
		}

		writeMu.Lock()
		defer writeMu.Unlock()
		if out == nil {
			track.mu.Lock()
			out = newRTPOutput(uint8(track.codec.PayloadType), track.eventPT, track.write)
			track.mu.Unlock()
		}

		return out.send(ctx, f)
	}

	return p, nil
}

func (p *webPeer) negotiated() error {
	select {
	case <-p.track.ready:
		p.track.mu.Lock()
		defer p.track.mu.Unlock()
		name := strings.TrimPrefix(strings.ToLower(p.track.codec.MimeType), "audio/")
		f, err := formatFor(name, int(p.track.codec.ClockRate), 1)
		if err != nil {
			return err
		}
		p.leg.format = f
		return nil
	default:
		return errors.New("WebRTC offer must negotiate bidirectional PCMU or PCMA audio")
	}
}

func gatherSDP(ctx context.Context, pc *webrtc.PeerConnection, desc webrtc.SessionDescription) (string, error) {
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(desc); err != nil {
		return "", err
	}

	select {
	case <-done:
		return pc.LocalDescription().SDP, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (e *Engine) serveWebRTC(n *Node, w http.ResponseWriter, r *http.Request) {
	auth, ok := e.config.httpAuth(n, w, r)
	if !ok {
		return
	}

	if r.URL.Path != n.Path {
		e.serveWebSession(n, auth, w, r)

		return
	}

	if serveBrowser(n, w, r) {
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "method not allowed", 405)

		return
	}

	typ, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if typ != "application/sdp" {
		http.Error(w, "expected application/sdp", 415)

		return
	}

	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(duration(e.timeouts(n).Handshake, 10*time.Second)))
	defer http.NewResponseController(w).SetReadDeadline(time.Time{})
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, e.limits(n).MaxHTTPBodyBytes))
	if err != nil {
		http.Error(w, "invalid SDP body", 413)

		return
	}

	ctx, cancel := context.WithCancel(e.ctx)
	stopRequest := context.AfterFunc(r.Context(), cancel)
	defer stopRequest()
	p, err := e.newPeer(ctx, n)
	if err != nil {
		cancel()
		http.Error(w, "media unavailable", 503)

		return
	}

	keep := false
	defer func() {
		if !keep {
			cancel()
			p.leg.Close()
		}
	}()
	remoteOffer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(body)}
	if err = p.pc.SetRemoteDescription(remoteOffer); err != nil {
		http.Error(w, "invalid SDP offer", 400)

		return
	}

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "unsupported SDP offer", 400)

		return
	}

	handshake, cancelHandshake := context.WithTimeout(r.Context(), duration(e.timeouts(n).Handshake, 10*time.Second))
	defer cancelHandshake()
	answerSDP, err := gatherSDP(handshake, p.pc, answer)
	if err != nil {
		http.Error(w, "ICE gathering failed", 504)

		return
	}

	if err = p.negotiated(); err != nil {
		http.Error(w, "bidirectional PCMU/PCMA audio required", 400)

		return
	}

	req := &Request{
		ID:     randomID(),
		Source: n.Name,
		Remote: r.RemoteAddr,
		Auth:   auth,
		HTTP:   httpRequestInfo(r),
	}
	id := randomID()
	early := make(chan struct{})
	var earlyOnce sync.Once
	if earlyMediaMode(n) == "answer" {
		req.earlyMedia = func() error {
			earlyOnce.Do(func() { close(early) })

			return nil
		}
	}

	type preparedCall struct {
		call *Call
		err  error
	}

	prepared := make(chan preparedCall, 1)
	published := make(chan struct{})
	defer func() {
		if !keep {
			cancel()
		}
		close(published)
	}()
	var timer *time.Timer
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()
		defer p.leg.Close()
		call, err := e.prepare(ctx, n, req, p.leg)
		prepared <- preparedCall{call, err}
		// The handler owns HTTP output and publishes the session before this
		// worker can remove it. Dialing may outlive an early HTTP answer.
		<-published
		if timer != nil {
			defer timer.Stop()
		}

		defer func() {
			e.webMu.Lock()
			delete(e.web, id)
			e.webMu.Unlock()
		}()
		if err != nil {
			e.logFailure(req, err)

			return
		}

		defer call.Close()
		if ctx.Err() == nil {
			call.Run()
		}
	}()

	select {
	case result := <-prepared:
		if result.err != nil {
			code, reason := statusError(result.err)
			http.Error(w, reason, code)

			return
		}
	case <-early:
	case <-r.Context().Done():
		return
	case <-ctx.Done():
		return
	}

	session := &webSession{
		engine: e,
		node:   n,
		source: n.Name,
		auth:   auth,
		pc:     p.pc,
		leg:    p.leg,
		cancel: cancel,
	}
	e.webMu.Lock()
	e.web[id] = session
	e.webMu.Unlock()
	path := strings.TrimSuffix(n.Path, "/") + "/sessions/" + id
	location := path
	if n.Advertise != nil && n.Advertise.Mode == "explicit" {
		location = strings.TrimSuffix(n.Advertise.BaseURL, "/") + path
	}

	w.Header().Set("Location", location)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Cache-Control", "no-store")
	if n.ICE != nil && n.ICE.Trickle {
		w.Header().Set("Accept-Patch", "application/trickle-ice-sdpfrag, application/json")
	}

	w.WriteHeader(http.StatusCreated)
	if _, err := io.WriteString(w, answerSDP); err != nil {
		return
	}

	// Ending the HTTP request must not cancel an accepted session. A pending
	// session already supports authenticated PATCH and DELETE through Location.
	if !stopRequest() && r.Context().Err() != nil {
		return
	}

	keep = true
	timer = time.AfterFunc(duration(e.timeouts(n).Handshake, 10*time.Second), func() {
		select {
		case <-p.connected:
		default:
			cancel()
			p.leg.Close()
		}
	})
}

func (e *Engine) serveWebSession(n *Node, auth *AuthIdentity, w http.ResponseWriter, r *http.Request) {
	prefix := strings.TrimSuffix(n.Path, "/") + "/sessions/"
	id := strings.TrimPrefix(r.URL.Path, prefix)
	e.webMu.Lock()
	s := e.web[id]
	e.webMu.Unlock()
	if s == nil || s.source != n.Name {
		http.NotFound(w, r)

		return
	}

	if s.auth != nil &&
		(auth == nil ||
			!equalSecret(s.auth.Username, auth.Username) ||
			!equalSecret(s.auth.Password, auth.Password)) {
		http.Error(w, "forbidden", 403)

		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.cancel()
		s.leg.Close()
		e.webMu.Lock()
		delete(e.web, id)
		e.webMu.Unlock()
		w.WriteHeader(204)
	case http.MethodPatch:
		if n.ICE == nil || !n.ICE.Trickle {
			http.Error(w, "trickle ICE disabled", 405)

			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, e.limits(n).MaxHTTPBodyBytes))
		if err != nil {
			http.Error(w, "body too large", 413)

			return
		}
		typ, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		var candidates []webrtc.ICECandidateInit
		switch typ {
		case "application/json":
			var candidate webrtc.ICECandidateInit
			if json.Unmarshal(b, &candidate) != nil {
				http.Error(w, "invalid candidate", 400)

				return
			}
			candidates = append(candidates, candidate)
		case "application/trickle-ice-sdpfrag":
			mid := "0"
			var ufrag *string
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, "a=mid:"):
					mid = strings.TrimPrefix(line, "a=mid:")
				case strings.HasPrefix(line, "a=ice-ufrag:"):
					v := strings.TrimPrefix(line, "a=ice-ufrag:")
					ufrag = &v
				case strings.HasPrefix(line, "a=candidate:"):
					m := mid
					candidates = append(
						candidates,
						webrtc.ICECandidateInit{
							Candidate:        strings.TrimPrefix(line, "a="),
							SDPMid:           &m,
							UsernameFragment: ufrag,
						},
					)
				case line == "a=end-of-candidates":
					candidates = append(candidates, webrtc.ICECandidateInit{})
				}
			}
		default:
			http.Error(w, "unsupported candidate content type", 415)
			return
		}
		for _, candidate := range candidates {
			if err := s.pc.AddICECandidate(candidate); err != nil {
				http.Error(w, "invalid ICE candidate", 400)

				return
			}
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "PATCH, DELETE, OPTIONS")
		http.Error(w, "method not allowed", 405)
	}
}

func (e *Engine) webEndpoint(ctx, dialCtx context.Context, n *Node, cr *Credential) (Leg, error) {
	if cr != nil && cr.Password == nil {
		return nil, errors.New("WebRTC HTTP authentication requires a plaintext password")
	}

	p, err := e.newPeer(ctx, n)
	if err != nil {
		return nil, err
	}

	fail := func(err error) (Leg, error) {
		p.leg.Close()

		return nil, err
	}
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return fail(err)
	}

	sdp, err := gatherSDP(dialCtx, p.pc, offer)
	if err != nil {
		return fail(err)
	}

	client, err := httpClient(n, duration(e.timeouts(n).Connect, 5*time.Second))
	if err != nil {
		return fail(err)
	}

	req, err := http.NewRequestWithContext(dialCtx, http.MethodPost, n.URL, strings.NewReader(sdp))
	if err != nil {
		return fail(err)
	}

	req.Header.Set("Content-Type", "application/sdp")
	if cr != nil {
		req.SetBasicAuth(cr.Username, *cr.Password)
	}

	res, err := client.Do(req)
	if err != nil {
		return fail(err)
	}

	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		return fail(fmt.Errorf("WebRTC endpoint returned %d", res.StatusCode))
	}

	b, err := io.ReadAll(io.LimitReader(res.Body, e.limits(n).MaxHTTPBodyBytes+1))
	if err != nil || int64(len(b)) > e.limits(n).MaxHTTPBodyBytes {
		return fail(errors.New("invalid SDP response"))
	}

	base, _ := url.Parse(n.URL)
	location, err := base.Parse(res.Header.Get("Location"))
	if err != nil || res.Header.Get("Location") == "" || location.Scheme != base.Scheme || location.Host != base.Host {
		return fail(errors.New("invalid or cross-origin session Location"))
	}

	closeFn := p.leg.closeFn
	p.leg.closeFn = func() error {
		err := closeFn()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, location.String(), nil)
		if cr != nil {
			req.SetBasicAuth(cr.Username, *cr.Password)
		}

		if res, e := client.Do(req); e == nil {
			res.Body.Close()
		}

		client.CloseIdleConnections()

		return err
	}
	remoteAnswer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(b)}
	if err = p.pc.SetRemoteDescription(remoteAnswer); err != nil {
		return fail(err)
	}

	if err = p.negotiated(); err != nil {
		return fail(err)
	}

	select {
	case <-p.connected:
		return p.leg, nil
	case <-dialCtx.Done():
		return fail(dialCtx.Err())
	}
}
