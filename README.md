# Sippycup

[![Go Reference](https://pkg.go.dev/badge/github.com/ronsor/sippycup.svg)](https://pkg.go.dev/github.com/ronsor/sippycup)

Sippycup is a signaling and audio gateway for several telephony (and adjacent) protocols:

* SIP + RTP
* WebRTC via a bidirectional version of the WHIP/WHEP protocol
* Asterisk's AudioSocket protocol and an extended version of such
* The AudioSocket protocol over WebSockets
* Regular applications with raw PCM over stdio

Sippycup removes the need for a heavyweight PBX like Asterisk; it's only capable of proxying
media and routing connections using basic rules.

You can use it to accept incoming calls using SIP, operate a WebRTC-to-SIP gateway, and much
more.

## Quick Start

First, build Sippycup using Go 1.26 or newer:

```sh
go build -o sippycup .
```

Next, run the basic audio echo example:

```sh
# Note: this example requires cat(1)
./sippycup example > calls.hcl
./sippycup check --config calls.hcl
./sippycup serve --config calls.hcl
```

You can now visit <http://127.0.0.1:8080/calls> (WebRTC) or <http://127.0.0.1:8080/audio>
(AudioSocket/WS) to initiate a call. Whatever you speak will be echoed back to you. Both pages
provide microphone audio, a DTMF keypad, mute, and hangup. **Please note microphone access
requires a secure origin (HTTPS or localhost).**

> [!IMPORTANT]
> The examples listen on loopback. Configure addresses, authentication, and TLS before
> using them as shared services.

For a configuration-free AudioSocket echo server, you can also run:

```sh
./sippycup listen --protocol audiosocket --bind 127.0.0.1:9019 -- cat
```

Run `./sippycup help` or `./sippycup <command> --help` for command options.

## Examples

All files in [examples/config](examples/config) are embedded in the binary. Print one with
`sippycup example NAME > calls.hcl`, edit it, and run `check` before starting the server.

| Name | Example |
| --- | --- |
| `echo` | [SIP-independent AudioSocket and WebRTC echo service](examples/config/echo.hcl). Requires `cat`. |
| `sip-to-audiosocket` | [Incoming SIP with caller username in AudioSocket metadata](examples/config/sip-to-audiosocket.hcl). |
| `audiosocket-to-sip` | [JSON destination extraction and per-call SIP credential passthrough](examples/config/audiosocket-to-sip.hcl). |
| `gateway` | [AudioSocket WebSocket-to-SIP gateway](examples/config/gateway.hcl). |
| `audiosocket-metadata` | [Validate and forward selected JSON metadata](examples/config/audiosocket-metadata.hcl). |
| `sip-early-media` | [Native early media between SIP and an extended AudioSocket application](examples/config/sip-early-media.hcl). |
| `webrtc-early-media` | [Answer WebRTC HTTP early while SIP dialing continues](examples/config/webrtc-early-media.hcl). |

## Documentation

- [CLI usage and configuration reloads](docs/cli.md)
- [Configuration, routing, and authentication](docs/configuration.md)
- [Audio, DTMF, and early media](docs/media.md)
- [Networking, TLS, ICE, and resource limits](docs/networking.md)
- [WebRTC HTTP signaling](docs/webrtc.md)
- [Subprocess endpoints](docs/applications.md)
- [Development and limitations](docs/development.md)

## Go Libraries

- [`libsippycup`](libsippycup/README.md): embed the gateway engine and its browser call pages.
- [`audiosocket`](audiosocket/README.md): use AudioSocket TCP/WS independently, including the JSON extensions.
