# After starting the gateway, open http://localhost:8080/calls for WebRTC
# or http://localhost:8080/audio for AudioSocket WebSocket. Both serve a browser
# call page with microphone audio and a DTMF keypad. Use localhost or HTTPS for
# microphone permission. This echo example starts the external "cat" command.
source "audio" {
  type     = "audiosocket"
  bind     = "127.0.0.1:9019"
  extended = true
}

source "web_audio" {
  type     = "audiosocket_ws"
  bind     = "127.0.0.1:8080"
  path     = "/audio"
  extended = true
}

source "browser" {
  type   = "webrtc_http"
  bind   = "127.0.0.1:8080"
  path   = "/calls"
  codecs = ["pcmu", "pcma"]

  ice {
    trickle = true
  }
}

endpoint "echo" {
  type    = "exec"
  command = ["cat"]
  io      = "pcm"

  transcode {
    mode        = "always"
    codec       = "pcm_s16le"
    sample_rate = 8000
    channels    = 1
  }
}

route "echo" {
  connect {
    endpoint = endpoint.echo
  }
}
