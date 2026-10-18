package libsippycup

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// Config describes the gateway. Call Validate before passing it to NewEngine.
// Request-dependent expressions are evaluated when calls arrive. A frontend may
// supply EvalContext for static references and functions.
type Config struct {
	Credentials    []*Credential    `hcl:"credential,block"`
	CredentialSets []*CredentialSet `hcl:"credential_set,block"`
	Sources        []*Node          `hcl:"source,block"`
	Endpoints      []*Node          `hcl:"endpoint,block"`
	Routes         []*Route         `hcl:"route,block"`
	Runtime        *RuntimeConfig   `hcl:"runtime,block"`

	// EvalContext supplies static variables and functions for route expressions.
	// It may be nil when all expressions are self-contained.
	EvalContext *hcl.EvalContext
}

// Credential defines a username and exactly one password, verification hash,
// or realm-specific SIP digest HA1 value.
type Credential struct {
	Name         string        `hcl:"name,label"`
	Username     string        `hcl:"username"`
	Password     *string       `hcl:"password,optional"`
	PasswordHash string        `hcl:"password_hash,optional"`
	Digest       *DigestConfig `hcl:"digest,block"`
}

// DigestConfig holds a SIP digest HA1 value for one realm and algorithm.
type DigestConfig struct {
	Realm     string `hcl:"realm"`
	Algorithm string `hcl:"algorithm"`
	HA1       string `hcl:"ha1"`
}

// CredentialSet groups named credentials for inbound verification.
// Usernames must be unique within a set.
type CredentialSet struct {
	Name        string   `hcl:"name,label"`
	Credentials []string `hcl:"credentials"`
}

// Node configures either a call source or an endpoint. Type determines which
// protocol-specific fields apply. Sources accept calls; endpoints create call legs.
// EarlyMedia selects disabled, native signaling, or answer-and-stream behavior.
// Defaults are native for SIP, answer for AudioSocket, and disabled for WebRTC.
// Native AudioSocket signaling requires Extended and explicit peer agreement.
// UUID optionally fixes the connection identity on TCP/WS AudioSocket endpoints.
// Exec endpoints inherit a minimal environment; PassEnvironment adds inherited
// names and Environment adds explicit values without exposing all daemon secrets.
type Node struct {
	// InProcess creates one leg per call for an inproc endpoint. It is Go-only
	// and must be attached before validation. Sources use Engine.Accept instead.
	InProcess EndpointFunc

	Name            string              `hcl:"name,label"`
	Type            string              `hcl:"type"`
	Bind            string              `hcl:"bind,optional"`
	Path            string              `hcl:"path,optional"`
	URL             string              `hcl:"url,optional"`
	URI             string              `hcl:"uri,optional"`
	OutboundProxy   string              `hcl:"outbound_proxy,optional"`
	Address         string              `hcl:"address,optional"`
	Source          string              `hcl:"source,optional"`
	Transport       string              `hcl:"transport,optional"`
	Transports      []string            `hcl:"transports,optional"`
	UUID            string              `hcl:"uuid,optional"`
	Extended        bool                `hcl:"extended,optional"`
	EarlyMedia      string              `hcl:"early_media,optional"`
	Codecs          []string            `hcl:"codecs,optional"`
	Command         []string            `hcl:"command,optional"`
	PassEnvironment []string            `hcl:"pass_environment,optional"`
	Environment     map[string]string   `hcl:"environment,optional"`
	IO              string              `hcl:"io,optional"`
	Control         string              `hcl:"control,optional"`
	Origins         []string            `hcl:"origins,optional"`
	Auth            *AuthConfig         `hcl:"auth,block"`
	Transcode       *TranscodeConfig    `hcl:"transcode,block"`
	DTMF            *DTMFConfig         `hcl:"dtmf,block"`
	TLS             *TLSConfig          `hcl:"tls,block"`
	Advertise       *AdvertiseConfig    `hcl:"advertise,block"`
	RTP             *RTPConfig          `hcl:"rtp,block"`
	Opus            *OpusConfig         `hcl:"opus,block"`
	ICE             *ICEConfig          `hcl:"ice,block"`
	Registration    *RegistrationConfig `hcl:"registration,block"`
	Timeouts        *TimeoutConfig      `hcl:"timeouts,block"`
	Limits          *LimitConfig        `hcl:"limits,block"`
}

