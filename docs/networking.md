# Networking and Resource Limits

[← Sippycup](../README.md)

`bind` selects local sockets. `advertise` controls addresses placed in signaling; SIP RTP has its
own `rtp.advertise` settings. Advertisement mode `auto` selects a local route address, but does not
discover the public IP behind NAT.

SIP `rtp.symmetric` defaults to `true`, allowing the daemon to learn a return address from valid
media sent by the SDP media IP or signaling peer. It does not create port forwards. Use
`rtp.stun_server` to discover RTP/RTCP mappings from the actual media sockets, or configure
explicit advertisement and forwarding. SIP ICE/TURN is not implemented; some NAT topologies require
a reachable relay/SBC.

See [networking details](../libsippycup/doc.go) for source-learning rules and private media destination
restrictions.

WebRTC has independent ICE configuration for bind addresses, interfaces, port ranges, STUN/TURN
servers, NAT mappings, and `policy = "all"` or `"relay"`. No STUN/TURN server is supplied by default.
Loopback candidates are disabled unless explicitly enabled or a loopback ICE bind is selected.

TLS blocks support certificate/key files, CA roots, server names, minimum TLS versions, and client
certificates. SIP TLS protects signaling, not RTP. HTTP source paths can share one bind address when
server-level TLS, handshake timeout, and connection limits agree. Browser origins default to
same-origin, but `origins` can explicitly allow additional origins.

Limits and timeouts can be set globally in `runtime`, with per-node overrides. Local overrides do not
bypass global limits. Durations are strings such as `"30s"`; zero numeric limits inherit defaults.

| Resource | Default |
| --- | --- |
| Calls, including setup | 100 |
| Pending calls | 20 |
| Connections | 200 |
| Child processes | 100 |
| Identity payload | 8192 bytes |
| HTTP request body | 65536 bytes |
| Media queue | 200 ms |

Default timeouts are connect `"5s"`, handshake `"10s"`, identity `"5s"`, dial `"30s"`, media idle
`"60s"`, maximum call duration `"2h"`, and subprocess shutdown `"5s"`. Only media idle and maximum
call duration accept `"0s"` to disable the limit. Media idle means no received media, not detected
silence.

See also [authentication](configuration.md#credentials-and-sip-providers) and [WebRTC](webrtc.md).
