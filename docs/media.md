# Audio, DTMF, and Early Media

[← Sippycup](../README.md)

Supported formats are mono PCMU, PCMA, Opus, and signed 16-bit little-endian PCM at AudioSocket
sample rates. SIP and WebRTC can negotiate Opus; AudioSocket carries PCM.
Codec conversion uses pure Go and does not require CGO or ffmpeg.

Opus is opt-in: add it to a SIP or WebRTC node's codec list. The default remains PCMU/PCMA.

```hcl
codecs = ["opus", "pcmu", "pcma"]

opus {
  bitrate = 32000
}
```

The optional `opus` block configures locally encoded audio (6000–510000 bits/s; default 32000).
It also applies to in-process nodes. Encoding uses mono 48 kHz and 20 ms packets.
Incoming packets may use other Opus frame durations and SILK, hybrid, or CELT modes;
stereo input is decoded to mono when transcoding. SDP uses `opus/48000/2`, as required
by the RTP format, while requesting mono operation.

Compatible mono Opus packets pass through intact unless transcoding or in-band DTMF requires
decoding. Packet duration drives pacing and RTP timestamps. `transcode { codec = "opus" }`
forces encoding; its sample rate, if supplied, must be 48000. Bitrate settings do not
alter packets passed through without encoding.

Opus uses a pinned development revision of `github.com/pion/opus`; no FEC, DTX, jitter
buffer, or packet-loss concealment is enabled by the gateway. Exec endpoints receive
PCM when inheriting an Opus input format; raw subprocess byte streams cannot delimit
Opus packets. In-process `Frame.Data` must contain one complete Opus packet.

A source or endpoint can select `transcode.mode = "auto"`, `"always"`, or `"never"`. To force a
format on an endpoint:

```hcl
transcode {
  mode        = "always"
  codec       = "pcm_s16le"
  sample_rate = 8000
  channels    = 1
}
```

DTMF is configured separately on each source and endpoint. `receive` describes what the daemon
accepts; `send` describes what it sends to that peer. Available styles depend on the protocol:
`native`, `rfc4733` (`rfc2833` alias), `sip_info`, `inband`, and `none`. In-band reception uses
Goertzel detection and transmission synthesizes tones. `none` disables digit-event processing,
not tones already in audio.

```hcl
dtmf {
  receive = ["rfc4733", "sip_info"]
  send    = "rfc4733"
}
```

Both source and endpoint blocks accept `early_media`:

| Value | Behavior |
| --- | --- |
| `disabled` | Suppress recognized pre-answer audio until the endpoint answers. |
| `native` | Preserve early/answered call states. Supported by SIP and explicitly enabled extended AudioSocket peers. |
| `answer` | Accept the source leg early and carry provisional audio in the normal stream. |

Defaults are `native` for SIP, `answer` for AudioSocket, and `disabled` for WebRTC. Disabling early
media on either leg suppresses the early-media bridge. The dial deadline remains in force until the
endpoint answers.

With WebRTC HTTP `early_media = "answer"`, the server may return its SDP answer before the SIP call
is answered. In this case, a subsequent SIP failure closes the session. Protocols without
call-state signaling cannot distinguish early audio from answered audio.

SIP BYE, peer closure, setup failure, subprocess EOF, and applicable deadlines close the bridge and
its resources.

See also [AudioSocket state signaling](../audiosocket/README.md#native-early-media)
and [WebRTC HTTP signaling](webrtc.md).
