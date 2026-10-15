# Extract and forward selected JSON metadata to another AudioSocket server.
# Incoming KindID JSON example:
# {"uuid":"550e8400-e29b-41d4-a716-446655440000","destination":"+18005550100","tenant":"demo","ignored":"not forwarded"}
# UUID-only clients remain valid AudioSocket peers, but do not match this route.
source "audio" {
  type     = "audiosocket"
  bind     = "127.0.0.1:9019"
  extended = true
}

endpoint "upstream" {
  type     = "audiosocket_ws"
  url      = "ws://127.0.0.1:9020/audio"
  extended = true
}

route "forward" {
  match {
    source = source.audio

    capture "number" {
      value   = request.identity.destination
      pattern = "^\\+(?P<digits>[1-9][0-9]{7,14})$"
    }

    capture "tenant" {
      value   = request.identity.tenant
      pattern = "^(?P<name>[a-z0-9_-]{1,32})$"
    }
  }

  connect {
    endpoint = endpoint.upstream
    identity = {
      destination = "+${capture.number.digits}"
      tenant      = capture.tenant.name
    }
  }
}

route "invalid_metadata" {
  reject {
    code   = 400
    reason = "Expected destination and tenant in the JSON identity"
  }
}

# This copies selected fields, not the complete incoming identity. tenant is
# caller-supplied routing metadata, not proof of authentication or authorization.

# The outgoing JSON identity also includes an automatically generated uuid
# in standard xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx format.
