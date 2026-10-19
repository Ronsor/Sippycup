# audiosocket

[← Sippycup](../README.md)

A reusable Go implementation of Asterisk AudioSocket over TCP/TLS and WebSocket.
It can wrap existing connections or dial and listen itself. It does not depend on
the Sippycup engine or its HCL configuration.

## Dialing

Import `github.com/ronsor/sippycup/audiosocket` independently of the engine. It
provides `Wrap` for existing connections, `Dial` and `Listen` for TCP/TLS and
WS/WSS, identity parsing, and audio/DTMF packet helpers. For example, in a function
with a context `ctx`:

```go
conn, err := audiosocket.Dial(ctx, "ws://127.0.0.1:8080/audio",
    audiosocket.WithExtended(true), audiosocket.WithContext(ctx))
if err != nil {
    return err
}
defer conn.Close()

err = conn.WriteIdentity(audiosocket.Identity{
    JSON: json.RawMessage(`{"uuid":"550e8400-e29b-41d4-a716-446655440000","destination":"+18005550100"}`),
})
if err != nil {
    return err
}
```

The snippet uses `encoding/json` and returns errors from its containing function.
After sending the identity, use `WriteAudio(rate, pcm)` for mono signed 16-bit
little-endian PCM, `WriteDTMF(digit)` for digits, and `ReadPacket()` for incoming
frames. `WritePacket` can send a `KindHangup` packet to signal hangup before closing.

For standard AudioSocket, omit `WithExtended(true)` and provide the 16 UUID bytes
in `Identity.UUID`. The library does not generate a UUID or add one to supplied
JSON: callers supply the complete identity. `ParseIdentity` populates `Identity.UUID`
for either format and preserves a copy of the JSON when present.

## Listening and Wrapping

`Listen` accepts `tcp://`, `tls://`, `ws://`, and `wss://` addresses. A WebSocket URL
selects the request path. TLS listeners require a certificate through `WithTLS`.
For example, inside a function with context `ctx`:

```go
listener, err := audiosocket.Listen("tcp://127.0.0.1:9019",
    audiosocket.WithExtended(true), audiosocket.WithContext(ctx))
if err != nil {
    return err
}
defer listener.Close()

conn, err := listener.Accept(ctx)
if err != nil {
    return err
}
defer conn.Close()

packet, err := conn.ReadPacket()
if err != nil {
    return err
}
if packet.Kind != audiosocket.KindID {
    return fmt.Errorf("expected an AudioSocket identity")
}
identity, err := audiosocket.ParseIdentity(packet.Payload, true)
if err != nil {
    return err
}
fmt.Printf("connection UUID bytes: %x\n", identity.UUID)
```

This example uses `fmt` and accepts one connection. A server normally loops over
`Accept` and handles each connection independently. Accepted connections remain
open when the listener closes, unless their own context is canceled.

Use `Wrap(net.Conn, ...)` for an existing stream, `WrapWebSocket(MessageConn, ...)`
for an existing WebSocket, or `Handler(callback, ...)` to mount AudioSocket on your
HTTP server. The callback owns the connection. Apply authentication middleware
before `Handler`; the library does not authenticate callers itself.

## Connection Behavior

- Wrapping transfers ownership: `Close` closes the underlying transport.
- Connections support one reader and concurrent serialized writers.
- The context passed to `Dial` bounds connection setup. Use `WithContext` to bind
  the connection lifetime; canceling that context interrupts I/O.
- `SetDeadline` applies to stream connections. Use context cancellation for WebSockets.
- `WithMaxPayload` bounds frame payloads; the wire maximum is 65535 bytes.
- `Wrap` tries to enable `TCP_NODELAY`, including through TLS and wrappers exposing
  `NetConn`. Socket-option errors are ignored. Opaque WebSocket transports retain
  their HTTP stack's socket settings.
- `WithTLS`, `WithHTTPClient`, and `WithHTTPHeader` configure outgoing TLS and HTTP
  handshakes. Do not mutate supplied TLS/client configuration during use.

`Conn.ReadPacket` and `Conn.WritePacket` validate known packet types. The package-level
`ReadPacket` and `WritePacket` helpers provide low-level framing over readers/writers.

