package libsippycup

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ronsor/sippycup/audiosocket"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

var opusFormat = Format{"opus", 48000, 1}

func opusTone(frame int) []int16 {
	pcm := make([]int16, 960)
	for i := range pcm {
		pcm[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(frame*960+i)/48000))
	}

	return pcm
}

func signalEnergy(pcm []int16) float64 {
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}

	return sum / float64(max(1, len(pcm)))
}

func TestOpusReferenceDecode(t *testing.T) {
	files, err := filepath.Glob("testdata/opus/*.bin")
	if err != nil || len(files) < 3 {
		t.Fatalf("missing reference fixtures: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			reader := bytes.NewReader(data)
			codec := mediaCodec{}
			var decoded []int16
			for reader.Len() > 0 {
				var size uint16
				if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
					t.Fatal(err)
				}
				packet := make([]byte, size)
				if _, err := io.ReadFull(reader, packet); err != nil {
					t.Fatal(err)
				}
				samples, err := opusSamples(packet)
				if err != nil {
					t.Fatal(err)
				}
				pcm, err := codec.decode(Frame{Format: opusFormat, Data: packet})
				if err != nil || len(pcm) != samples {
					t.Fatalf("decode samples=%d expected=%d: %v", len(pcm), samples, err)
				}
				decoded = append(decoded, pcm...)
			}
			if signalEnergy(decoded) < 1e6 || toneFraction(decoded) < 0.5 {
				t.Fatal("reference audio decoded to silence")
			}
		})
	}
}

func TestOpusPacketBoundariesAndTimestamps(t *testing.T) {
	ctx := context.Background()
	src := newChannelLeg(ctx, opusFormat, 4)
	defer src.Close()
	dst := newChannelLeg(ctx, opusFormat, 4)
	defer dst.Close()
	codec := mediaCodec{}
	var packets [][]byte
	for i := 0; i < 3; i++ {
		packet, err := codec.encode(opusTone(i), opusFormat)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, packet)
		src.emit(Frame{Format: opusFormat, Data: packet})
	}
	src.fail(io.EOF)
	var sent []*rtp.Packet
	out := newRTPOutput(111, 101, func(p *rtp.Packet) error {
		sent = append(sent, p)
		return nil
	})
	dst.writeFn = out.send
	node := &Node{Type: "sip", Transcode: &TranscodeConfig{Mode: "never"}}
	if err := pump(ctx, src, dst, node, node, 0); err != io.EOF {
		t.Fatal(err)
	}
	if len(sent) != len(packets) {
		t.Fatalf("packet count: %d", len(sent))
	}
	for i, packet := range packets {
		if !bytes.Equal(packet, sent[i].Payload) {
			t.Fatal("Opus packet was split or modified")
		}
		if i > 0 && sent[i].Timestamp-sent[i-1].Timestamp != 960 {
			t.Fatal("RTP timestamp used packet length instead of sample count")
		}
	}
	before := out.timestamp
	if err := out.send(ctx, Frame{Digit: '5'}); err != nil {
		t.Fatal(err)
	}
	if out.timestamp-before != 4800 {
		t.Fatal("DTMF did not advance the 48 kHz audio clock correctly")
	}
}

