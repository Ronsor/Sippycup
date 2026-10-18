"use strict";

const element = (id) => document.getElementById(id);
const settings = document.body.dataset;
const keypad = [...document.querySelectorAll("[data-digit]")];
let active = null;

function status(message) {
  element("status").textContent = message;
}

function controls(busy, ready = false) {
  element("call").disabled = busy;
  element("hangup").disabled = !busy;
  element("mute").disabled = !ready;
  element("destination").disabled = busy;

  for (const button of keypad) {
    button.disabled = !ready;
  }
}

function callURL() {
  const url = new URL(location.href);
  url.hash = "";
  url.searchParams.delete("sippycup_asset");
  const destination = element("destination").value.trim();

  if (destination) {
    url.searchParams.set("destination", destination);
  } else {
    url.searchParams.delete("destination");
  }

  return url;
}

function assetURL(name) {
  const url = new URL(location.href);
  url.search = "";
  url.hash = "";
  url.searchParams.set("sippycup_asset", name);

  return url;
}

function deleteSession(url) {
  return fetch(url, { method: "DELETE", credentials: "same-origin", keepalive: true }).catch(
    () => {},
  );
}

function finish(call, message) {
  if (active !== call) {
    return;
  }

  active = null;
  clearTimeout(call.timer);
  clearTimeout(call.disconnectTimer);
  call.abort.abort();

  if (call.ws) {
    if (call.ws.readyState === WebSocket.OPEN) {
      call.ws.send(packet(0, new Uint8Array()));
    }

    call.ws.close();
  }

  if (call.pc) {
    call.pc.close();
  }

  if (call.stream) {
    for (const track of call.stream.getTracks()) {
      track.stop();
    }
  }

  if (call.worklet) {
    call.worklet.disconnect();
  }

  if (call.microphone) {
    call.microphone.disconnect();
  }

  if (call.context) {
    void call.context.close().catch(() => {});
  }

  if (call.session) {
    void deleteSession(call.session);
  }

  element("remote").srcObject = null;
  element("mute").textContent = "Mute";
  element("mute").setAttribute("aria-pressed", "false");
  controls(false);
  status(message);
}

function packet(kind, payload) {
  const result = new Uint8Array(payload.length + 3);
  result[0] = kind;
  result[1] = payload.length >> 8;
  result[2] = payload.length & 255;
  result.set(payload, 3);

  return result;
}

function uuidBytes() {
  const id = crypto.getRandomValues(new Uint8Array(16));
  id[6] = (id[6] & 15) | 64;
  id[8] = (id[8] & 63) | 128;

  return id;
}

function identity() {
  const id = uuidBytes();

  if (settings.extended !== "true") {
    return id;
  }

  const metadata = JSON.parse(element("metadata").value);

  if (!metadata || Array.isArray(metadata) || typeof metadata !== "object") {
    throw new Error("Connection metadata must be a JSON object.");
  }

  const hex = [...id].map((byte) => byte.toString(16).padStart(2, "0")).join("");
  metadata.uuid = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
  const destination = element("destination").value.trim();

  if (destination) {
    metadata.destination = destination;
  }

  if (settings.basicAuth === "true" && element("username").value) {
    metadata.basic_auth = {
      username: element("username").value,
      password: element("password").value,
    };
  }

  const payload = new TextEncoder().encode(JSON.stringify(metadata));

  if (payload.length > 65535) {
    throw new Error("Connection metadata is too large.");
  }

  return payload;
}

function gatherICE(call) {
  return new Promise((resolve, reject) => {
    const pc = call.pc;
    const done = (error) => {
      clearTimeout(timer);
      pc.removeEventListener("icegatheringstatechange", changed);
      call.abort.signal.removeEventListener("abort", canceled);

      if (error) {
        reject(error);
      } else {
        resolve();
      }
    };

    const changed = () => {
      if (pc.iceGatheringState === "complete") {
        done();
      }
    };

    const canceled = () => done(new Error("Call canceled."));
    const timer = setTimeout(
      () => done(new Error("ICE gathering timed out. Check the browser ICE servers.")),
      15000,
    );
    pc.addEventListener("icegatheringstatechange", changed);
    call.abort.signal.addEventListener("abort", canceled, { once: true });

    if (call.abort.signal.aborted) {
      canceled();
    } else {
      changed();
    }
  });
}

