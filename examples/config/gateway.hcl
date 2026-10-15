source "gateway" {
  type     = "audiosocket_ws"
  bind     = "127.0.0.1:8080"
  path     = "/dial"
  extended = true

  auth {
    mode    = "passthrough"
    sources = ["http_basic", "basic_auth"]
  }
}

endpoint "provider" {
  type = "sip"
  uri  = "sip:provider.example.net"

  # If the provider uses a separate outbound proxy:
  # outbound_proxy = "sip:proxy.example.net:5060;transport=tcp"

  auth {
    credential = request.auth.credential
  }

  transcode {
    codec = "pcmu"
  }

  dtmf {
    receive = ["rfc4733", "sip_info"]
    send    = "rfc4733"
  }
}

route "dial" {
  match {
    source = source.gateway

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
