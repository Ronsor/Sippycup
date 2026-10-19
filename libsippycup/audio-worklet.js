"use strict";

// Capture 20 ms frames of 8 kHz PCM and play the remote mono or stereo stream. All queues
// are bounded so a slow connection cannot accumulate unbounded audio latency.
class SippycupAudio extends AudioWorkletProcessor {
  constructor(options) {
    super();
    this.channels = options.processorOptions?.channels === 2 ? 2 : 1;
    this.capture = new Int16Array(160 * this.channels);
    this.captureSize = 0;
    this.capturePhase = 0;
    this.captureSum = [0, 0];
    this.captureCount = 0;
    this.queue = [];
    this.queued = 0;
    this.offset = 0;
    this.port.onmessage = (event) => {
      const { samples, rate, channels = 1 } = event.data;

      if (!(samples instanceof Float32Array) || !rate ||
          (channels !== 1 && channels !== 2) || samples.length % channels) {
        return;
      }

      const size = Math.round((samples.length * sampleRate) / (rate * channels));

      if (size > sampleRate * 2) {
        return;
      }

      const output = [new Float32Array(size), new Float32Array(size)];

      for (let i = 0; i < size; i++) {
        const position = (i * rate) / sampleRate;
        const index = Math.floor(position),
          fraction = position - index;
        for (let ch = 0; ch < 2; ch++) {
          const sourceChannel = channels === 1 ? 0 : ch;
          const next = Math.min(index + 1, samples.length / channels - 1);
          output[ch][i] =
            samples[index * channels + sourceChannel] * (1 - fraction) +
            samples[next * channels + sourceChannel] * fraction;
        }
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
    const input = inputs[0];
    const output = outputs[0];

    for (let i = 0; i < output[0].length; i++) {
      if (input.length) {
        for (let ch = 0; ch < this.channels; ch++) {
          this.captureSum[ch] += (input[ch] || input[0])[i];
        }
        this.captureCount++;
        this.capturePhase += 8000;

        if (this.capturePhase >= sampleRate) {
          this.capturePhase -= sampleRate;
          for (let ch = 0; ch < this.channels; ch++) {
            const value = Math.max(-1, Math.min(1, this.captureSum[ch] / this.captureCount));
            this.capture[this.captureSize++] = Math.round(value * (value < 0 ? 32768 : 32767));
            this.captureSum[ch] = 0;
          }
          this.captureCount = 0;

          if (this.captureSize === this.capture.length) {
            const buffer = new ArrayBuffer(this.capture.length * 2);
            const view = new DataView(buffer);

            for (let j = 0; j < this.capture.length; j++) {
              view.setInt16(j * 2, this.capture[j], true);
            }

            this.port.postMessage(buffer, [buffer]);
            this.captureSize = 0;
          }
        }
      }

      if (this.queue.length) {
        for (let ch = 0; ch < output.length; ch++) {
          output[ch][i] = this.queue[0][Math.min(ch, 1)][this.offset];
        }
        this.offset++;
        this.queued--;

        if (this.offset === this.queue[0][0].length) {
          this.queue.shift();
          this.offset = 0;
        }
      } else {
        for (const channel of output) {
          channel[i] = 0;
        }
      }
    }

    return true;
  }
}

registerProcessor("sippycup-audio", SippycupAudio);
