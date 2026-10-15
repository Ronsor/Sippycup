# Two-way WebRTC HTTP -> SIP, with early audio in the normal WebRTC stream.
# Replace the demonstration credentials and provider URI before deployment.
# POST an SDP offer to /calls?destination=%2B18005550100 using HTTP Basic auth.
# Encode '+' as %2B in query strings. This is the two-way signaling profile,
# not standard playback-only WHEP. Offers must include PCMU or PCMA (no Opus).
credential "browser" {
  username = "demo"
  password = "replace-browser-password"
}

credential_set "browsers" {
  credentials = [credential.browser]
}

credential "provider" {
  username = "replace-provider-username"
  password = "replace-provider-password"
}

source "browser" {
  type        = "webrtc_http"
  bind        = "127.0.0.1:8080"
  path        = "/calls"
  codecs      = ["pcmu", "pcma"]
  early_media = "answer"

  auth {
    mode           = "verify"
    sources        = ["http_basic"]
    credential_set = credential_set.browsers
  }

  ice {
    trickle = true
  }
}

endpoint "provider" {
  type        = "sip"
  uri         = "sip:provider.example.net"
  early_media = "native"
  codecs      = ["pcmu", "pcma"]

  auth { credential = credential.provider }
}

route "dial" {
  match {
    source = source.browser

    capture "number" {
      value   = request.http.query.destination
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
    reason = "Expected one destination query parameter in +digits format"
  }
}

# A SIP 183/180 with SDP allows an early HTTP 201 SDP answer. Dialing continues
# while the browser hears early audio. The Location resource accepts authenticated
# PATCH/DELETE requests. A later SIP failure closes the session; it cannot change
# the already-sent HTTP status. Use early_media = "disabled" on the source to
# wait for the final SIP answer instead. Native mode is unavailable for WebRTC.
# Configure TLS and ICE STUN/TURN for your deployment; no ICE servers are set here.
