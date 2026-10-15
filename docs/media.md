# Audio, DTMF, and Early Media

[← Sippycup](../README.md)

Supported formats are mono PCMU, PCMA, and signed 16-bit little-endian PCM at AudioSocket sample
rates. RTP protocols always negotiate PCMU/PCMA, while AudioSocket carries PCM.

Opus is not currently supported. Codec conversion does not require CGO or ffmpeg.

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
