# Incoming SIP -> an extended AudioSocket application with native call states.
# The application must implement sippycup's KindState extension. This is not
# compatible with an unmodified Asterisk AudioSocket application in native mode.
source "sip" {
  type        = "sip"
  bind        = "127.0.0.1:5060"
  codecs      = ["pcmu", "pcma"]
  early_media = "native"
}

endpoint "application" {
  type        = "audiosocket"
  address     = "127.0.0.1:9019"
  extended    = true
  early_media = "native"

  transcode {
    mode        = "always"
    codec       = "pcm_s16le"
    sample_rate = 8000
  }
}

route "application" {
  match { source = source.sip }

  connect {
    endpoint = endpoint.application
    timeout  = "30s"
  }
}

# After receiving the JSON identity, the application sends KindState (0x02)
# with the one-byte payload 0 before early PCM. The SIP caller gets 183 with SDP.
# When the application sends KindState with payload 1, the SIP caller gets 200.
# The dial timeout still applies while the application is sending early media.
# Source early_media = "answer" sends SIP 200 as early audio becomes available.
# Source early_media = "disabled" discards early audio until the answered state.

# The outgoing JSON identity also includes an automatically generated uuid
# in standard xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx format.