// AuthConfig defines inbound verification or outbound authentication.
// Credential expressions on endpoints may refer to the current request.
type AuthConfig struct {
	Mode          string         `hcl:"mode,optional"`
	Sources       []string       `hcl:"sources,optional"`
	CredentialSet string         `hcl:"credential_set,optional"`
	Credential    hcl.Expression `hcl:"credential,optional"`
	Realm         string         `hcl:"realm,optional"`
	Method        string         `hcl:"method,optional"`
}

// TranscodeConfig selects automatic conversion, a forced output format,
// or rejection of incompatible formats when Mode is never.
type TranscodeConfig struct {
	Mode       string `hcl:"mode,optional"`
	Codec      string `hcl:"codec,optional"`
	SampleRate int    `hcl:"sample_rate,optional"`
	Channels   int    `hcl:"channels,optional"`
}

// DTMFConfig selects received and transmitted digit representations.
// Directions are relative to the gateway and apply independently to each leg.
type DTMFConfig struct {
	Receive []string `hcl:"receive,optional"`
	Send    string   `hcl:"send,optional"`
}

// TLSConfig describes PEM identities, trust roots, and TLS policy.
type TLSConfig struct {
	CertificateFile string `hcl:"certificate_file,optional"`
	PrivateKeyFile  string `hcl:"private_key_file,optional"`
	CAFile          string `hcl:"ca_file,optional"`
	ServerName      string `hcl:"server_name,optional"`
	MinVersion      string `hcl:"min_version,optional"`
	ClientCAFile    string `hcl:"client_ca_file,optional"`
}

// AdvertiseConfig selects the address placed in signaling or media descriptions.
// Auto selects a local route address; it does not discover a public NAT mapping.
type AdvertiseConfig struct {
	Mode    string `hcl:"mode,optional"`
	Address string `hcl:"address,optional"`
	BaseURL string `hcl:"base_url,optional"`
}

// RTPConfig controls SIP media sockets, NAT learning, keepalives, and advertisement.
// A nil Symmetric defaults to true; an explicit false enforces the SDP source.
// AllowPrivateDestinations permits unsolicited local-network media even when
// signaling comes from a public IP. Enable it only for trusted media topologies.
type RTPConfig struct {
	AllowPrivateDestinations bool             `hcl:"allow_private_destinations,optional"`
	Bind                     string           `hcl:"bind,optional"`
	Symmetric                *bool            `hcl:"symmetric,optional"`
	Keepalive                string           `hcl:"keepalive,optional"`
	STUNServer               string           `hcl:"stun_server,optional"`
	PortRange                []int            `hcl:"port_range,optional"`
	Advertise                *AdvertiseConfig `hcl:"advertise,block"`
}

func (r *RTPConfig) symmetricEnabled() bool {
	return r == nil || r.Symmetric == nil || *r.Symmetric
}

// ICEConfig controls WebRTC candidate gathering and connectivity policy.
// It does not apply to SIP RTP sockets.
// Loopback candidates require IncludeLoopback or an explicit loopback Bind.
type ICEConfig struct {
	IncludeLoopback bool          `hcl:"include_loopback,optional"`
	Bind            string        `hcl:"bind,optional"`
	Interfaces      []string      `hcl:"interfaces,optional"`
	PortRange       []int         `hcl:"port_range,optional"`
	Policy          string        `hcl:"policy,optional"`
	Trickle         bool          `hcl:"trickle,optional"`
	Servers         []*ICEServer  `hcl:"server,block"`
	NATMappings     []*NATMapping `hcl:"nat_mapping,block"`
}

// ICEServer identifies STUN or TURN URLs and an optional named TURN credential.
type ICEServer struct {
	Name       string   `hcl:"name,label"`
	URLs       []string `hcl:"urls"`
	Credential string   `hcl:"credential,optional"`
}

// NATMapping rewrites a local WebRTC host candidate to a configured public IP.
type NATMapping struct {
	Local  string `hcl:"local"`
	Public string `hcl:"public"`
}

