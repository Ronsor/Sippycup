# Subprocess Endpoints

[← Sippycup](../README.md)

An `exec` endpoint starts a fresh process for each call. `io = "pcm"` uses PCM,
`io = "raw"` uses the selected encoded format, and `io = "audiosocket"` uses framed
AudioSocket. Use `transcode` to fix the application's format. Stdin is received
audio, stdout is transmitted audio, and stderr is inherited.

The environment includes `CALL_ID`, `AUDIO_CODEC`, `AUDIO_SAMPLE_RATE`, and
`AUDIO_CHANNELS`, plus a small set of system variables. Use `pass_environment` to
inherit additional variables or `environment` to supply explicit values. Daemon
credential variables are not inherited automatically.

On Unix, `control = "json_fd"` provides newline-delimited JSON events on fd 3 and
accepts commands on fd 4. Events include call metadata and digits;
`{"type":"dtmf","digit":"5"}` sends a digit and `{"type":"hangup"}` ends the call.
Process groups are terminated on Unix; other platforms terminate the direct
child and do not support `json_fd`.

See also [CLI listen and dial](cli.md#quick-tools), [media formats](media.md),
and the [Go engine library](../libsippycup/README.md).
