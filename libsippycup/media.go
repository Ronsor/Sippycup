package libsippycup

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zaf/g711"
)

// Format describes an audio encoding, sample rate in Hz, and channel count.
// The gateway currently supports mono audio only.
type Format struct {
	Codec    string `json:"codec"`
	Rate     int    `json:"sample_rate"`
	Channels int    `json:"channels"`
}

func formatFor(codec string, rate, channels int) (Format, error) {
	if codec == "" {
		codec = "pcm_s16le"
	}

	if channels == 0 {
		channels = 1
	}

	if channels != 1 {
		return Format{}, errors.New("only mono audio is supported")
	}

	switch codec {
	case "pcmu", "pcma":
		if rate != 0 && rate != 8000 {
			return Format{}, errors.New("G.711 requires 8000 Hz")
		}
		rate = 8000
	case "pcm_s16le":
		if rate == 0 {
			rate = 8000
		}
		if !slices.Contains([]int{8000, 12000, 16000, 24000, 32000, 44100, 48000, 96000, 192000}, rate) {
			return Format{}, fmt.Errorf("unsupported PCM sample rate %d", rate)
		}
	default:
		return Format{}, fmt.Errorf("unsupported codec %q", codec)
	}

	return Format{codec, rate, channels}, nil
}

func (f Format) bytesPerSample() int {
	if f.Codec == "pcm_s16le" {
		return 2
	}

	return 1
}

func (f Format) frameBytes() int {
	return f.Rate / 50 * f.bytesPerSample()
}

func nodeFormat(n *Node, fallback Format) Format {
	if n.Transcode != nil && n.Transcode.Codec != "" {
		f, _ := formatFor(n.Transcode.Codec, n.Transcode.SampleRate, n.Transcode.Channels)

		return f
	}

	return fallback
}

// Frame carries encoded audio or one DTMF event. A nonzero Digit denotes
// an event and takes precedence over Data.
type Frame struct {
	Format Format
	Data   []byte
	Digit  byte
}

// Leg is one side of a call. Read and Write operate in opposite directions
// and may run concurrently; Close must interrupt blocked operations.
type Leg interface {
	Read(context.Context) (Frame, error)
	Write(context.Context, Frame) error
	Format() Format
	Close() error
}

type channelLeg struct {
	ctx       context.Context
	cancel    context.CancelFunc
	frames    chan Frame
	errors    chan error
	ended     chan struct{}
	endOnce   sync.Once
	format    Format
	node      *Node
	closeOnce sync.Once
	closeFn   func() error
	writeFn   func(context.Context, Frame) error
	startOnce sync.Once
	startFn   func()
}

func newChannelLeg(ctx context.Context, f Format, depth int) *channelLeg {
	ctx, cancel := context.WithCancel(ctx)
	if depth < 1 {
		depth = 10
	}

	return &channelLeg{
		ctx:    ctx,
		cancel: cancel,
		frames: make(chan Frame, depth),
		errors: make(chan error, 1),
		ended:  make(chan struct{}),
		format: f,
	}
}

func (l *channelLeg) emit(f Frame) bool {
	select {
	case l.frames <- f:
		return true
	case <-l.ctx.Done():
		return false
	}
}

func (l *channelLeg) fail(err error) {
	l.endOnce.Do(func() {
		close(l.ended)
	})
	if err == nil {
		err = io.EOF
	}

	select {
	case l.errors <- err:
	default:
	}
}

// Read returns a queued frame, terminal error, or context cancellation.
func (l *channelLeg) Read(ctx context.Context) (Frame, error) {
	l.start()
	select {
	case f := <-l.frames:
		return f, nil
	default:
	}

	select {
	case f := <-l.frames:
		return f, nil
	case e := <-l.errors:
		return Frame{}, e
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-l.ctx.Done():
		return Frame{}, io.EOF
	}
}

func (l *channelLeg) start() {
	l.startOnce.Do(func() {
		if l.startFn != nil && l.ctx.Err() == nil {
			l.startFn()
		}
	})
}

// Write sends a frame through the protocol-specific writer.
func (l *channelLeg) Write(ctx context.Context, f Frame) error {
	l.start()

	return l.writeFn(ctx, f)
}

// Format returns the preferred output format negotiated for this leg.
func (l *channelLeg) Format() Format {
	return l.format
}

// Close cancels I/O and runs protocol cleanup at most once.
func (l *channelLeg) Close() error {
	var e error
	l.closeOnce.Do(func() {
		l.endOnce.Do(func() {
			close(l.ended)
		})
		l.cancel()
		// Join any in-flight startup before protocol cleanup waits for workers.
		l.startOnce.Do(func() {})
		if l.closeFn != nil {
			e = l.closeFn()
		}
	})

	return e
}