func TestOpusSIPAndWebRTC(t *testing.T) {
	for _, channels := range []int{1, 2} {
		for _, protocol := range []string{"sip", "webrtc_http"} {
			t.Run(fmt.Sprintf("%s/%d", protocol, channels), func(t *testing.T) {
				address := freeAddress(t)
				format := Format{"opus", 48000, channels}
				extra := fmt.Sprintf("opus { channels=%d }\n", channels)
				if protocol == "webrtc_http" {
					extra += "path=\"/calls\"\n ice { bind=\"127.0.0.1\" }"
				}
				e := testEngine(t, fmt.Sprintf("source \"in\" {\n type=%q\n bind=%q\n codecs=[\"opus\"]\n%s\n}\n",
					protocol, address, extra)+echoEndpoint)
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				n := &Node{Name: "client", Type: protocol, Codecs: []string{"opus"}, Opus: &OpusConfig{Channels: channels}}
				var leg Leg
				var err error
				if protocol == "sip" {
					n.Bind, n.URI = "127.0.0.1:0", "sip:"+address
					leg, err = e.sipEndpoint(ctx, ctx, n, "echo", nil)
				} else {
					n.URL, n.ICE = "http://"+address+"/calls", &ICEConfig{Bind: "127.0.0.1"}
					leg, err = e.webEndpoint(ctx, ctx, n, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer leg.Close()
				if leg.Format() != format {
					t.Fatalf("negotiated %+v", leg.Format())
				}
				codec := mediaCodec{}
				for i := 0; i < 10; i++ {
					packet, err := codec.encode(mixChannels(opusTone(i), 1, channels), format)
					if err != nil {
						t.Fatal(err)
					}
					if err := leg.Write(ctx, Frame{Format: format, Data: packet}); err != nil {
						t.Fatal(err)
					}
				}
				for {
					frame, err := leg.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					pcm, err := codec.decode(frame)
					if err != nil {
						t.Fatal(err)
					}
					if signalEnergy(pcm) > 1e6 {
						break
					}
				}
			})
		}
	}
}

// The external reference decoder is never required by the default test suite.
func TestOpusFFmpegDecode(t *testing.T) {
	binaryPath := os.Getenv("SIPPYCUP_FFMPEG")
	if binaryPath == "" || testing.Short() {
		t.Skip("set SIPPYCUP_FFMPEG to run optional libopus interoperability")
	}

	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprint(channels), func(t *testing.T) {
			format := Format{"opus", 48000, channels}
			var ogg bytes.Buffer
			writer, err := oggwriter.NewWith(&ogg, 48000, uint16(channels))
			if err != nil {
				t.Fatal(err)
			}
			codec := mediaCodec{}
			for i := 0; i < 50; i++ {
				packet, err := codec.encode(mixChannels(opusTone(i), 1, channels), format)
				if err != nil {
					t.Fatal(err)
				}
				err = writer.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i * 960)},
					Payload: packet,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binaryPath, "-v", "error", "-f", "ogg", "-i", "pipe:0",
				"-f", "s16le", "-ac", fmt.Sprint(channels), "-ar", "48000", "pipe:1")
			cmd.Stdin = bytes.NewReader(ogg.Bytes())
			data, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			pcm := decode(Frame{Format: Format{"pcm_s16le", 48000, channels}, Data: data})
			pcm = mixChannels(pcm, channels, 1)
			if len(pcm) < 40000 || signalEnergy(pcm) < 1e6 || toneFraction(pcm) < 0.5 {
				t.Fatalf("FFmpeg decoded missing or silent audio: %d samples", len(pcm))
			}
		})
	}
}

func TestOpusMalformedDuration(t *testing.T) {
	for _, packet := range [][]byte{nil, {3}, {3, 0}, {3, 63}} {
		if _, err := opusSamples(packet); err == nil {
			t.Fatalf("accepted malformed packet %x", packet)
		}
	}
}

// toneFraction measures signal power at the expected frequency independently
// of codec delay and phase. Noise with similar RMS must not pass audio checks.
func toneFraction(pcm []int16) float64 {
	var re, im, energy float64
	for i, sample := range pcm {
		angle := 2 * math.Pi * 440 * float64(i) / 48000
		value := float64(sample)
		re += value * math.Cos(angle)
		im += value * math.Sin(angle)
		energy += value * value
	}
	if energy == 0 {
		return 0
	}

	return 2 * (re*re + im*im) / (float64(len(pcm)) * energy)
}

