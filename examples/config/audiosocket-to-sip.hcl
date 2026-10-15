# TCP AudioSocket dialing proxy with per-call SIP credentials.
# Run: sippycup example audiosocket-to-sip > calls.hcl
# Replace the provider URI/proxy before starting the server.
#
# Send KindID (0x01) with this JSON payload, followed by PCM packets:
# {
#   "uuid":"550e8400-e29b-41d4-a716-446655440000",
#   "destination":"+18005550100",
#   "basic_auth":{"username":"alice","password":"secret"}
# }
# JSON identities require uuid and must exceed 16 bytes.
# The JSON bytes begin with '{'; the normal AudioSocket packet header remains.
# basic_auth is removed from request.identity and passed only to SIP auth.
# Missing credentials (including UUID-only clients) fail authentication.
# Authenticated requests with missing/invalid destinations reach the reject route.
source "dial" {
  type        = "audiosocket"
  bind        = "127.0.0.1:9019"
  extended    = true
  early_media = "answer"

  # Forward credentials to the fixed provider; do not verify them locally.
  auth {
    mode    = "passthrough"
    sources = ["basic_auth"]
  }

  dtmf {
    receive = ["native"]
    send    = "native"
  }
}

endpoint "provider" {
  type           = "sip"
  uri            = "sip:provider.example.net"
  outbound_proxy = "sip:proxy.example.net:5060;transport=udp"
  early_media    = "native"
  codecs         = ["pcmu", "pcma"]

  auth { credential = request.auth.credential }

  transcode {
    mode  = "always"
    codec = "pcmu"
  }

  dtmf {
    receive = ["rfc4733", "sip_info"]
    send    = "rfc4733"
  }
}

route "dial_number" {
  match {
    source = source.dial

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
}

route "invalid_destination" {
  reject {
    code   = 400
    reason = "Expected destination in international +digits format"
  }
}

# The provider's provisional audio is sent as ordinary AudioSocket PCM.
# No answer notification is sent in answer mode. To distinguish call states,
# change the source to early_media = "native" and enable native mode in the
# client too: KindState (0x02), payload 0 = early, payload 1 = answered.
# To suppress pre-answer audio instead, set early_media = "disabled" on either leg.