func validateDTMF(n *Node) error {
	if n.DTMF == nil {
		return nil
	}

	d := n.DTMF
	allowed := []string{"native", "default", "none", "inband"}
	switch n.Type {
	case "sip":
		allowed = append(allowed, "sip_info", "rfc4733", "rfc2833")
	case "webrtc_http":
		allowed = append(allowed, "rfc4733", "rfc2833")
	case "exec":
		if n.Control == "json_fd" {
			allowed = append(allowed, "json")
		}
	}

	for _, s := range append(append([]string{}, d.Receive...), d.Send) {
		if s != "" && !slices.Contains(allowed, s) {
			return fmt.Errorf("unsupported DTMF mode %q for %s", s, n.Type)
		}

		if s == "inband" && n.Transcode != nil && n.Transcode.Mode == "never" {
			return errors.New("inband DTMF conflicts with transcode never")
		}
	}

	if slices.Contains(d.Receive, "none") && len(d.Receive) > 1 {
		return errors.New("DTMF none must be the only receive mode")
	}

	return nil
}

func nativeDTMF(n *Node) string {
	switch n.Type {
	case "sip", "webrtc_http":
		return "rfc4733"
	case "exec":
		if n.IO == "audiosocket" {
			return "audiosocket"
		}
		if n.Control == "json_fd" {
			return "json"
		}
		return "none"
	default:
		return "audiosocket"
	}
}

func normalizeDTMF(s string, n *Node) string {
	switch s {
	case "", "native", "default":
		return nativeDTMF(n)
	case "rfc2833":
		return "rfc4733"
	}

	return s
}

func receives(n *Node, mode string) bool {
	if n.DTMF == nil || len(n.DTMF.Receive) == 0 {
		return nativeDTMF(n) == mode
	}

	for _, s := range n.DTMF.Receive {
		if normalizeDTMF(s, n) == mode {
			return true
		}
	}

	return false
}

func sends(n *Node) string {
	if n.DTMF == nil {
		return nativeDTMF(n)
	}

	return normalizeDTMF(n.DTMF.Send, n)
}

func decode(f Frame) []int16 {
	out := make([]int16, len(f.Data)/f.Format.bytesPerSample())
	for i := range out {
		switch f.Format.Codec {
		case "pcmu":
			out[i] = g711.DecodeUlawFrame(f.Data[i])
		case "pcma":
			out[i] = g711.DecodeAlawFrame(f.Data[i])
		default:
			out[i] = int16(binary.LittleEndian.Uint16(f.Data[i*2:]))
		}
	}

	return out
}

func encode(pcm []int16, f Format) []byte {
	out := make([]byte, len(pcm)*f.bytesPerSample())
	for i, v := range pcm {
		switch f.Codec {
		case "pcmu":
			out[i] = g711.EncodeUlawFrame(v)
		case "pcma":
			out[i] = g711.EncodeAlawFrame(v)
		default:
			binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
		}
	}

	return out
}

// resampler is a streaming windowed-sinc resampler. Its history and fractional
// position survive packet boundaries, including non-integral rate ratios.
type resampler struct {
	in, out int
	samples []float64
	pos     float64
}

func (r *resampler) process(samples []int16, in, out int) []int16 {
	if in == out {
		return samples
	}

	const radius = 16
	if r.in != in || r.out != out {
		r.in = in
		r.out = out
		r.samples = make([]float64, radius)
		r.pos = radius
	}

	for _, v := range samples {
		r.samples = append(r.samples, float64(v))
	}

	result := make([]int16, 0, len(samples)*out/in+1)
	cut := math.Min(1, float64(out)/float64(in)) * .94
	for r.pos+radius < float64(len(r.samples)) {
		sum, weights := 0.0, 0.0
		center := int(r.pos)
		for j := center - radius + 1; j <= center+radius; j++ {
			x := float64(j) - r.pos
			if j < 0 {
				continue
			}

			sx := math.Pi * x * cut
			v := cut
			if math.Abs(sx) > 1e-9 {
				v = math.Sin(sx) / (math.Pi * x)
			}

			v *= .5 + .5*math.Cos(math.Pi*x/radius)
			sum += r.samples[j] * v
			weights += v
		}

		if weights != 0 {
			sum /= weights
		}

		sum = math.Max(-32768, math.Min(32767, sum))
		result = append(result, int16(math.Round(sum)))
		r.pos += float64(in) / float64(out)
	}

	consumed := int(r.pos) - radius
	if consumed > 0 {
		r.samples = append(r.samples[:0], r.samples[consumed:]...)
		r.pos -= float64(consumed)
	}

	return result
}

var dtmfRows = []float64{697, 770, 852, 941}

var dtmfCols = []float64{1209, 1336, 1477, 1633}

const dtmfKeys = "123A456B789C*0#D"

func tone(d byte, rate int) []int16 {
	idx := strings.IndexByte(dtmfKeys, d)
	if idx < 0 {
		return nil
	}

	out := make([]int16, rate/10)
	for i := range out {
		x := float64(i) / float64(rate)
		gain := 6000.0
		edge := rate / 200
		if i < edge {
			gain *= float64(i) / float64(edge)
		} else if len(out)-1-i < edge {
			gain *= float64(len(out)-1-i) / float64(edge)
		}

		out[i] = int16(gain * (math.Sin(2*math.Pi*dtmfRows[idx/4]*x) + math.Sin(2*math.Pi*dtmfCols[idx%4]*x)))
	}

	return out
}