## Extended Identities and WebSocket Framing

Standard AudioSocket starts with a KindID packet containing a 16-byte UUID.
With `extended = true`, a JSON KindID must start with `{`, exceed 16 bytes, and
contain a `uuid` string in standard hyphenated UUID format. Every 16-byte payload
is treated as a binary UUID, even if its first byte is `{`. Other payloads are
rejected.

Example JSON identity:

```json
{
  "uuid": "550e8400-e29b-41d4-a716-446655440000",
  "destination": "+18005550100",
  "basic_auth": {"username": "alice", "password": "secret"}
}
```

JSON uses the normal three-byte AudioSocket frame header. The wire payload limit
is 65535 bytes; the engine additionally applies its configured identity limit. Over
WebSocket, each binary message contains exactly one complete AudioSocket packet,
including its header; the subprotocol name is `audiosocket`.

The required `uuid` is a string in `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` format.
Additional metadata is application-defined. `Identity.BasicAuth()` extracts the
optional credentials without verifying or removing them; authentication and redaction
are the application's responsibility.

AudioSocket over WebSocket uses the `audiosocket` subprotocol and is distinct from
Asterisk's `chan_websocket` protocol.

## Stereo PCM

Stereo PCM is a Sippycup extension requiring `WithExtended(true)` on both peers.
The standard mono kinds `0x10`–`0x18` are unchanged. Stereo kinds `0x20`–`0x28`
use the same rate order: 8000, 12000, 16000, 24000, 32000, 44100, 48000, 96000,
and 192000 Hz. Each payload contains interleaved signed 16-bit little-endian
samples in left/right order, with a length divisible by four. The three-byte
header and WebSocket framing are unchanged; the length counts payload bytes.

Use `conn.WritePCM(rate, 2, pcm)` to send stereo and `PCMFormat(packet.Kind)` to
read its rate and channel count. `PCMKind(rate, channels)` constructs either
mono or stereo packet kinds. Existing `WriteAudio`, `AudioKind`, and `AudioRate`
retain their mono semantics. `Conn` rejects stereo in standard mode and rejects
incomplete PCM sample frames on reads and writes. The low-level free functions
`ReadPacket` and `WritePacket` still only handle framing.

Extended mode permits stereo but does not negotiate it or require it. Configure
stereo output explicitly on both applications as needed; mono packets are still
accepted. An ordinary Asterisk AudioSocket peer does not support these kinds.

## Native Early Media

Native early-media signaling adds KindState (`0x02`) with a one-byte payload:
`0` means early and `1` means answered. Both peers must explicitly enable this
profile. `extended = true` alone does not require state messages. These extensions
are specific to Sippycup and are not part of standard Asterisk AudioSocket.

`WriteState(StateEarly)` and `WriteState(StateAnswered)` send these messages.
Applications manage call-state transitions; the library does not automatically
answer a connection or decide when early audio becomes answered audio.

## Sippycup Engine Integration

The following behavior belongs to the [engine](../libsippycup/README.md), not the
raw AudioSocket library:

The incoming JSON `uuid` field is required; other fields are optional. `basic_auth`
is removed before metadata is exposed to routes. Outgoing extended identities
automatically include `uuid`, even with a custom `connect.identity`. Set `uuid = "550e8400-e29b-41d4-a716-446655440000"`
on an AudioSocket endpoint to use that fixed UUID on every connection; otherwise
a fresh UUID is generated. The endpoint UUID overrides any `uuid` in the custom
identity expression and applies to binary identities too.

The engine's default identity limit is 8192 bytes, configurable with
`limits.max_identity_bytes`. See the [metadata forwarding example](../examples/config/audiosocket-metadata.hcl),
[SIP dialing proxy](../examples/config/audiosocket-to-sip.hcl), and
[early-media guide](../docs/media.md).

## Tests and API Reference

From the repository root:

```sh
go test ./audiosocket
go doc ./audiosocket
```

Tests use Go and loopback sockets; no external server or executable is required.
See the [executable examples](example_test.go) and [API source](audiosocket.go).