// RegistrationConfig maintains a SIP registration for a source.
// Registrar is the server URI; AOR is the account address of record.
type RegistrationConfig struct {
	Registrar  string `hcl:"registrar"`
	AOR        string `hcl:"aor"`
	Credential string `hcl:"credential"`
	Expires    string `hcl:"expires,optional"`
}

// TimeoutConfig contains quoted Go duration strings. Empty fields inherit
// runtime defaults. Only MediaIdle and MaxCallDuration may disable their limit
// with a zero duration.
type TimeoutConfig struct {
	Connect         string `hcl:"connect,optional"`
	Handshake       string `hcl:"handshake,optional"`
	Identity        string `hcl:"identity,optional"`
	Dial            string `hcl:"dial,optional"`
	MediaIdle       string `hcl:"media_idle,optional"`
	MaxCallDuration string `hcl:"max_call_duration,optional"`
	Shutdown        string `hcl:"shutdown,optional"`
}

// LimitConfig bounds calls, pending setups, sockets, processes, and buffers.
// Zero numeric fields inherit defaults rather than disabling protection.
type LimitConfig struct {
	MaxCalls           int    `hcl:"max_calls,optional"`
	MaxPendingCalls    int    `hcl:"max_pending_calls,optional"`
	MaxConnections     int    `hcl:"max_connections,optional"`
	MaxProcesses       int    `hcl:"max_processes,optional"`
	MaxIdentityBytes   int    `hcl:"max_identity_bytes,optional"`
	MaxHTTPBodyBytes   int64  `hcl:"max_http_body_bytes,optional"`
	MediaQueueDuration string `hcl:"media_queue_duration,optional"`
}

// RuntimeConfig supplies default timeouts and resource limits for all nodes.
type RuntimeConfig struct {
	Timeouts *TimeoutConfig `hcl:"timeouts,block"`
	Limits   *LimitConfig   `hcl:"limits,block"`
}

// Route matches a request and either connects an endpoint or rejects the call.
// Routes are evaluated in declaration order; the first match wins.
type Route struct {
	Name    string   `hcl:"name,label"`
	Match   *Match   `hcl:"match,block"`
	Connect *Connect `hcl:"connect,block"`
	Reject  *Reject  `hcl:"reject,block"`
}

// Match restricts a route by source, extracted captures, and an optional predicate.
type Match struct {
	Source   string         `hcl:"source,optional"`
	Sources  []string       `hcl:"sources,optional"`
	Captures []*Capture     `hcl:"capture,block"`
	When     hcl.Expression `hcl:"when,optional"`
}

// Capture extracts a value and named regular-expression groups into route context.
type Capture struct {
	Name    string         `hcl:"name,label"`
	Value   hcl.Expression `hcl:"value"`
	Pattern string         `hcl:"pattern"`
	re      *regexp.Regexp
}

// Connect selects an endpoint and evaluates per-call destination or metadata.
type Connect struct {
	Endpoint    string         `hcl:"endpoint"`
	Destination hcl.Expression `hcl:"destination,optional"`
	Identity    hcl.Expression `hcl:"identity,optional"`
	Metadata    hcl.Expression `hcl:"metadata,optional"`
	Timeout     string         `hcl:"timeout,optional"`
}

// Reject terminates routing with a status code and reason.
type Reject struct {
	Code   int    `hcl:"code,optional"`
	Reason string `hcl:"reason,optional"`
}

func optionalExpr(e hcl.Expression) hcl.Expression {
	if e == nil {
		return nil
	}

	v, d := e.Value(nil)
	if !d.HasErrors() && v.IsNull() {
		return nil
	}

	return e
}

func (c *Config) credential(name string) *Credential {
	for _, v := range c.Credentials {
		if v.Name == name {
			return v
		}
	}

	return nil
}

func (c *Config) credentialSet(name string) *CredentialSet {
	for _, v := range c.CredentialSets {
		if v.Name == name {
			return v
		}
	}

	return nil
}

func (c *Config) endpoint(name string) *Node {
	for _, v := range c.Endpoints {
		if v.Name == name {
			return v
		}
	}

	return nil
}