type detector struct {
	buf       []int16
	last      byte
	candidate byte
	count     int
	rate      int
}

func (d *detector) process(pcm []int16, rate int) []byte {
	if rate != d.rate {
		d.buf = nil
		d.rate = rate
		d.last = 0
		d.count = 0
	}

	d.buf = append(d.buf, pcm...)
	var digits []byte
	n := rate / 50
	for len(d.buf) >= n {
		b := d.buf[:n]
		digit := detectWindow(b, rate)
		if digit == d.candidate {
			d.count++
		} else {
			d.candidate = digit
			d.count = 1
		}

		if d.count >= 2 {
			if digit != 0 && digit != d.last {
				digits = append(digits, digit)
			}

			d.last = digit
		}

		d.buf = d.buf[n:]
	}

	return digits
}

func detectWindow(b []int16, rate int) byte {
	energy := 0.0
	for _, v := range b {
		energy += float64(v) * float64(v)
	}

	if energy/float64(len(b)) < 100000 {
		return 0
	}

	power := func(freq float64) float64 {
		c := 2 * math.Cos(2*math.Pi*freq/float64(rate))
		s1, s2 := 0.0, 0.0
		for _, v := range b {
			s := float64(v) + c*s1 - s2
			s2 = s1
			s1 = s
		}

		return s1*s1 + s2*s2 - c*s1*s2
	}
	best := func(freqs []float64) (int, float64, float64) {
		idx := 0
		p, second := 0.0, 0.0
		for i, f := range freqs {
			v := power(f)
			if v > p {
				second = p
				p = v
				idx = i
			} else if v > second {
				second = v
			}
		}

		return idx, p, second
	}
	row, r, r2 := best(dtmfRows)
	col, c, c2 := best(dtmfCols)
	if r < r2*4 || c < c2*4 || r/c > 4 || c/r > 4 || (r+c) < energy*float64(len(b))*.30 {
		return 0
	}

	return dtmfKeys[row*4+col]
}

// pump converts one direction independently; pacing belongs to the bridge so
// subprocesses and TCP peers cannot burst arbitrary amounts of audio downstream.
func pump(ctx context.Context, src, dst Leg, sn, dn *Node, idle time.Duration) error {
	target := dst.Format()
	var rs resampler
	var detector detector
	var pending []byte
	next := time.Now()
	writeAudio := func(data []byte) error {
		pending = append(pending, data...)
		size := target.frameBytes()
		for len(pending) >= size {
			if wait := time.Until(next); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			} else if wait < -100*time.Millisecond {
				next = time.Now()
			}

			b := append([]byte(nil), pending[:size]...)
			pending = pending[size:]
			if e := dst.Write(ctx, Frame{Format: target, Data: b}); e != nil {
				return e
			}

			next = next.Add(20 * time.Millisecond)
		}

		return nil
	}
	sendDigit := func(d byte) error {
		switch sends(dn) {
		case "none":
			return nil
		case "inband":
			return writeAudio(encode(tone(d, target.Rate), target))
		default:
			return dst.Write(ctx, Frame{Digit: d})
		}
	}
	for {
		readCtx := ctx
		cancel := func() {}
		if idle > 0 {
			readCtx, cancel = context.WithTimeout(ctx, idle)
		}

		f, e := src.Read(readCtx)
		cancel()
		if e != nil {
			if idle > 0 && errors.Is(e, context.DeadlineExceeded) && ctx.Err() == nil {
				return fmt.Errorf("media idle timeout on %s after %s: %w", sn.Name, idle, e)
			}

			return e
		}

		if f.Digit != 0 {
			if e = sendDigit(f.Digit); e != nil {
				return e
			}

			continue
		}

		if len(f.Data) == 0 {
			continue
		}

		needDecode := f.Format != target ||
			receives(sn, "inband") ||
			(dn.Transcode != nil &&
				dn.Transcode.Mode == "always")
		if f.Format != target &&
			((sn.Transcode != nil &&
				sn.Transcode.Mode == "never") ||
				(dn.Transcode != nil &&
					dn.Transcode.Mode == "never")) {
			return errors.New("audio formats incompatible with transcode never")
		}

		if !needDecode {
			e = writeAudio(f.Data)
		} else {
			pcm := decode(f)
			if receives(sn, "inband") {
				for _, d := range detector.process(pcm, f.Format.Rate) {
					if e = sendDigit(d); e != nil {
						return e
					}
				}
			}

			pcm = rs.process(pcm, f.Format.Rate, target.Rate)
			e = writeAudio(encode(pcm, target))
		}

		if e != nil {
			return e
		}
	}
}
