"use strict";

// Capture 20 ms frames of 8 kHz PCM and play the remote mono stream. All queues
// are bounded so a slow connection cannot accumulate unbounded audio latency.
class SippycupAudio extends AudioWorkletProcessor {
  constructor() {
    super();
    this.capture = new Int16Array(160);
    this.captureSize = 0;
    this.capturePhase = 0;
    this.captureSum = 0;
    this.captureCount = 0;
    this.queue = [];
    this.queued = 0;
    this.offset = 0;
    this.port.onmessage = (event) => {
      const { samples, rate } = event.data;

      if (!(samples instanceof Float32Array) || !rate) {
        return;
      }

      const size = Math.round((samples.length * sampleRate) / rate);

      if (size > sampleRate * 2) {
        return;
      }

      const output = new Float32Array(size);

      for (let i = 0; i < size; i++) {
        const position = (i * rate) / sampleRate;
        const index = Math.floor(position),
          fraction = position - index;
        output[i] =
          samples[index] * (1 - fraction) +
          samples[Math.min(index + 1, samples.length - 1)] * fraction;
      }

      if (this.queued + size > sampleRate * 2) {
        this.queue = [];
        this.queued = 0;
        this.offset = 0;
      }

      if (size) {
        this.queue.push(output);
        this.queued += size;
      }
    };
  }

  process(inputs, outputs) {
    const input = inputs[0][0];
    const output = outputs[0][0];

    for (let i = 0; i < output.length; i++) {
      if (input) {
        this.captureSum += input[i];
        this.captureCount++;
        this.capturePhase += 8000;

        if (this.capturePhase >= sampleRate) {
          this.capturePhase -= sampleRate;
          const value = Math.max(-1, Math.min(1, this.captureSum / this.captureCount));
          this.capture[this.captureSize++] = Math.round(value * (value < 0 ? 32768 : 32767));
          this.captureSum = 0;
          this.captureCount = 0;

          if (this.captureSize === 160) {
            const buffer = new ArrayBuffer(320);
            const view = new DataView(buffer);

            for (let j = 0; j < 160; j++) {
              view.setInt16(j * 2, this.capture[j], true);
            }

            this.port.postMessage(buffer, [buffer]);
            this.captureSize = 0;
          }
        }
      }

      if (this.queue.length) {
        output[i] = this.queue[0][this.offset++];
        this.queued--;

        if (this.offset === this.queue[0].length) {
          this.queue.shift();
          this.offset = 0;
        }
      } else {
        output[i] = 0;
      }
    }

    return true;
  }
}

registerProcessor("sippycup-audio", SippycupAudio);