async function startWebRTC(call) {
  const iceServers = JSON.parse(element("ice").value);

  if (!Array.isArray(iceServers)) {
    throw new Error("Browser ICE servers must be a JSON array.");
  }

  const pc = (call.pc = new RTCPeerConnection({ iceServers }));
  pc.ontrack = (event) => {
    if (active !== call) {
      return;
    }

    element("remote").srcObject = new MediaStream([event.track]);
    element("remote")
      .play()
      .catch(() => status("Audio playback was blocked. Use the audio controls to play."));
    element("remote").controls = true;
  };

  pc.onconnectionstatechange = () => {
    if (active !== call) {
      return;
    }

    if (pc.connectionState === "connected") {
      clearTimeout(call.timer);
      clearTimeout(call.disconnectTimer);
      controls(true, true);
      status("Audio connected. The remote call may still be ringing.");
    } else if (pc.connectionState === "failed" || pc.connectionState === "closed") {
      finish(call, "Call ended.");
    } else if (pc.connectionState === "disconnected") {
      clearTimeout(call.disconnectTimer);
      call.disconnectTimer = setTimeout(() => finish(call, "Connection lost."), 10000);
    }
  };

  call.sender = pc.addTrack(call.stream.getAudioTracks()[0], call.stream);
  const transceiver = pc.getTransceivers()[0];
  const codecs = RTCRtpReceiver.getCapabilities("audio").codecs.filter((codec) =>
    /audio\/(PCMU|PCMA|opus|telephone-event)$/i.test(codec.mimeType),
  );

  if (!codecs.some((codec) => /audio\/(PCMU|PCMA|opus)$/i.test(codec.mimeType))) {
    throw new Error("This browser does not support PCMU, PCMA, or Opus.");
  }

  transceiver.setCodecPreferences(codecs);
  await pc.setLocalDescription(await pc.createOffer());
  await gatherICE(call);

  if (active !== call) {
    return;
  }

  status("Dialing…");
  const response = await fetch(callURL(), {
    method: "POST",
    headers: { "Content-Type": "application/sdp", Accept: "application/sdp" },
    credentials: "same-origin",
    body: pc.localDescription.sdp,
    signal: call.abort.signal,
  });

  if (response.status !== 201) {
    throw new Error(`Call failed (HTTP ${response.status}).`);
  }

  const locationHeader = response.headers.get("Location");

  if (!locationHeader) {
    throw new Error("The server did not return a session URL.");
  }

  const session = new URL(locationHeader, response.url);

  if (session.origin !== location.origin) {
    throw new Error("The session URL must use the page's origin.");
  }

  if (active !== call) {
    void deleteSession(session);

    return;
  }

  call.session = session;
  await pc.setRemoteDescription({ type: "answer", sdp: await response.text() });
}