func (c *Config) source(name string) *Node {
	for _, v := range c.Sources {
		if v.Name == name {
			return v
		}
	}

	return nil
}

func duration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}

	v, _ := time.ParseDuration(s)

	return v
}

func checkDurations(t *TimeoutConfig, l *LimitConfig) error {
	values := []string{}
	if t != nil {
		values = append(
			values,
			t.Connect,
			t.Handshake,
			t.Identity,
			t.Dial,
			t.MediaIdle,
			t.MaxCallDuration,
			t.Shutdown,
		)
	}

	if l != nil {
		values = append(values, l.MediaQueueDuration)
		if l.MaxCalls < 0 ||
			l.MaxPendingCalls < 0 ||
			l.MaxConnections < 0 ||
			l.MaxProcesses < 0 ||
			l.MaxIdentityBytes < 0 ||
			l.MaxIdentityBytes > 65535 ||
			l.MaxHTTPBodyBytes < 0 ||
			l.MaxHTTPBodyBytes == math.MaxInt64 {
			return fmt.Errorf("invalid resource limit")
		}
	}

	for _, v := range values {
		if v == "" {
			continue
		}

		d, e := time.ParseDuration(v)
		if e != nil || d < 0 {
			return fmt.Errorf("invalid duration %q", v)
		}
	}

	return validateTimeoutSemantics(t, l)
}

