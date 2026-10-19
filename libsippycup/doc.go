// Package libsippycup routes calls between SIP, AudioSocket, WebRTC, subprocesses,
// and in-process Go applications. The engine proxies all audio and supports
// transcoding, DTMF conversion, authentication, and early media.
//
// # Engine lifecycle
//
// Build a [Config], call [Config.Validate], and pass it to [NewEngine]. HCL
// parsing and command-line handling belong to the sippycup CLI; Go applications
// can construct configuration directly.
//
// [Engine.Serve] starts the engine and runs until its context is canceled or
// [Engine.Close] is called. It closes resources before returning. Applications
// that manage their own lifecycle can use [Engine.Start] and [Engine.Close]
// instead. Do not call both Start and Serve on the same engine.
//
// [Engine.Reload] applies a fresh configuration to new calls while existing
// calls keep their original settings. Listener and transport changes that
// require a restart are rejected. See Reload for the configuration ownership
// rules and restrictions.
//
// # Sources, endpoints, and routes
//
// Sources accept calls; endpoints make calls. Both use [Node] configuration
// with a type of sip, audiosocket, audiosocket_ws, webrtc_http, or inproc.
// Endpoints also support exec to run a subprocess for each call. A SIP source
// can register with a remote registrar and accept calls through that account.
//
// Routes run in declaration order, matching incoming request information and
// optionally extracting values with regular expressions. Each route connects
// to an endpoint or rejects the call. Unmatched calls are rejected. Credentials,
// media policies, timeouts, and resource limits are configured per node, with
// global limits bounding total usage.
//
// # In-process calls
//
// A [Leg] carries audio frames and DTMF events. [StatefulLeg] adds early-media
// and answered notifications. [NewLegPair] creates connected legs with bounded
// in-memory queues for applications that do not need a network transport.
//
// After starting the engine, pass a leg and [Request] to [Engine.Accept] to
// route a call through an inproc source. Accept takes ownership of the leg and
// blocks until the call ends. For an inproc endpoint, set [Node.InProcess] to an
// [EndpointFunc] that creates a fresh leg for each call. The callback's context
// covers setup; [EndpointRequest.CallContext] lasts for the entire call.
//
// # Audio and DTMF
//
// Supported audio formats are mono PCMU/PCMA and mono or stereo Opus and
// signed 16-bit little-endian PCM. Codecs and resampling use pure Go.
// Transcoding can be automatic, forced to a configured format, or disabled.
//
// SIP and WebRTC default to PCMU and PCMA; Opus is opt-in. Opus encoding uses
// 48 kHz audio in 20 ms packets, with a default bitrate of 32000 bits per
// second. Mono is the default; stereo is opt-in. Each Opus [Frame] contains
// one complete packet. AudioSocket carries mono PCM, or stereo PCM in extended
// mode. Opus FEC, DTX, and adaptive jitter buffering are not enabled.
//
// DTMF receive and send policies are relative to the engine and configurable
// independently for each node. Native events use RTP telephone-event for SIP
// and WebRTC, digit packets for AudioSocket, and JSON control for subprocesses.
// SIP INFO and in-band tone detection and synthesis are also supported.
//
// Early media can be disabled, carried with native call-state signaling, or
// delivered by answering the source early. SIP and extended AudioSocket can
// preserve early and answered states. WebRTC HTTP and ordinary AudioSocket
// can carry early audio after accepting the source connection.
//
// # Network protocols
//
// AudioSocket supports TCP and WebSocket transports. Extended mode accepts
// JSON identities containing a UUID and application metadata, and optionally
// carries call-state messages. The separate [audiosocket] package provides
// reusable framing, dialing, and listening without the routing engine.
//
// WebRTC uses a custom bidirectional HTTP signaling profile, not WHEP. An SDP
// POST creates a session; PATCH adds ICE candidates and DELETE closes it.
// HTTP sources can share a bind address with distinct paths. Browser requests
// preferring HTML receive an embedded call page with microphone audio and a
// DTMF keypad. Microphone access requires HTTPS or localhost.
//
// SIP supports digest authentication, outbound proxies, and symmetric RTP.
// Public RTP address discovery via STUN is optional; automatic advertisement
// alone chooses a local address. WebRTC has separate ICE and TURN settings.
// SIP TLS protects signaling, not RTP.
//
// The engine is not a PBX or registrar. Conferencing, REFER, SIP re-INVITE and
// hold, SIP SRTP, and SDP renegotiation are not supported.
//
// # Further documentation
//
// The repository contains detailed guides for [configuration], [media],
// [networking], [subprocess endpoints], and [WebRTC signaling]. CLI commands
// and examples are described in the [CLI guide].
//
// [audiosocket]: https://pkg.go.dev/github.com/ronsor/sippycup/audiosocket
// [configuration]: https://github.com/ronsor/sippycup/blob/master/docs/configuration.md
// [media]: https://github.com/ronsor/sippycup/blob/master/docs/media.md
// [networking]: https://github.com/ronsor/sippycup/blob/master/docs/networking.md
// [subprocess endpoints]: https://github.com/ronsor/sippycup/blob/master/docs/applications.md
// [WebRTC signaling]: https://github.com/ronsor/sippycup/blob/master/docs/webrtc.md
// [CLI guide]: https://github.com/ronsor/sippycup/blob/master/docs/cli.md
package libsippycup
