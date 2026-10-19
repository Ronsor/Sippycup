package libsippycup

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pion/opus"
)

// OpusConfig controls locally encoded Opus. Bitrate defaults to 32000 bits/s.
// Channels defaults to one; two enables stereo. Encoding uses 48 kHz PCM
// and 20 ms packets. It does not enable FEC or DTX.
type OpusConfig struct {
	Bitrate  int `hcl:"bitrate,optional"`
	Channels int `hcl:"channels,optional"`
}

// mediaCodec owns codec history for one bridge direction. Encoders and decoders
// must never be shared between calls or used concurrently.
type mediaCodec struct {
	decoder         *opus.Decoder
	encoder         *opus.Encoder
	bitrate         int
	encoderChannels int
}

func (c *mediaCodec) decode(f Frame) ([]int16, error) {
	if f.Format.Codec != "opus" {
		return decode(f), nil
	}

	if _, err := opusSamples(f.Data); err != nil {
		return nil, err
	}

	if c.decoder == nil {
		d, err := opus.NewDecoderWithOutput(48000, 2)
		if err != nil {
			return nil, err
		}
		c.decoder = &d
	}

	pcm := make([]int16, 5760*2) // Opus permits up to 120 ms per packet.
	n, err := c.decoder.DecodeToInt16(f.Data, pcm)
	if err != nil {
		return nil, err
	}

	return mixChannels(pcm[:n*2], 2, f.Format.Channels), nil
}

func (c *mediaCodec) encode(pcm []int16, f Format) ([]byte, error) {
	if f.Codec != "opus" {
		return encode(pcm, f), nil
	}

	if c.encoder == nil || c.encoderChannels != f.Channels {
		bitrate := c.bitrate
		if bitrate == 0 {
			bitrate = 32000
		}

		encoder, err := opus.NewEncoder(opus.WithChannels(f.Channels), opus.WithBitrate(bitrate))
		if err != nil {
			return nil, err
		}
		c.encoder = encoder
		c.encoderChannels = f.Channels
	}

	packet := make([]byte, 1276)
	n, err := c.encoder.Encode(encode(pcm, Format{"pcm_s16le", 48000, f.Channels}), packet)
	if err != nil {
		return nil, err
	}

	return packet[:n], nil
}

// opusSamples reads packet duration from the RFC 6716 TOC, in 48 kHz samples.
// Full compressed-frame validation belongs to the decoder.
func opusSamples(packet []byte) (int, error) {
	if len(packet) == 0 || len(packet) > 65535 {
		return 0, errors.New("invalid Opus packet length")
	}

	config := packet[0] >> 3
	var samples int
	switch {
	case config >= 16:
		samples = 120 << (config & 3)
	case config >= 12:
		samples = 480 << (config & 1)
	default:
		samples = 480 << (config & 3)
		if config&3 == 3 {
			samples = 2880
		}
	}

	count := 1
	switch packet[0] & 3 {
	case 1, 2:
		count = 2
	case 3:
		if len(packet) < 2 {
			return 0, errors.New("truncated Opus packet")
		}
		count = int(packet[1] & 63)
	}

	if count == 0 || count*samples > 5760 {
		return 0, errors.New("invalid Opus packet duration")
	}

	return samples * count, nil
}

func frameSamples(f Frame) (int, error) {
	if f.Format.Codec == "opus" {
		return opusSamples(f.Data)
	}

	return len(f.Data) / (f.Format.bytesPerSample() * f.Format.Channels), nil
}

// opusChannels is the preferred channel count for locally encoded audio.
func opusChannels(n *Node) int {
	if n.Transcode != nil && n.Transcode.Codec == "opus" && n.Transcode.Channels != 0 {
		return n.Transcode.Channels
	}
	if n.Opus != nil && n.Opus.Channels != 0 {
		return n.Opus.Channels
	}

	return 1
}

func opusFMTP(channels int) string {
	stereo := channels - 1
	return fmt.Sprintf("stereo=%d;sprop-stereo=%d;useinbandfec=0", stereo, stereo)
}

// remoteOpusStereo reads the receiver's preference, not sprop-stereo (sender).
func remoteOpusStereo(body string, pt uint8) bool {
	prefix := fmt.Sprintf("a=fmtp:%d ", pt)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		for _, field := range strings.Split(strings.TrimPrefix(line, prefix), ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
			if ok && strings.TrimSpace(key) == "stereo" {
				return strings.TrimSpace(value) == "1"
			}
		}
	}

	return false
}

// packetFormat reports the actual Opus channel count independently of SDP /2.
func packetFormat(f Format, data []byte) Format {
	if f.Codec == "opus" && len(data) > 0 {
		f.Channels = 1
		if data[0]&4 != 0 {
			f.Channels = 2
		}
	}

	return f
}
