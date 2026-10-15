# Accept direct SIP calls, e.g. sip:echo@127.0.0.1:5060.
# This local example does not require authentication or registration.
source "sip" {
  type       = "sip"
  bind       = "127.0.0.1:5060"
  transports = ["udp", "tcp"]
  codecs     = ["pcmu", "pcma"]

  rtp {
    bind       = "127.0.0.1"
    port_range = [10000, 10100]
  }

  dtmf {
    receive = ["rfc4733", "sip_info"]
    send    = "rfc4733"
  }
}

# A fresh TCP connection is opened for each call.
# Extended AudioSocket: a JSON KindID, followed by PCM and DTMF packets.
# The receiving application must support extended JSON identities.
endpoint "audio" {
  type     = "audiosocket"
  address  = "127.0.0.1:9019"
  extended = true

  # Optional: reuse this UUID for every outbound connection instead of generating one.
  # uuid = "550e8400-e29b-41d4-a716-446655440000"

  transcode {
    mode        = "always"
    codec       = "pcm_s16le"
    sample_rate = 8000
  }

  dtmf {
    receive = ["native"]
    send    = "native"
  }
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

# For From: <sip:alice@example.net>, the JSON identity contains
# {"uuid":"xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx","caller_username":"alice"}.
# request.sip.from is the From URI username asserted by the caller, not an
# authenticated identity. This local example does not verify SIP credentials.

# The outgoing JSON identity also includes an automatically generated uuid
# in standard xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx format.
