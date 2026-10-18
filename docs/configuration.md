# Configuration

[← Sippycup](../README.md)

## Supported Protocols

| Type | Source | Endpoint | Notes |
| --- | --- | --- | --- |
| `sip` | Incoming calls, optionally with registrar registration | Outbound calls | UDP, TCP, TLS signaling; RTP media; digest auth and outbound proxies. |
| `audiosocket` | TCP listener | TCP connection | Standard binary UUID or opt-in extended JSON identity. |
| `audiosocket_ws` | HTTP WebSocket upgrade | WebSocket connection | AudioSocket packets over WS/WSS; not Asterisk `chan_websocket`. |
| `webrtc_http` | HTTP SDP offer | HTTP SDP offer to another server | Custom bidirectional audio signaling; PCMU/PCMA and opt-in Opus. |
| `exec` | — | Subprocess | PCM, raw encoded audio, or framed AudioSocket over stdio. |

The WebRTC HTTP profile is a modified version of WHIP/WHEP signaling where calls carry two-way
audio. In all cases, media is relayed through Sippycup.

## Configuration

Configuration uses HCL.

* A `source` accepts incoming calls. SIP registration also belongs to a source,
  even though the REGISTER exchange is outbound.
* An `endpoint` describes the other side of a call: a remote service or application.
* A `route` matches request information and connects an endpoint or rejects the call.
* `credential` and `credential_set` blocks describe authentication.
* `runtime` supplies default timeouts and resource limits.

Protocols are selected using the `type` attribute inside a source or endpoint: `type = "sip"`,
for example.

Here's an example of a SIP-to-AudioSocket bridge that passes the caller's username:

```hcl
source "sip" {
  type = "sip"
  bind = "127.0.0.1:5060"
}

endpoint "audio" {
  type     = "audiosocket"
  address  = "127.0.0.1:9019"
  extended = true
}

route "sip_to_audio" {
  match { source = source.sip }

  connect {
    endpoint = endpoint.audio
    identity = {
      caller_username = request.sip.from
    }
  }
}
```

The AudioSocket peer receives JSON containing `caller_username` and an automatic `uuid`. The
username comes from the SIP From URI. See the [complete example](../examples/config/sip-to-audiosocket.hcl).

### Routing and Metadata

Routes are evaluated in declaration order; the first match wins. A route without `match` is a
fallback. A route has exactly one `connect` or `reject` action; if no matching route is found, the
call is rejected.

Expressions can use `request.id`, `request.source`, `request.remote`, `request.identity`,
`request.sip`, `request.http`, and `request.auth`. A match can select `source` or `sources`, apply
regular-expression `capture` blocks, and test a boolean `when`. Captures expose `.value` and named
groups. Missing capture fields cause the route not to match.

For example, inside a route matching an extended AudioSocket source:

```hcl
match {
  capture "number" {
    value   = request.identity.destination
    pattern = "^\\+(?P<digits>[1-9][0-9]{7,14})$"
  }
}

connect {
  endpoint    = endpoint.provider
  destination = "+${capture.number.digits}"
  timeout     = "30s"
}
```

SIP destinations are usernames at the configured endpoint domain, not arbitrary SIP URLs.
`connect.identity` builds outgoing AudioSocket JSON. `connect.metadata` supplies metadata for an
exec control start event. Reusable passwords and HTTP authorization headers are excluded from
request expressions.

### Credentials and SIP Providers

For a SIP provider, define a credential and reference it from the endpoint:

```hcl
credential "provider" {
  username = "your-account"
  password = env("SIP_PASSWORD")
}

endpoint "provider" {
  type           = "sip"
  uri            = "sip:provider.example.net"
  outbound_proxy = "sip:proxy.example.net:5060;transport=udp"

  auth { credential = credential.provider }
}
```

`env()` reads the environment at configuration load time and fails if the variable
is unset. `auth.credential` handles both server (401) and proxy (407) challenges.
An outbound proxy changes the next hop while preserving the destination domain.
Only loose proxy routing is supported.

A SIP source can maintain registration using a `registration` block with `registrar`, `aor`, and
`credential`. An endpoint with `source = source.NAME` reuses that source's SIP signaling transport
and inherits its proxy unless overridden.

Incoming authentication uses `auth.mode = "verify"` and a `credential_set`. HTTP sources accept
`http_basic`; extended AudioSocket accepts JSON `basic_auth`. AudioSocket WS can use either. SIP
uses SIP digest authentication.

A credential contains exactly one of `password`, `password_hash`, or
`digest { realm, algorithm, ha1 }`. bcrypt, sha256crypt, and sha512crypt hashes can verify
HTTP/AudioSocket Basic authentication, but outbound authentication needs a reusable password, or a
SIP HA1 for the matching realm and algorithm; a verification hash cannot be forwarded as a
password.

Non-SIP sources also support `auth.mode = "passthrough"`. This passes supplied credentials to an
endpoint using `auth { credential = request.auth.credential }` and does not verify them locally.
See the [TCP proxy](../examples/config/audiosocket-to-sip.hcl) and [WebSocket gateway](../examples/config/gateway.hcl)
examples.

See also [media and DTMF](media.md), [networking and limits](networking.md),
and the [configuration types](../libsippycup/config.go).
