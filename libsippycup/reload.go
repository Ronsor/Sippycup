package libsippycup

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
)

func (e *Engine) active() *Engine {
	return e.current.Load()
}

// Reload publishes a validated configuration for new calls without interrupting
// existing calls. Config must be a fresh, independent configuration tree and must
// remain immutable after this call. Reload retains
// listeners, SIP registrations, dialogs, sessions, and global resource accounting.
// Source names/types, binds, TLS, SIP transport/advertisement/registration settings,
// and HTTP header timeouts must stay unchanged; incompatible changes are rejected
// without modifying the running configuration. HTTP paths may change; existing
// session URLs continue to use their original configuration and authentication.
// Call Reload only after Start. Do not call it concurrently with Start or Close.
func (e *Engine) Reload(config *Config) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()

	if err := e.ctx.Err(); err != nil {
		return err
	}

	if config == nil {
		return fmt.Errorf("reload requires a configuration")
	}

	old := e.active()
	if config == old.config {
		return fmt.Errorf("reload requires a new configuration")
	}

	if err := config.Validate(); err != nil {
		return err
	}

	next := &Engine{config: config, engineState: e.engineState}
	if len(config.Sources) != len(old.config.Sources) {
		return fmt.Errorf("changing the source set requires a restart")
	}

	for _, source := range old.config.Sources {
		n := config.source(source.Name)
		if n == nil || n.Type != source.Type || n.Bind != source.Bind ||
			!reflect.DeepEqual(n.TLS, source.TLS) {
			return fmt.Errorf("source %s: changing listener identity, bind, or TLS requires a restart", source.Name)
		}

		if n.Type == "sip" && (n.Transport != source.Transport ||
			!reflect.DeepEqual(n.Transports, source.Transports) ||
			!reflect.DeepEqual(n.Advertise, source.Advertise) ||
			!reflect.DeepEqual(n.Registration, source.Registration) ||
			sipNetwork(n) != sipNetwork(source) ||
			(n.Registration != nil && n.OutboundProxy != source.OutboundProxy)) {
			return fmt.Errorf("source %s: changing SIP transport or registration requires a restart", source.Name)
		}

		if (n.Type == "webrtc_http" || n.Type == "audiosocket_ws") &&
			next.timeouts(n).Handshake != old.timeouts(source).Handshake {
			return fmt.Errorf("source %s: changing HTTP handshake timeout requires a restart", source.Name)
		}
	}

	e.current.Store(next)

	return nil
}

// serveHTTP chooses a generation per request. Existing WebRTC resources resolve
// before current paths so moves and credential changes do not orphan live calls.
func (e *Engine) serveHTTP(bind string, w http.ResponseWriter, r *http.Request) {
	if !e.beginWork() {
		http.Error(w, "Shutting Down", http.StatusServiceUnavailable)
		return
	}

	defer e.wg.Done()

	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	e.webMu.Lock()
	session := e.web[id]
	e.webMu.Unlock()
	if session != nil && session.node != nil && session.node.Bind == bind &&
		r.URL.Path == strings.TrimSuffix(session.node.Path, "/")+"/sessions/"+id {
		if checkOrigin(session.node, w, r) {
			session.engine.serveWebRTC(session.node, w, r)
		}

		return
	}

	snapshot := e.active()
	for _, n := range snapshot.config.Sources {
		if n.Bind != bind || (n.Type != "webrtc_http" && n.Type != "audiosocket_ws") {
			continue
		}

		if r.URL.Path != n.Path && !(n.Type == "webrtc_http" &&
			strings.HasPrefix(r.URL.Path, strings.TrimSuffix(n.Path, "/")+"/sessions/")) {
			continue
		}

		if !checkOrigin(n, w, r) {
			return
		}

		if n.Type == "audiosocket_ws" {
			snapshot.serveAudioWS(n, w, r)
		} else {
			snapshot.serveWebRTC(n, w, r)
		}

		return
	}

	http.NotFound(w, r)
}
