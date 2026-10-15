# CLI Usage

[← Sippycup](../README.md)

```sh
# Run the configured sources and routes
sippycup serve --config calls.hcl

# Parse and validate configuration without running
sippycup check --config calls.hcl

# Print an embedded example configuration (by default, the echo example)
sippycup example [name]

# Start one subprocess for each incoming call, like `inetd` or `nc -le`
sippycup listen [flags] -- command [args]

# Connect a subprocess to an outbound endpoint, like `nc -e`
sippycup dial [flags] -- command [args]
```

### Lifecycle

Sending SIGTERM or SIGINT quits Sippycup. You can also send SIGHUP to reload the supplied
configuration file:

```sh
kill -HUP <pid>
```

The file is read again, including `env()` lookups. New calls use the updated configuration;
existing calls keep their original routing, credentials, and media settings until they end.
SIP dialogs and registrations stay running, and existing WebRTC session URLs remain usable
with their original authentication. Resource counters are retained across reloads.

Routes, endpoints, credentials, source media settings, limits, and HTTP paths can be updated
live. Changing the source set, source names or types, bind addresses, listener TLS settings,
SIP transport/advertisement/registration settings, or HTTP handshake timeouts requires a
restart. Listener certificate files are loaded at startup, so replacing their contents also
requires a restart. SIP registration credential changes take effect on the next refresh.

A parse error, validation error, or restart-required change rejects the entire reload and
leaves the current configuration and calls running. Check the logs for the result.

`listen` and `dial` do not support SIGHUP reloads.

### Quick Tools

For a configuration-free AudioSocket echo server:

```sh
sippycup listen --protocol audiosocket --bind 127.0.0.1:9019 -- cat
```

To make a call through a configured SIP endpoint:

```sh
sippycup dial --config calls.hcl --endpoint provider --to +18005550100 -- ./handler
```

Or connect directly to an AudioSocket server:

```sh
sippycup dial --protocol audiosocket --to 127.0.0.1:9019 -- ./handler
```

The CLI handler receives mono signed 16-bit little-endian PCM on stdin and writes PCM to stdout.
The default sample rate is 8000 Hz; use `--sample-rate` to change it. Logs and debug output are
sent to `stderr`.

Note that Sippycup does not start a shell for commands.

See also [configuration](configuration.md) and [subprocess endpoints](applications.md).
