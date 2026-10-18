package libsippycup

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

func validateNodeShape(n *Node, source bool) error {
	bad := func(s string) error {
		return fmt.Errorf("%s: %s", n.Name, s)
	}

	if n.InProcess != nil && (source || n.Type != "inproc") {
		return bad("InProcess applies only to inproc endpoints")
	}

	if n.Type == "inproc" && (n.Bind != "" || n.Path != "" || n.URL != "" ||
		n.URI != "" || n.Address != "" || n.Auth != nil || n.TLS != nil ||
		n.Advertise != nil || n.Transport != "" || len(n.Transports) != 0 ||
		len(n.Codecs) != 0 || len(n.Origins) != 0) {
		return bad("inproc uses Go legs and transcode settings, not network settings")
	}

	if n.Opus != nil {
		if n.Type != "sip" && n.Type != "webrtc_http" && n.Type != "inproc" {
			return bad("opus options apply to SIP, WebRTC, and inproc nodes")
		}
		if n.Opus.Bitrate != 0 && (n.Opus.Bitrate < 6000 || n.Opus.Bitrate > 510000) {
			return bad("Opus bitrate must be between 6000 and 510000")
		}
	}
	if n.Type == "exec" && n.Transcode != nil && n.Transcode.Codec == "opus" {
		return bad("exec streams require PCM or G.711; Opus requires packet boundaries")
	}

	if n.UUID != "" {
		if source || (n.Type != "audiosocket" && n.Type != "audiosocket_ws") {
			return bad("uuid applies only to AudioSocket endpoints")
		}

		if _, err := parseAudioUUID(n.UUID); err != nil {
			return bad(err.Error())
		}
	}

	if n.EarlyMedia != "" && !slices.Contains([]string{"disabled", "native", "answer"}, n.EarlyMedia) {
		return bad("early_media must be disabled, native, or answer")
	}

	if n.EarlyMedia == "native" && n.Type != "sip" && n.Type != "inproc" &&
		!(n.Extended && (n.Type == "audiosocket" || n.Type == "audiosocket_ws")) {
		return bad("native early media requires SIP, inproc, or extended AudioSocket")
	}

	if n.Type == "exec" && n.EarlyMedia != "" && n.EarlyMedia != "disabled" {
		return bad("exec endpoints do not signal early media")
	}

	if n.Source != "" && (n.Bind != "" || n.TLS != nil || n.Advertise != nil || len(n.Transports) > 0) {
		return bad("configure bind, TLS, advertisement, and transports on the reused source")
	}

	if n.Bind != "" {
		if _, _, err := splitAddress(n.Bind); err != nil {
			return bad("bind must be host:port")
		}
	}

	if n.Type == "exec" {
		for _, name := range n.PassEnvironment {
			if name == "" || strings.ContainsAny(name, "=\x00") {
				return bad("invalid pass_environment variable name")
			}
		}

		for name, value := range n.Environment {
			if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
				return bad("invalid environment variable")
			}
		}

		if n.IO == "" {
			n.IO = "pcm"
		}

		if !slices.Contains([]string{"pcm", "raw", "audiosocket"}, n.IO) {
			return bad("io must be pcm, raw, or audiosocket")
		}

		if n.Control != "" && n.Control != "json_fd" {
			return bad("control must be json_fd")
		}

		if len(n.Codecs) > 0 {
			return bad("exec uses transcode.codec rather than a codec negotiation list")
		}

		if n.Auth != nil || n.TLS != nil {
			return bad("exec does not use network authentication or TLS")
		}
	} else if len(n.Command) > 0 || n.IO != "" || n.Control != "" ||
		len(n.PassEnvironment) > 0 || len(n.Environment) > 0 {
		return bad("command, environment, io, and control apply only to exec endpoints")
	}

	if n.Extended && !strings.HasPrefix(n.Type, "audiosocket") && !(n.Type == "exec" && n.IO == "audiosocket") {
		return bad("extended applies only to AudioSocket")
	}

	if n.RTP != nil && n.Type != "sip" {
		return bad("rtp applies only to SIP")
	}

	if n.ICE != nil && n.Type != "webrtc_http" {
		return bad("ice applies only to WebRTC")
	}

	if n.OutboundProxy != "" {
		if n.Type != "sip" {
			return bad("outbound_proxy applies only to SIP")
		}

		var proxy sip.Uri
		if err := sip.ParseUri(n.OutboundProxy, &proxy); err != nil ||
			(proxy.Scheme != "sip" &&
				proxy.Scheme != "sips") ||
			proxy.Host == "" ||
			proxy.User != "" ||
			proxy.Password != "" ||
			proxy.Port < 0 ||
			proxy.Port > 65535 ||
			len(proxy.Headers) != 0 ||
			strings.ContainsAny(n.OutboundProxy, "\r\n<>") {
			return bad("outbound_proxy must be a SIP URI without user information or headers")
		}

		transport, _ := proxy.UriParams.Get("transport")
		if transport != "" && !slices.Contains([]string{"udp", "tcp", "tls"}, transport) {
			return bad("unsupported outbound proxy transport")
		}

		if proxy.Scheme == "sips" &&
			(transport != "" &&
				transport != "tls" &&
				transport != "tcp" ||
				n.Transport != "" &&
					n.Transport != "tls") {
			return bad("sips outbound proxy requires TLS")
		}

		if transport != "" &&
			n.Transport != "" &&
			transport != n.Transport &&
			!(proxy.Scheme == "sips" &&
				transport == "tcp" &&
				n.Transport == "tls") {
			return bad("outbound proxy transport conflicts with transport")
		}

		if len(n.Transports) > 0 && !slices.Contains(n.Transports, sipNetwork(n)) {
			return bad("outbound proxy transport must be enabled in transports")
		}

		if strings.HasPrefix(n.URI, "sips:") && sipNetwork(n) != "tls" {
			return bad("sips destination requires TLS to the outbound proxy")
		}

		if n.Registration != nil && strings.HasPrefix(n.Registration.Registrar, "sips:") && sipNetwork(n) != "tls" {
			return bad("sips registrar requires TLS to the outbound proxy")
		}
	}

	for _, codec := range n.Codecs {
		if (n.Type == "sip" || n.Type == "webrtc_http") && codec != "pcmu" && codec != "pcma" && codec != "opus" {
			return bad("SIP and WebRTC support pcmu, pcma, and opus")
		}

		if strings.HasPrefix(n.Type, "audiosocket") {
			return bad("AudioSocket format is selected with transcode, not codecs")
		}
	}

	if t := n.Transcode; t != nil {
		if t.Codec == "" && (t.SampleRate != 0 || t.Channels != 0) {
			return bad("sample_rate and channels require transcode.codec")
		}

		if t.Codec != "" {
			switch n.Type {
			case "sip", "webrtc_http":
				if t.Codec != "pcmu" && t.Codec != "pcma" && t.Codec != "opus" {
					return bad("SIP/WebRTC target codec must be pcmu, pcma, or opus")
				}
			case "audiosocket", "audiosocket_ws":
				if t.Codec != "pcm_s16le" {
					return bad("AudioSocket requires pcm_s16le")
				}
			case "exec":
				if (n.IO == "pcm" || n.IO == "audiosocket") && t.Codec != "pcm_s16le" {
					return bad("selected exec I/O requires pcm_s16le")
				}
			}
		}
	}

	if n.Type == "sip" {
		if slices.Contains(n.Transports, "tcp") && slices.Contains(n.Transports, "tls") {
			_, port, err := splitAddress(n.Bind)
			if err == nil && port != 0 {
				return bad("SIP TCP and TLS require separate fixed listen ports; use separate sources")
			}
		}

		for _, kind := range append(append([]string{}, n.Transports...), n.Transport) {
			if kind != "" && !slices.Contains([]string{"udp", "tcp", "tls"}, kind) {
				return bad("SIP transport must be udp, tcp, or tls")
			}
		}

		if n.Transport != "" && len(n.Transports) > 0 {
			return bad("specify transport or transports, not both")
		}

		if !source {
			var uri sip.Uri
			if err := sip.ParseUri(n.URI, &uri); err != nil || uri.Host == "" {
				return bad("invalid SIP uri")
			}
		}
	}

	if !source && (n.Type == "audiosocket_ws" || n.Type == "webrtc_http") {
		u, err := url.Parse(n.URL)
		if err != nil || u.Host == "" || u.User != nil {
			return bad("invalid endpoint URL; use auth for credentials")
		}

		if n.Type == "audiosocket_ws" && u.Scheme != "ws" && u.Scheme != "wss" {
			return bad("AudioSocket WebSocket URL must use ws or wss")
		}

		if n.Type == "webrtc_http" && u.Scheme != "http" && u.Scheme != "https" {
			return bad("WebRTC URL must use http or https")
		}
	}

	if !source && n.Type == "audiosocket" {
		if _, _, err := splitAddress(strings.TrimPrefix(n.Address, "tcp://")); err != nil {
			return bad("AudioSocket address must be host:port")
		}
	}

	if n.TLS != nil {
		if _, err := makeTLS(n.TLS, source); err != nil {
			return fmt.Errorf("%s TLS: %w", n.Name, err)
		}
	}

	if a := n.Auth; a != nil {
		if source {
			if a.Credential != nil || a.Method != "" {
				return bad("source auth uses mode, sources, and credential_set")
			}
		} else {
			if a.Credential == nil {
				return bad("endpoint auth requires credential")
			}

			if a.Mode != "" || a.CredentialSet != "" || len(a.Sources) > 0 {
				return bad("endpoint auth uses credential and method")
			}

			switch n.Type {
			case "audiosocket", "audiosocket_ws":
				method := a.Method
				if method == "" {
					if n.Type == "audiosocket" {
						method = "basic_auth"
					} else {
						method = "http_basic"
					}
				}

				if method != "basic_auth" && method != "http_basic" {
					return bad("invalid AudioSocket auth method")
				}

				if method == "basic_auth" && !n.Extended {
					return bad("JSON basic_auth requires extended=true")
				}

				if method == "http_basic" && n.Type != "audiosocket_ws" {
					return bad("HTTP Basic requires WebSocket transport")
				}
			case "webrtc_http":
				if a.Method != "" && a.Method != "http_basic" {
					return bad("WebRTC uses http_basic")
				}
			case "sip":
				if a.Method != "" && a.Method != "sip_digest" {
					return bad("SIP uses digest authentication")
				}
			}
		}
	}

	for _, a := range []*AdvertiseConfig{n.Advertise, func() *AdvertiseConfig {
		if n.RTP != nil {
			return n.RTP.Advertise
		}

		return nil
	}()} {
		if a == nil {
			continue
		}

		if !slices.Contains([]string{"", "auto", "bind", "explicit"}, a.Mode) {
			return bad("advertise.mode must be auto, bind, or explicit")
		}

		if a.Mode == "explicit" && a.Address == "" && a.BaseURL == "" {
			return bad("explicit advertisement requires an address or base_url")
		}

		if a.Mode != "explicit" && (a.Address != "" || a.BaseURL != "") {
			return bad("explicit addresses require advertise.mode=explicit")
		}
	}

	if a := n.Advertise; a != nil && a.Mode == "explicit" {
		if n.Type == "sip" {
			if _, _, err := splitAddress(a.Address); err != nil {
				return bad("SIP advertised address must be host:port")
			}
		} else if n.Type == "webrtc_http" {
			u, err := url.Parse(a.BaseURL)
			if err != nil ||
				u.Host == "" ||
				(u.Scheme != "http" &&
					u.Scheme != "https") ||
				u.User != nil ||
				u.RawQuery != "" ||
				u.Fragment != "" ||
				u.Path != "" &&
					u.Path != "/" {
				return bad("advertised base_url must be an HTTP(S) origin")
			}
		} else {
			return bad("advertise applies to SIP or WebRTC")
		}
	}

	if r := n.RTP; r != nil {
		if r.Keepalive != "" {
			d, err := time.ParseDuration(r.Keepalive)
			if err != nil || d < 0 || d > 0 && d < 20*time.Millisecond {
				return bad("RTP keepalive must be zero (disabled) or at least 20ms")
			}
		}

		if r.STUNServer != "" {
			if _, err := parseRTPSTUN(r.STUNServer); err != nil {
				return bad(err.Error())
			}

			if r.Advertise != nil && r.Advertise.Mode != "" && r.Advertise.Mode != "auto" {
				return bad("RTP stun_server cannot override bind or explicit advertisement")
			}
		}

		if r.Bind != "" && net.ParseIP(r.Bind) == nil {
			return bad("RTP bind must be an IP")
		}

		if len(r.PortRange) == 2 {
			start := r.PortRange[0]
			if start%2 != 0 {
				start++
			}

			if start+1 > r.PortRange[1] {
				return bad("RTP port_range must include an even RTP port and following RTCP port")
			}
		}

		if r.Advertise != nil && r.Advertise.Mode == "explicit" && net.ParseIP(r.Advertise.Address) == nil {
			return bad("RTP advertised address must be an IP")
		}
	}

	if i := n.ICE; i != nil {
		if i.Bind != "" && net.ParseIP(i.Bind) == nil {
			return bad("ICE bind must be an IP")
		}

		for _, m := range i.NATMappings {
			if net.ParseIP(m.Local) == nil || net.ParseIP(m.Public) == nil {
				return bad("ICE NAT mappings require IP addresses")
			}
		}

		for _, s := range i.Servers {
			if len(s.URLs) == 0 {
				return bad("ICE server requires URLs")
			}

			for _, value := range s.URLs {
				u, err := url.Parse(value)
				if err != nil || !slices.Contains([]string{"stun", "stuns", "turn", "turns"}, u.Scheme) {
					return bad("invalid STUN/TURN URL")
				}

				if strings.HasPrefix(u.Scheme, "turn") && s.Credential == "" {
					return bad("TURN server requires credentials")
				}
			}
		}
	}

	return nil
}

func validateTimeoutSemantics(t *TimeoutConfig, l *LimitConfig) error {
	if t != nil {
		for _, s := range []string{t.Connect, t.Handshake, t.Identity, t.Dial, t.Shutdown} {
			if s != "" && duration(s, 0) <= 0 {
				return fmt.Errorf("connection, handshake, identity, dial, and shutdown timeouts must be positive")
			}
		}
	}

	if l != nil {
		if l.MaxIdentityBytes > 0 && l.MaxIdentityBytes < 16 {
			return fmt.Errorf("max_identity_bytes must be at least 16")
		}

		if l.MediaQueueDuration != "" && duration(l.MediaQueueDuration, 0) < 20*time.Millisecond {
			return fmt.Errorf("media_queue_duration must be at least 20ms")
		}
	}

	return nil
}

// parseAudioUUID accepts the standard hyphenated form without constraining
// version bits, so configured identities are preserved exactly on the wire.
func parseAudioUUID(s string) ([16]byte, error) {
	var id [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return id, fmt.Errorf("uuid must use xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx format")
	}

	compact := s[:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	if _, err := hex.Decode(id[:], []byte(compact)); err != nil {
		return id, fmt.Errorf("uuid must contain hexadecimal digits")
	}

	return id, nil
}
