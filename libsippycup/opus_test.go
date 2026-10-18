package libsippycup

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, protocol := range []string{"sip", "webrtc_http"} {
		t.Run(protocol, func(t *testing.T) {
			address := freeAddress(t)
			extra := ""
			if protocol == "webrtc_http" {
				extra = "path=\"/calls\"\n ice { bind=\"127.0.0.1\" }"
			}
			e := testEngine(t, fmt.Sprintf("source \"in\" {\n type=%q\n bind=%q\n codecs=[\"opus\"]\n%s\n}\n",
				protocol, address, extra)+echoEndpoint)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			n := &Node{Name: "client", Type: protocol, Codecs: []string{"opus"}}
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
			if leg.Format() != opusFormat {
				t.Fatalf("negotiated %+v", leg.Format())
			}
			codec := mediaCodec{}
			for i := 0; i < 10; i++ {
				packet, err := codec.encode(opusTone(i), opusFormat)
				if err != nil {
					t.Fatal(err)
				}
				if err := leg.Write(ctx, Frame{Format: opusFormat, Data: packet}); err != nil {
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

// The external reference decoder is never required by the default test suite.
func TestOpusFFmpegDecode(t *testing.T) {
	binaryPath := os.Getenv("SIPPYCUP_FFMPEG")
	if binaryPath == "" || testing.Short() {
		t.Skip("set SIPPYCUP_FFMPEG to run optional libopus interoperability")
	}
	var ogg bytes.Buffer
	writer, err := oggwriter.NewWith(&ogg, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	codec := mediaCodec{}
	for i := 0; i < 50; i++ {
		packet, err := codec.encode(opusTone(i), opusFormat)
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
		"-f", "s16le", "-ac", "1", "-ar", "48000", "pipe:1")
	cmd.Stdin = bytes.NewReader(ogg.Bytes())
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pcm := decode(Frame{Format: Format{"pcm_s16le", 48000, 1}, Data: data})
	if len(pcm) < 40000 || signalEnergy(pcm) < 1e6 || toneFraction(pcm) < 0.5 {
		t.Fatalf("libopus decoded missing or silent audio: %d samples", len(pcm))
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
