# Development

[← Sippycup](../README.md)

Run the self-contained Go suite and static checks:

```sh
go test ./...
go vet ./...
```

Tests need loopback socket access but do not require Asterisk, ffmpeg, `cat`, or
other external executables. Asterisk interoperability tests are optional and
require both the build tag and an explicit executable:

```sh
SIPPYCUP_ASTERISK=/opt/asterisk/sbin/asterisk \
SIPPYCUP_ASTERISK_MODULE_DIR=/opt/asterisk/lib/asterisk/modules \
go test -tags=asterisk -run TestAsteriskInteroperability ./libsippycup
```

The Asterisk tests skip when the executable variable is unset or `-short` is used.
`CGO_ENABLED=0 go build` builds without C dependencies.

## Limitations

Current limitations include SIP SRTP, SIP ICE/TURN, conferencing, registrar
service, REFER, SIP re-INVITE/hold, multi-stream SDP, and codec renegotiation.
Early media does not support 100rel/PRACK, concurrent forked dialogs, early SIP INFO,
or locally generated ringback. RTP loss and reordering have no adaptive jitter
buffer or packet-loss concealment in this version.

See also the [engine library](../libsippycup/README.md).

Opus tests include checked-in libopus tone fixtures and SIP/WebRTC loopback calls.
To additionally check Pion-encoded audio with FFmpeg (optional):

```sh
CGO_ENABLED=0 SIPPYCUP_FFMPEG=/usr/bin/ffmpeg go test -run TestOpusFFmpegDecode ./libsippycup
```

Without `SIPPYCUP_FFMPEG`, or with `-short`, that external-tool test skips.