func TestStereoOpusChannelSeparation(t *testing.T) {
	format := Format{"opus", 48000, 2}
	codec := mediaCodec{bitrate: 96000}
	var left, right []int16
	for frame := 0; frame < 30; frame++ {
		pcm := make([]int16, 1920)
		for i := 0; i < 960; i++ {
			x := float64(frame*960+i) / 48000
			pcm[2*i] = int16(10000 * math.Sin(2*math.Pi*440*x))
			pcm[2*i+1] = int16(10000 * math.Sin(2*math.Pi*880*x))
		}
		packet, err := codec.encode(pcm, format)
		if err != nil {
			t.Fatal(err)
		}
		if packet[0]&4 == 0 {
			t.Fatal("encoder did not emit stereo")
		}
		decoded, err := codec.decode(Frame{Format: format, Data: packet})
		if err != nil || len(decoded) != 1920 {
			t.Fatalf("decode length=%d: %v", len(decoded), err)
		}
		for i := 0; i < 960; i++ {
			left = append(left, decoded[2*i])
			right = append(right, decoded[2*i+1])
		}
	}
	if toneFraction(left) < .5 || toneFraction(right) > .1 || signalEnergy(right) < 1e6 {
		t.Fatal("stereo channels were mixed or lost")
	}
}

func TestChannelConversion(t *testing.T) {
	mono := []int16{-32768, 32767, 100, -100}
	stereo := mixChannels(mono, 1, 2)
	for i, v := range mono {
		if stereo[2*i] != v || stereo[2*i+1] != v {
			t.Fatal("mono duplication")
		}
	}
	got := mixChannels([]int16{32767, 32767, -32768, -32768, 100, -100}, 2, 1)
	if got[0] != 32767 || got[1] != -32768 || got[2] != 0 {
		t.Fatalf("downmix: %v", got)
	}
	var rs channelResampler
	for frame := 0; frame < 10; frame++ {
		pcm := make([]int16, 1920)
		for i := 0; i < 960; i++ {
			pcm[2*i] = 10000
		}
		out := rs.process(pcm, 48000, 8000, 2)
		for i := 0; i < len(out)/2; i++ {
			if out[2*i+1] != 0 {
				t.Fatal("resampler leaked between channels")
			}
		}
		if frame > 0 && len(out) != 320 {
			t.Fatalf("stereo sample count: %d", len(out))
		}
	}
}

func TestOpusStereoPreference(t *testing.T) {
	for _, test := range []struct {
		fmtp   string
		stereo bool
	}{
		{"stereo=1;sprop-stereo=0", true}, {"stereo=0;sprop-stereo=1", false}, {"sprop-stereo=1", false},
	} {
		if got := remoteOpusStereo("a=fmtp:111 "+test.fmtp+"\r\n", 111); got != test.stereo {
			t.Fatalf("%s: %v", test.fmtp, got)
		}
	}
}

func TestStereoAudioSocketGateway(t *testing.T) {
	for _, mode := range []string{"pcm", "raw", "audiosocket"} {
		for _, protocol := range []string{"audiosocket", "audiosocket_ws"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				address := freeAddress(t)
				path, scheme := "", "tcp"
				if protocol == "audiosocket_ws" {
					path = "path=\"/audio\""
					scheme = "ws"
				}
				source := fmt.Sprintf(`source "in" {
 type = %q
 bind = %q
 extended = true
 %s
 transcode {
  codec = "pcm_s16le"
  sample_rate = 48000
  channels = 2
 }
}
`, protocol, address, path)
				echo := strings.Replace(echoEndpoint, "sample_rate = 8000", "sample_rate = 48000\n  channels = 2", 1)
				echo = strings.Replace(echo, "@test-echo", "@test-stereo-echo", 1)
				echo = strings.Replace(echo, `io = "pcm"`, fmt.Sprintf("io = %q", mode), 1)
				if mode == "audiosocket" {
					echo = strings.Replace(echo, `type = "exec"`, "type = \"exec\"\n extended = true", 1)
				}
				testEngine(t, source+echo)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c, err := audiosocket.Dial(ctx, scheme+"://"+address+"/audio", audiosocket.WithExtended(true))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if err := c.WriteIdentity(audiosocket.Identity{}); err != nil {
					t.Fatal(err)
				}
				pcm := make([]int16, 1920)
				for i := 0; i < 960; i++ {
					pcm[2*i] = int16(i)
					pcm[2*i+1] = -int16(i)
				}
				data := encode(pcm, Format{"pcm_s16le", 48000, 2})
				if err := c.WritePCM(48000, 2, data); err != nil {
					t.Fatal(err)
				}
				p, err := c.ReadPacket()
				if err != nil || p.Kind != 0x26 || !bytes.Equal(p.Payload, data) {
					t.Fatalf("stereo echo kind=%x length=%d: %v", p.Kind, len(p.Payload), err)
				}
			})
		}
	}
}

