# WebRTC HTTP Signaling

[← Sippycup](../README.md)

The `webrtc_http` source and endpoint use a custom bidirectional audio profile,
not standard playback-only WHEP. Offers must negotiate PCMU, PCMA, or explicitly enabled Opus.

The browser page is served for GET/HEAD requests preferring `text/html`. SDP
requests and WebSocket upgrades retain their protocol behavior. Authentication
and origin checks apply to the page and its assets.

For a programmatic WebRTC call:

1. POST a bidirectional audio SDP offer to the source path with
   `Content-Type: application/sdp`.
2. Read the `201` SDP answer and the `Location` session URL.
3. If `ice.trickle = true`, PATCH the session with `application/trickle-ice-sdpfrag`
   or an `application/json` ICE candidate.
4. DELETE the session to hang up.

Session requests must authenticate as the caller that created the session. The
browser's destination field supplies a `destination` query parameter; extended
AudioSocket also receives it in JSON metadata. ICE restart and renegotiation are
not supported.

See also [early media](media.md) and [ICE configuration](networking.md).
