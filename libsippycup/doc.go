// Package libsippycup implements an HCL-configured audio gateway.
// Config describes the gateway; Validate prepares it for NewEngine, which creates an engine,
// whose Serve method runs until cancellation. DialCommand connects a subprocess
// to an endpoint. The audiosocket package is independently reusable.
//
// Build with CGO_ENABLED=0 go build. In an exported workspace with a placeholder
// .git directory, use go build -buildvcs=false instead. Run `sippycup example`
// for an echo configuration, or `sippycup example gateway` for a credential-
// passing AudioSocket-to-SIP gateway. `check --config FILE` validates HCL without
// opening sockets. `serve --config FILE` runs it until SIGINT or SIGTERM.
// `sippycup example sip-to-audiosocket` bridges incoming SIP calls to a TCP
// AudioSocket server. For a local echo server, run `sippycup listen --protocol
// audiosocket --bind 127.0.0.1:9019 -- cat` in another terminal.
//
// # Browser calls
//
// Open a webrtc_http or audiosocket_ws source URL in a browser to get a call
// page with microphone audio, a DTMF keypad, mute, and hangup. HTML is served
// for GET/HEAD requests preferring text/html, after origin and HTTP auth checks.
// SDP submissions, WebSocket upgrades, and session resources keep their normal
// behavior. The page and scripts are embedded; no external asset service is used.
//
// Microphone access requires HTTPS or localhost. WebRTC uses bidirectional
// PCMU/PCMA or opt-in Opus and native DTMF; AudioSocket WebSocket uses 8 kHz PCM and native DTMF
// packets. The destination field supplies the destination query parameter and,
// for extended AudioSocket, the JSON destination field. Extended identities also
// allow arbitrary JSON metadata and optional basic_auth credentials. Browser
// HTTP authentication uses the endpoint's existing Basic authentication challenge.
//
// WebRTC ICE servers for the browser may be entered on the page. Server-side ICE
// configuration remains independent; TURN credentials are not exposed by the page.
// Native extended AudioSocket displays early/answered state notifications. Other
// modes cannot reliably distinguish an answered call from an open audio stream.
//
// # Configuration
//
// Named source and endpoint blocks have a type attribute. Source types are sip,
// audiosocket, audiosocket_ws, webrtc_http, and inproc; endpoints additionally
// support exec. Inproc sources accept Go-supplied legs through Engine.Accept;
// inproc endpoints use Node.InProcess callbacks. NewLegPair supplies bounded
// in-memory media and StatefulLeg early/answered notifications.
// A SIP source can register with a registrar. An endpoint can reuse a SIP
// source's signaling transport with source = source.NAME. Outgoing destinations
// are SIP usernames at the configured endpoint domain, never arbitrary URLs.
// SIP outbound_proxy="sip:proxy.example:5060;transport=tcp" adds a preloaded
// loose Route while preserving the endpoint URI domain and authentication URI.
// A source's proxy applies to REGISTER and is inherited by endpoints reusing
// that source, unless they specify their own proxy. auth.credential handles both
// server (401) and proxy (407) challenges using the same credential. Dialog
// requests follow the returned Record-Route set; a proxy must Record-Route to
// remain on the dialog path. Only loose outbound routing is supported.
// HTTP sources with the same bind address share one server and distinct paths.
// Server-wide TLS, handshake timeout, and connection limits must agree.
//
// Routes run in declaration order. match.source or match.sources select the
// ingress, captures apply Go regular expressions, and match.when evaluates an
// optional HCL boolean expression. Missing capture fields do not match. Captures
// expose .value and named groups. A route has one connect or reject action.
// connect supports endpoint, destination, identity (outbound AudioSocket JSON),
// metadata (exec control start event), and timeout. The expression context has
// request.id, source, remote, identity, sip, http, and auth. Passwords and inbound
// Authorization headers are excluded. request.auth.credential is an opaque
// reference usable by endpoint auth. No matching route rejects the request.
//
// credential blocks contain username and exactly one of password, password_hash,
// or digest { realm, algorithm, ha1 }. bcrypt, sha256crypt, and sha512crypt hashes
// verify HTTP and AudioSocket Basic auth. SIP digest accepts a password or HA1
// for MD5/SHA-256. credential_set groups entries with unique usernames. env("X")
// loads an environment variable and fails if it is unset. SIP sources support
// none/verify authentication; other sources also support passthrough. Source
// authentication sources are http_basic and/or basic_auth (extended AudioSocket).
// Outbound credentials need a password, or a realm/algorithm-specific SIP HA1.
// SIP challenges use signed one-minute nonces bound to the signaling IP, realm,
// and algorithm. Issuing unauthenticated challenges allocates no replay state.
// Successfully authenticated nonces enter a bounded, single-use replay cache
// per credential username, so one account cannot fill another's replay cache.
//
// # Media
//
// All media traverses the daemon. Supported formats are mono PCMU, PCMA, Opus,
// and signed 16-bit little-endian PCM at AudioSocket sample rates. Codecs are
// implemented in pure Go. transcode.mode is auto, always, or never. always requires
// codec and optionally sample_rate/channels. A codec without mode means always.
// Network RTP protocols negotiate PCMU/PCMA and opt-in Opus; AudioSocket carries PCM.
// Opus encoding uses 48 kHz mono and 20 ms packets, with opus.bitrate defaulting
// to 32000. Each Opus Frame holds one complete packet. No FEC or DTX is enabled.
// Exec io=pcm requires PCM; io=raw preserves the selected encoded format; and
// io=audiosocket carries framed AudioSocket over stdin/stdout. Without a fixed
// exec format, its input format is inherited from the incoming leg.
//
// DTMF receive/send directions are relative to the daemon. Native DTMF is RTP
// telephone-event for SIP/WebRTC, digit packets for AudioSocket, and JSON for
// exec with json_fd control. SIP also supports sip_info. inband uses Goertzel
// detection on received audio and synthesized tones on transmitted audio; none
// disables event processing but does not remove tones from audio. rfc2833 is an
// alias for rfc4733. The bridge paces audio in 20 ms blocks and resamples with a
// streaming windowed-sinc filter. Internal queues are bounded; TCP/exec readers
// apply backpressure. RTP loss/reordering is not concealed by an adaptive jitter
// buffer in this version.
// SIP INFO digits use a separate ordered queue of at most 32 pending digits,
// with a two-second delivery timeout per digit. Failed deliveries are logged;
// audio continues. Queue overflow terminates the call rather than growing memory.
//
// # Subprocess contract
//
// A fresh exec process is started for each call. Children inherit only PATH,
// SystemRoot, WINDIR, TMPDIR, TMP, TEMP, LANG, LC_ALL, and TZ, plus generated
// call/media variables. Use
// pass_environment=["HOME", "APP_SETTING"] to inherit additional names and
// environment={APP_MODE="call"} for explicit values. Configured values cannot
// override CALL_ID or AUDIO_* metadata. Daemon credential variables are not
// inherited implicitly.
//
// stdin receives audio, stdout sends audio, and stderr is inherited. command
// is an argument vector, without an implicit shell.
// CALL_ID, AUDIO_CODEC, AUDIO_SAMPLE_RATE, and AUDIO_CHANNELS
// describe the stream. With control="json_fd", fd 3 receives newline-delimited
// JSON events and fd 4 sends newline-delimited commands. A start event includes
// call identity, format, and configured metadata. DTMF messages have type="dtmf"
// and digit="5"; type="hangup" ends a call. Peer closure, process EOF, failures,
// and deadlines close both legs and reap the subprocess group. Exec process
// group cancellation is available on Unix. Other platforms cancel the direct
// child only and reject json_fd control, which requires inherited Unix file
// descriptors.
//
// # AudioSocket extension
//
// Standard KindID contains a 16-byte binary UUID, regardless of its first byte.
// With extended=true, JSON KindID must start with '{', exceed 16 bytes, and include
// a uuid string in canonical hyphenated format. Other JSON fields are optional.
// Payloads are bounded by 65535 bytes and limits.max_identity_bytes.
// Outgoing extended identities include uuid as a canonical
// UUID string (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx), even with a custom identity
// expression. This reserved field uses the generated connection UUID and overrides
// any uuid value in the identity expression. Set uuid on an AudioSocket endpoint
// to use a fixed UUID for every connection (binary or JSON); otherwise a fresh
// random UUID is generated. Fixed UUIDs use the hyphenated form shown above.
// basic_auth is {"username":"...","password":"..."};
// it is removed before exposing identity to routes or subprocess metadata.
// One complete AudioSocket packet occupies one WebSocket binary message.
//
// # Early media
//
// Sources and endpoints accept early_media="disabled", "native", or "answer".
// Disabled suppresses recognized early media until the endpoint answers. Native
// preserves the distinction between provisional media and an answered call.
// Answer accepts the source leg when early media starts and carries that media
// in its normal audio stream. A protocol without call-state signaling cannot
// distinguish downstream early audio from answered audio on an endpoint leg.
//
// Defaults are native for SIP, answer for AudioSocket, and disabled for WebRTC.
// SIP endpoints recognize SDP-bearing provisional responses, including 180 and
// 183. Native SIP sources send 183 with their own SDP; answer-mode SIP sources
// send 200 instead. Disabling early media on either leg suppresses the bridge.
// Per-node transcoding, DTMF, and NAT settings still apply. Pending-call limits
// and the original dial deadline remain in force until the endpoint answers.
//
// A WebRTC HTTP source with early_media="answer" returns its 201 SDP response
// as soon as early media is available and continues dialing in the background.
// Its authenticated Location supports PATCH and DELETE while dialing. A later
// failure closes the peer connection and removes the session; an HTTP error
// cannot be returned after 201. Without early media, ordinary setup errors are
// still returned through HTTP. ICE connection timeout starts at the HTTP answer.
//
// AudioSocket native mode requires extended=true and explicit configuration on
// both peers. KindState (0x02) is a sippycup extension with a one-byte payload:
// 0 means early and 1 means answered. A native server sends early before early
// audio, then answered before answered audio, or answered directly. Duplicate
// states are tolerated; a transition back to early is rejected. Native
// endpoints wait for answered before completing setup, under the dial timeout.
// Ordinary AudioSocket uses no state messages. Enabling extended JSON identities
// alone does not enable call-state signaling or require these messages.
//
// The final SDP can change the remote media address or codec. A final response
// without SDP reuses the provisional answer only within the same dialog.
// Ordinary provisional responses are supported; 100rel/PRACK, early SIP INFO,
// locally generated ringback, and concurrent forked early dialogs are not.
//
// # HTTP WebRTC profile
//
// POST source.path with application/sdp and a bidirectional audio offer returns
// 201, application/sdp, and a Location under path/sessions/ID. The answer contains
// gathered candidates. When ice.trickle=true, PATCH that Location with either
// application/trickle-ice-sdpfrag or application/json (an ICECandidateInit).
// DELETE closes it. Each request is authenticated; session credentials must
// match the creating request. This is a custom two-way profile, not WHEP. ICE
// restart and SDP renegotiation are not implemented. Browser origins default to
// same-origin and can be explicitly allowed with origins=["https://example.com"].
//
// # Networking and limits
//
// bind controls local sockets; advertise mode is auto, bind, or explicit. SIP
// uses advertise.address (host:port), RTP uses a separate IP advertisement, and
// WebRTC HTTP uses advertise.base_url. Auto chooses a local route address and
// performs no public-IP discovery. SIP signaling binds should be explicit on
// multihomed hosts.
// SIP rtp.symmetric defaults to true, enabling one-time address/port learning from the
// SDP media IP or SIP signaling peer IP. A changed address requires two valid,
// sequential RTP packets from the same SSRC with negotiated payload types.
// Return RTP uses the learned address; RTCP continues to use the SDP address.
// Set symmetric=false for strict SDP source checking. This does not discover a
// public address or create router port forwards.
// SDP RTP/RTCP addresses must be numeric unicast IPs matching their declared
// address family; remote SDP does not trigger DNS resolution. Public signaling
// cannot initiate media to private, loopback, or link-local targets. RTP to such
// a target waits for a valid packet from that endpoint; RTCP remains suppressed.
// Local/private signaling retains the corresponding local/private media paths.
// Trusted topologies can opt out with rtp.allow_private_destinations=true.
// SIP sends two startup silence packets if application audio has not opened
// the path, then sends silence when idle for rtp.keepalive (default "15s").
// Set keepalive="0s" to disable. These packets do not reset receive-idle timers
// or begin media before a provisional or final SDP answer is received.
// Optional rtp.stun_server="stun:stun.example.net:3478" discovers public RTP
// and RTCP mappings using the actual media sockets, including translated ports.
// Both discoveries share a three-second deadline; failure rejects setup rather
// than silently advertising an undiscovered mapping. Use local advertisement
// for a LAN-facing source and STUN or explicit advertisement for Internet legs.
// STUN cannot guarantee traversal of endpoint-dependent mappings or NATs without
// suitable hairpin routing. SIP ICE/TURN is not implemented; such deployments
// need a reachable media relay/SBC or configured port forwarding. WebRTC has
// separate ICE/TURN support. SIP signaling advertisement remains separate.
// ICE has bind, interfaces, port_range, policy=all/relay, STUN/TURN server blocks,
// nat_mapping { local, public }, and include_loopback=false by default.
// An explicit loopback bind also enables loopback candidates;
// set include_loopback=true for other local deployments.
// TLS supports PEM certificate/private_key files, CA roots, server_name, minimum version, and
// client_ca_file for mutual TLS. SIP TLS encrypts signaling, not RTP.
//
// Runtime defaults are 100 calls, 20 pending calls, 200 connections, 100 child
// processes, 8192-byte identities, 65536-byte HTTP bodies, and 200 ms media queues.
// Calls include setup; a two-leg bridge counts once globally and once against
// each participating node. Local overrides never bypass global caps. Omitted
// or zero numeric limits inherit defaults. Timeouts are quoted Go durations:
// connect=5s, handshake=10s, identity=5s, dial=30s, media_idle=60s,
// max_call_duration=2h, shutdown=5s. Only media_idle and max_call_duration accept
// zero to disable. Media idle means no received media, not silence detection.
// Shutdown bounds exec pipe cleanup after exit or cancellation; it does not
// impose an overall deadline on Engine.Close.
//
// This is not a registrar or PBX: conferencing, REFER, SIP re-INVITE/hold,
// multi-stream SDP, SIP SRTP, and codec renegotiation are explicitly unsupported.
//
// # Tests
//
// Run go test ./... for the self-contained suite. Tests use loopback sockets
// and the Go test executable as an audio subprocess; no shell, cat, or ffmpeg
// executable is required. Socket tests need permission to bind loopback ports.
// Asterisk interoperability is excluded unless the asterisk build tag is set:
//
//	SIPPYCUP_ASTERISK=asterisk go test -tags=asterisk ./...
//
// SIPPYCUP_ASTERISK accepts an executable path or name. Set
// SIPPYCUP_ASTERISK_MODULE_DIR when its compiled module path does not match the
// installation. The test writes an isolated temporary PBX configuration and
// skips when SIPPYCUP_ASTERISK is unset or go test -short is used.
package libsippycup