async function startAudioSocket(call) {
  const initialIdentity = identity();
  const context = (call.context = new AudioContext());
  await context.resume();
  await context.audioWorklet.addModule(assetURL("audio-worklet.js"));

  if (active !== call) {
    return;
  }

  const worklet = (call.worklet = new AudioWorkletNode(context, "sippycup-audio", {
    numberOfInputs: 1,
    numberOfOutputs: 1,
    outputChannelCount: [1],
  }));
  call.microphone = context.createMediaStreamSource(call.stream);
  call.microphone.connect(worklet);
  worklet.connect(context.destination);
  const url = callURL();
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  const ws = (call.ws = new WebSocket(url, "audiosocket"));
  ws.binaryType = "arraybuffer";
  worklet.port.onmessage = (event) => {
    if (active === call && ws.readyState === WebSocket.OPEN && ws.bufferedAmount < 65536) {
      ws.send(packet(0x10, new Uint8Array(event.data)));
    }
  };

  ws.onopen = () => {
    if (active !== call) {
      ws.close();

      return;
    }

    ws.send(packet(1, initialIdentity));
    controls(true, true);
    status(
      settings.native === "true" ? "Dialing…" : "Audio stream open. Waiting for the remote call…",
    );
  };

  ws.onclose = () => finish(call, "Call ended.");
  ws.onerror = () =>
    finish(call, "WebSocket connection failed. Check authentication and server logs.");
  ws.onmessage = (event) => {
    if (active !== call) {
      return;
    }

    if (!(event.data instanceof ArrayBuffer) || event.data.byteLength < 3) {
      finish(call, "Invalid AudioSocket packet.");

      return;
    }

    const view = new DataView(event.data);
    const kind = view.getUint8(0),
      length = view.getUint16(1);

    if (length + 3 !== view.byteLength) {
      finish(call, "Invalid AudioSocket packet length.");

      return;
    }

    if (kind === 0 || kind === 255) {
      finish(call, kind === 0 ? "Call ended." : "The remote endpoint rejected the call.");

      return;
    }

    if (kind === 2 && length === 1 && settings.native === "true") {
      const state = view.getUint8(3);

      if (state === 0) {
        status("Early audio. The remote call has not answered yet.");
      } else if (state === 1) {
        clearTimeout(call.timer);
        status("Call answered.");
      } else {
        finish(call, "Invalid call state.");
      }

      return;
    }

    if (kind === 3 && length === 1) {
      status(`Received DTMF: ${String.fromCharCode(view.getUint8(3))}`);

      return;
    }

    const rates = [8000, 12000, 16000, 24000, 32000, 44100, 48000, 96000, 192000];
    const rate = rates[kind - 0x10];

    if (rate && length % 2 === 0) {
      clearTimeout(call.timer);

      if (settings.native !== "true") {
        status("Audio connected.");
      }

      const samples = new Float32Array(length / 2);

      for (let i = 0; i < samples.length; i++) {
        samples[i] = view.getInt16(3 + i * 2, true) / 32768;
      }

      worklet.port.postMessage({ rate, samples }, [samples.buffer]);
    }
  };
}

async function start() {
  if (active) {
    return;
  }

  const call = { abort: new AbortController() };
  active = call;
  controls(true);
  element("digits").textContent = "";
  status("Requesting microphone access…");

  try {
    if (!navigator.mediaDevices || !isSecureContext) {
      throw new Error("Use HTTPS or localhost to access the microphone.");
    }

    const stream = await navigator.mediaDevices.getUserMedia({
      audio: {
        channelCount: 1,
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    });

    if (active !== call) {
      for (const track of stream.getTracks()) {
        track.stop();
      }

      return;
    }

    call.stream = stream;
    call.timer = setTimeout(() => finish(call, "Call setup timed out."), 90000);

    if (settings.protocol === "webrtc_http") {
      await startWebRTC(call);
    } else {
      await startAudioSocket(call);
    }
  } catch (error) {
    finish(call, error.message || "Unable to start the call.");
  }
}

function sendDigit(digit) {
  const call = active;

  if (!call) {
    return;
  }

  try {
    if (call.ws && call.ws.readyState === WebSocket.OPEN) {
      call.ws.send(packet(3, new Uint8Array([digit.charCodeAt(0)])));
    } else if (call.sender && call.sender.dtmf && call.sender.dtmf.canInsertDTMF) {
      call.sender.dtmf.insertDTMF(call.sender.dtmf.toneBuffer + digit, 120, 70);
    } else {
      status("DTMF is not available for this session.");

      return;
    }

    element("digits").textContent = (element("digits").textContent + digit).slice(-64);
  } catch (error) {
    status(error.message);
  }
}

element("destination").value = new URL(location.href).searchParams.get("destination") || "";
element("call").onclick = start;
element("hangup").onclick = () => {
  if (active) {
    finish(active, "Call ended.");
  }
};

element("mute").onclick = () => {
  if (!active || !active.stream) {
    return;
  }

  const tracks = active.stream.getAudioTracks();
  const muted = tracks[0].enabled;

  for (const track of tracks) {
    track.enabled = !muted;
  }

  element("mute").textContent = muted ? "Unmute" : "Mute";
  element("mute").setAttribute("aria-pressed", String(muted));
};

for (const button of keypad) {
  button.onclick = () => sendDigit(button.dataset.digit);
}

window.addEventListener("pagehide", () => {
  if (active) {
    finish(active, "Call ended.");
  }
});
