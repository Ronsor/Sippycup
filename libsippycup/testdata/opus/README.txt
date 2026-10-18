Generated from a 0.5-second, 440 Hz sine wave (amplitude 10000, signed 16-bit PCM, mono 48 kHz) using FFmpeg/libopus.
Files contain little-endian uint16 packet lengths followed by raw Opus packets. No Ogg headers or tags.
Command: ffmpeg -f s16le -ar 48000 -ac 1 -i input.pcm -c:a libopus -application voip -b:a BITRATE -frame_duration MS -f ogg output.ogg
speech.bin: 12000 bits/s, 60 ms; hybrid.bin: 24000 bits/s, 20 ms; celt.bin: 64000 bits/s, 10 ms.
These generated tone fixtures contain no third-party audio.
stereo.bin: 64000 bits/s, 20 ms, libopus default audio application, two 440 Hz channels with amplitudes 10000 and 5000.