// Validate checks references, protocol constraints, and resource settings.
// It fills configuration defaults and compiles route regular expressions.
func (c *Config) Validate() error {
	// gohcl represents omitted optional expression fields as literal nulls.
	for _, nodes := range [][]*Node{c.Sources, c.Endpoints} {
		for _, n := range nodes {
			if n.Auth != nil {
				n.Auth.Credential = optionalExpr(n.Auth.Credential)
			}
		}
	}

	for _, r := range c.Routes {
		if r.Match != nil {
			r.Match.When = optionalExpr(r.Match.When)
		}

		if r.Connect != nil {
			r.Connect.Destination = optionalExpr(r.Connect.Destination)
			r.Connect.Identity = optionalExpr(r.Connect.Identity)
			r.Connect.Metadata = optionalExpr(r.Connect.Metadata)
		}
	}

	if c.Runtime == nil {
		c.Runtime = &RuntimeConfig{}
	}

	if e := checkDurations(c.Runtime.Timeouts, c.Runtime.Limits); e != nil {
		return e
	}

	for _, v := range c.Credentials {
		if e := validateCredential(v); e != nil {
			return fmt.Errorf("credential %s: %w", v.Name, e)
		}
	}

	for _, s := range c.CredentialSets {
		seen := map[string]bool{}
		for _, name := range s.Credentials {
			cr := c.credential(name)
			if cr == nil {
				return fmt.Errorf("unknown credential %q", name)
			}

			if seen[cr.Username] {
				return fmt.Errorf("duplicate username in credential_set %s", s.Name)
			}

			seen[cr.Username] = true
		}
	}

	for _, list := range [][]*Node{c.Sources, c.Endpoints} {
		for _, n := range list {
			source := c.source(n.Name) == n
			if err := validateNodeShape(n, source); err != nil {
				return err
			}

			if !slices.Contains(
				[]string{"sip", "audiosocket", "audiosocket_ws", "webrtc_http", "exec", "inproc"},
				n.Type,
			) {
				return fmt.Errorf("%s: unknown type %q", n.Name, n.Type)
			}

			if source && n.Type == "exec" {
				return fmt.Errorf("%s: exec is an endpoint type", n.Name)
			}

			if source && n.Type != "inproc" && n.Bind == "" {
				return fmt.Errorf("source %s: bind is required", n.Name)
			}

			if !source {
				switch n.Type {
				case "inproc":
					if n.InProcess == nil {
						return fmt.Errorf("endpoint %s: InProcess callback is required", n.Name)
					}
				case "exec":
					if len(n.Command) == 0 {
						return fmt.Errorf("endpoint %s: command is required", n.Name)
					}
				case "sip":
					if n.URI == "" {
						return fmt.Errorf("endpoint %s: uri is required", n.Name)
					}
				case "audiosocket":
					if n.Address == "" {
						return fmt.Errorf("endpoint %s: address is required", n.Name)
					}
				default:
					if n.URL == "" {
						return fmt.Errorf("endpoint %s: url is required", n.Name)
					}
				}
			}

			if n.Source != "" && (c.source(n.Source) == nil || c.source(n.Source).Type != "sip" || n.Type != "sip") {
				return fmt.Errorf("%s: source must reference a SIP source", n.Name)
			}

			if n.Source != "" {
				signaling := c.source(n.Source)
				effective := *n
				if effective.OutboundProxy == "" {
					effective.OutboundProxy = signaling.OutboundProxy
				}

				if effective.OutboundProxy != "" {
					if err := validateNodeShape(&effective, false); err != nil {
						return err
					}

					enabled := signaling.Transports
					if len(enabled) == 0 {
						enabled = []string{sipNetwork(signaling)}
					}

					if !slices.Contains(enabled, sipNetwork(&effective)) {
						return fmt.Errorf("%s: outbound proxy transport is unavailable on source %s", n.Name, n.Source)
					}
				}
			}

			if e := checkDurations(n.Timeouts, n.Limits); e != nil {
				return fmt.Errorf("%s: %w", n.Name, e)
			}

			for _, codec := range n.Codecs {
				if codec != "pcmu" && codec != "pcma" && codec != "pcm_s16le" && codec != "opus" {
					return fmt.Errorf("%s: unsupported codec %q", n.Name, codec)
				}
			}

			if n.Transcode != nil {
				t := n.Transcode
				if t.Mode == "" {
					if t.Codec != "" {
						t.Mode = "always"
					} else {
						t.Mode = "auto"
					}
				}

				if !slices.Contains([]string{"auto", "always", "never"}, t.Mode) {
					return fmt.Errorf("%s: invalid transcode mode", n.Name)
				}

				if t.Mode == "always" && t.Codec == "" {
					return fmt.Errorf("%s: always transcode requires codec", n.Name)
				}

				if t.Codec != "" {
					if _, e := formatFor(t.Codec, t.SampleRate, t.Channels); e != nil {
						return fmt.Errorf("%s: %w", n.Name, e)
					}
				}
			}

			if e := validateDTMF(n); e != nil {
				return fmt.Errorf("%s: %w", n.Name, e)
			}

			for _, r := range []*[]int{func() *[]int {
				if n.RTP != nil {
					return &n.RTP.PortRange
				}

				return nil
			}(), func() *[]int {
				if n.ICE != nil {
					return &n.ICE.PortRange
				}

				return nil
			}()} {
				if r != nil && len(*r) > 0 && (len(*r) != 2 || (*r)[0] < 1024 || (*r)[1] > 65535 || (*r)[0] > (*r)[1]) {
					return fmt.Errorf("%s: invalid port_range", n.Name)
				}
			}

			if n.Auth != nil {
				a := n.Auth
				if source {
					if a.Mode == "" {
						a.Mode = "verify"
					}

					if !slices.Contains([]string{"none", "verify", "passthrough"}, a.Mode) ||
						(n.Type == "sip" &&
							a.Mode == "passthrough") {
						return fmt.Errorf("%s: unsupported auth mode", n.Name)
					}

					if a.Mode == "verify" {
						set := c.credentialSet(a.CredentialSet)
						if set == nil {
							return fmt.Errorf("%s: verification requires credential_set", n.Name)
						}

						if n.Type == "sip" {
							for _, ref := range set.Credentials {
								cr := c.credential(ref)
								if cr.Password == nil && cr.Digest == nil {
									return fmt.Errorf("%s: SIP digest cannot use password_hash", n.Name)
								}

								if cr.Digest != nil && cr.Digest.Realm != realm(a) {
									return fmt.Errorf("%s: SIP credential realm mismatch", n.Name)
								}
							}
						}
					}

					if len(a.Sources) == 0 {
						switch n.Type {
						case "audiosocket":
							a.Sources = []string{"basic_auth"}
						case "audiosocket_ws":
							a.Sources = []string{"http_basic"}
						case "webrtc_http":
							a.Sources = []string{"http_basic"}
						}
					}

					for _, s := range a.Sources {
						if a.Mode == "none" {
							continue
						}

						if s == "basic_auth" {
							if !n.Extended || !strings.HasPrefix(n.Type, "audiosocket") {
								return fmt.Errorf("%s: basic_auth requires extended AudioSocket", n.Name)
							}
						} else if s != "http_basic" || (n.Type != "audiosocket_ws" && n.Type != "webrtc_http") {
							return fmt.Errorf("%s: invalid auth source %q", n.Name, s)
						}
					}
				} else if a.Credential != nil {
					if !hasRequest(a.Credential) {
						name, e := exprString(a.Credential, c.EvalContext)
						if e != nil {
							return e
						}

						cr := c.credential(name)
						if cr == nil {
							return fmt.Errorf("%s: unknown credential", n.Name)
						}

						if cr.Password == nil && !(n.Type == "sip" && cr.Digest != nil) {
							return fmt.Errorf("%s: outbound authentication requires a reusable credential", n.Name)
						}
					}
				}
			}

			if n.Registration != nil {
				r := n.Registration
				if !source || n.Type != "sip" {
					return fmt.Errorf("%s: registration belongs to SIP source", n.Name)
				}

				cr := c.credential(r.Credential)
				if cr == nil || cr.Password == nil && cr.Digest == nil {
					return fmt.Errorf("%s: registration requires password or SIP digest credential", n.Name)
				}

				if r.Expires != "" {
					if d, e := time.ParseDuration(r.Expires); e != nil || d <= 0 {
						return fmt.Errorf("%s: invalid registration expires", n.Name)
					}
				}
			}

			if n.ICE != nil {
				if n.ICE.Policy != "" && n.ICE.Policy != "all" && n.ICE.Policy != "relay" {
					return fmt.Errorf("%s: invalid ICE policy", n.Name)
				}

				for _, s := range n.ICE.Servers {
					if s.Credential != "" {
						cr := c.credential(s.Credential)
						if cr == nil || cr.Password == nil {
							return fmt.Errorf("%s: ICE server requires password credential", n.Name)
						}
					}
				}
			}
		}
	}

	for _, r := range c.Routes {
		if (r.Connect == nil) == (r.Reject == nil) {
			return fmt.Errorf("route %s: exactly one connect or reject block required", r.Name)
		}

		if r.Connect != nil {
			if c.endpoint(r.Connect.Endpoint) == nil {
				return fmt.Errorf("route %s: unknown endpoint", r.Name)
			}

			if r.Connect.Timeout != "" {
				if d, e := time.ParseDuration(r.Connect.Timeout); e != nil || d <= 0 {
					return fmt.Errorf("route %s: invalid timeout", r.Name)
				}
			}
		}

		if r.Match != nil {
			m := r.Match
			for _, s := range append(append([]string{}, m.Sources...), m.Source) {
				if s != "" && c.source(s) == nil {
					return fmt.Errorf("route %s: unknown source %q", r.Name, s)
				}
			}

			seen := map[string]bool{}
			for _, cap := range m.Captures {
				if seen[cap.Name] {
					return fmt.Errorf("route %s: duplicate capture", r.Name)
				}

				seen[cap.Name] = true
				re, e := regexp.Compile(cap.Pattern)
				if e != nil {
					return fmt.Errorf("route %s: %w", r.Name, e)
				}

				cap.re = re
			}
		}
	}

	return validateHTTPGroups(c.Sources)
}

func hasRequest(e hcl.Expression) bool {
	for _, v := range e.Variables() {
		if v.RootName() == "request" {
			return true
		}
	}

	return false
}

func exprString(e hcl.Expression, ctx *hcl.EvalContext) (string, error) {
	if e == nil {
		return "", nil
	}

	v, d := e.Value(ctx)
	if d.HasErrors() {
		return "", fmt.Errorf("expression at %s: %s", e.Range(), d.Error())
	}

	if v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return "", fmt.Errorf("expression at %s must be a string", e.Range())
	}

	return v.AsString(), nil
}