func TestStereoMemoryLegOpusPacket(t *testing.T) {
	ctx := context.Background()
	format := Format{"opus", 48000, 2}
	a, b, err := NewLegPair(ctx, format, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// A valid mono silence packet may be odd-sized, even in a stereo stream.
	packet := []byte{0xf8, 0xff, 0xfe}
	if err := a.Write(ctx, Frame{Format: format, Data: packet}); err != nil {
		t.Fatal(err)
	}
	got, err := b.Read(ctx)
	if err != nil || !bytes.Equal(got.Data, packet) {
		t.Fatalf("packet lost: %v", err)
	}
}

func TestStereoConfigValidation(t *testing.T) {
	for _, typ := range []string{"audiosocket", "audiosocket_ws", "exec"} {
		n := &Node{
			Name:      "test",
			Type:      typ,
			Transcode: &TranscodeConfig{Codec: "pcm_s16le", Channels: 2},
		}
		switch typ {
		case "exec":
			n.IO = "audiosocket"
		case "audiosocket":
			n.Address = "127.0.0.1:9019"
		case "audiosocket_ws":
			n.URL = "ws://127.0.0.1:9019/audio"
		}
		if err := validateNodeShape(n, false); err == nil || !strings.Contains(err.Error(), "stereo AudioSocket") {
			t.Fatalf("%s: expected extended-mode error, got %v", typ, err)
		}
		n.Extended = true
		if err := validateNodeShape(n, false); err != nil {
			t.Fatalf("%s rejected extended stereo: %v", typ, err)
		}
	}
	for _, codec := range []string{"pcmu", "pcma"} {
		if _, err := formatFor(codec, 8000, 2); err == nil {
			t.Fatalf("%s accepted stereo", codec)
		}
	}
}

func TestStereoPumpMixing(t *testing.T) {
	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprint(channels), func(t *testing.T) {
			ctx := context.Background()
			input := Format{"pcm_s16le", 8000, channels}
			output := Format{"pcm_s16le", 8000, 3 - channels}
			src := newChannelLeg(ctx, input, 1)
			dst := newChannelLeg(ctx, output, 1)
			defer src.Close()
			defer dst.Close()
			pcm := make([]int16, 160*channels)
			for i := 0; i < 160; i++ {
				pcm[i*channels] = 12000
				if channels == 2 {
					pcm[i*channels+1] = -4000
				}
			}
			src.emit(Frame{Format: input, Data: encode(pcm, input)})
			src.fail(io.EOF)
			var received []int16
			dst.writeFn = func(_ context.Context, f Frame) error { received = append(received, decode(f)...); return nil }
			n := &Node{Type: "inproc"}
			if err := pump(ctx, src, dst, n, n, 0); err != io.EOF {
				t.Fatal(err)
			}
			if len(received) != 160*output.Channels {
				t.Fatalf("output length %d", len(received))
			}
			want := int16(12000)
			if channels == 2 {
				want = 4000
			}
			for _, sample := range received {
				if sample != want {
					t.Fatalf("sample=%d want=%d", sample, want)
				}
			}
		})
	}
}

func TestStereoOpusMonoPacketPassthrough(t *testing.T) {
	ctx := context.Background()
	stereo := Format{"opus", 48000, 2}
	src := newChannelLeg(ctx, opusFormat, 1)
	dst := newChannelLeg(ctx, stereo, 1)
	defer src.Close()
	defer dst.Close()
	packet := []byte{0xf8, 0xff, 0xfe}
	src.emit(Frame{Format: opusFormat, Data: packet})
	src.fail(io.EOF)
	var got []byte
	dst.writeFn = func(_ context.Context, f Frame) error { got = f.Data; return nil }
	n := &Node{Type: "inproc", Transcode: &TranscodeConfig{Mode: "never"}}
	if err := pump(ctx, src, dst, n, n, 0); err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, packet) {
		t.Fatal("mono packet changed in stereo Opus stream")
	}
}
