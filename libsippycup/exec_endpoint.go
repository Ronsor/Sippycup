package libsippycup

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/ronsor/sippycup/audiosocket"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// exec endpoints use fd 3 for events to the child and fd 4 for child commands.
// Audio is always stdin/stdout; stderr is inherited for application diagnostics.
func (e *Engine) execEndpoint(
	ctx context.Context,
	n *Node,
	req *Request,
	eval *hcl.EvalContext,
	action *Connect,
	fallback Format,
) (Leg, error) {
	f := nodeFormat(n, fallback)
	if f.Codec == "opus" {
		if n.Transcode != nil && n.Transcode.Mode == "never" {
			return nil, errors.New("exec cannot stream packetized Opus with transcode never")
		}
		f = Format{"pcm_s16le", 48000, 1}
	}
	if n.IO == "pcm" && f.Codec != "pcm_s16le" {
		return nil, errors.New("io=pcm requires PCM format; configure transcode")
	}

	childCtx, childCancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, n.Command[0], n.Command[1:]...)
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = duration(e.timeouts(n).Shutdown, 5*time.Second)
	if err := configureChildProcess(cmd, n.Control); err != nil {
		childCancel()

		return nil, err
	}

	input, err := cmd.StdinPipe()
	if err != nil {
		childCancel()

		return nil, err
	}

	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		childCancel()

		return nil, err
	}

	files := []*os.File{}
	var events *os.File
	var commands *os.File
	if n.Control == "json_fd" {
		er, ew, err := os.Pipe()
		if err != nil {
			input.Close()
			output.Close()
			childCancel()

			return nil, err
		}

		cr, cw, err := os.Pipe()
		if err != nil {
			er.Close()
			ew.Close()
			input.Close()
			output.Close()
			childCancel()

			return nil, err
		}

		cmd.ExtraFiles = []*os.File{er, cw}
		events = ew
		commands = cr
		files = []*os.File{er, ew, cr, cw}
	}

	metadata := map[string]any{
		"id":       req.ID,
		"source":   req.Source,
		"identity": req.Identity,
		"format":   f,
	}
	if action.Metadata != nil {
		v, d := action.Metadata.Value(eval)
		if d.HasErrors() {
			for _, v := range files {
				v.Close()
			}

			input.Close()
			output.Close()
			childCancel()

			return nil, errors.New("metadata expression failed")
		}

		b, err := ctyjson.Marshal(v, v.Type())
		if err != nil {
			for _, f := range files {
				f.Close()
			}

			input.Close()
			output.Close()
			childCancel()

			return nil, err
		}

		var custom any
		if json.Unmarshal(b, &custom) != nil {
			for _, f := range files {
				f.Close()
			}

			input.Close()
			output.Close()
			childCancel()

			return nil, errors.New("invalid metadata")
		}

		metadata["metadata"] = custom
	}

	b, _ := json.Marshal(metadata)
	cmd.Env = append(
		execEnvironment(n),
		"CALL_ID="+req.ID,
		"AUDIO_CODEC="+f.Codec,
		"AUDIO_SAMPLE_RATE="+strconv.Itoa(f.Rate),
		"AUDIO_CHANNELS=1",
	)
	if err = cmd.Start(); err != nil {
		for _, v := range files {
			v.Close()
		}

		input.Close()
		output.Close()
		childCancel()

		return nil, err
	}

	for _, v := range cmd.ExtraFiles {
		v.Close()
	}

	l := newChannelLeg(childCtx, f, e.depth(n))
	var audioMu, controlMu sync.Mutex
	done := make(chan struct{})
	sendEvent := func(v any) error {
		if events == nil {
			return nil
		}

		controlMu.Lock()
		defer controlMu.Unlock()
		_ = events.SetWriteDeadline(time.Now().Add(duration(e.timeouts(n).Handshake, 10*time.Second)))

		return json.NewEncoder(events).Encode(v)
	}
	l.closeFn = func() error {
		childCancel()
		input.Close()
		output.Close()
		for _, f := range files {
			f.Close()
		}

		<-done

		return nil
	}
	l.writeFn = func(ctx context.Context, f Frame) error {
		if f.Digit != 0 {
			if n.IO == "audiosocket" {
				audioMu.Lock()
				defer audioMu.Unlock()

				return audiosocket.WritePacket(
					input,
					audiosocket.Packet{Kind: audiosocket.KindDTMF, Payload: []byte{f.Digit}},
				)
			}

			return sendEvent(map[string]any{"type": "dtmf", "digit": string(f.Digit)})
		}

		audioMu.Lock()
		defer audioMu.Unlock()
		if n.IO == "audiosocket" {
			kind, err := audiosocket.AudioKind(f.Format.Rate)
			if err != nil {
				return err
			}

			return audiosocket.WritePacket(input, audiosocket.Packet{Kind: kind, Payload: f.Data})
		}

		return writeAll(input, f.Data)
	}
	if events != nil {
		go func() {
			if err := sendEvent(map[string]any{"type": "start", "call": json.RawMessage(b)}); err != nil {
				l.fail(err)
			}
		}()
		go func() {
			scanner := bufio.NewScanner(commands)
			scanner.Buffer(make([]byte, 1024), 8192)
			for scanner.Scan() {
				var msg struct {
					Type  string `json:"type"`
					Digit string `json:"digit"`
				}
				if json.Unmarshal(scanner.Bytes(), &msg) != nil {
					l.fail(errors.New("invalid application control JSON"))

					return
				}

				switch msg.Type {
				case "dtmf":
					if len(msg.Digit) != 1 || !strings.ContainsAny(msg.Digit, "0123456789*#ABCD") {
						l.fail(errors.New("invalid application DTMF digit"))

						return
					}
					if receives(n, "json") {
						if !l.emit(Frame{Digit: msg.Digit[0]}) {
							return
						}
					}
				case "hangup":
					l.fail(io.EOF)
					return
				default:
					l.fail(fmt.Errorf("unknown application command %q", msg.Type))
					return
				}
			}

			if err := scanner.Err(); err != nil && childCtx.Err() == nil {
				l.fail(err)
			}
		}()
	}

	if n.IO == "audiosocket" {
		identity, _ := hex.DecodeString(req.ID)
		if n.Extended {
			var uuid [16]byte
			copy(uuid[:], identity)
			metadata["uuid"] = fmt.Sprintf("%x-%x-%x-%x-%x",
				uuid[:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
			identity, _ = json.Marshal(metadata)
		}

		audioMu.Lock()
		go func() {
			defer audioMu.Unlock()
			packet := audiosocket.Packet{Kind: audiosocket.KindID, Payload: identity}
			if err := audiosocket.WritePacket(input, packet); err != nil {
				l.fail(err)
			}
		}()
	}

	go func() {
		defer func() {
			l.fail(io.EOF)
			err := cmd.Wait()
			close(done)
			l.fail(err)
		}()
		for {
			if n.IO == "audiosocket" {
				p, err := audiosocket.ReadPacket(output, 65535)
				if err != nil {
					return
				}

				if p.Kind == audiosocket.KindHangup {
					return
				}

				if rate, ok := audiosocket.AudioRate(p.Kind); ok {
					if len(p.Payload)%2 != 0 {
						l.fail(errors.New("odd AudioSocket audio length"))

						return
					}

					if !l.emit(Frame{Format: Format{"pcm_s16le", rate, 1}, Data: p.Payload}) {
						return
					}
				} else if p.Kind == audiosocket.KindDTMF && len(p.Payload) == 1 && receives(n, "audiosocket") {
					if !l.emit(Frame{Digit: p.Payload[0]}) {
						return
					}
				}

				continue
			}

			b := make([]byte, f.frameBytes())
			count, err := io.ReadFull(output, b)
			if count > 0 {
				if count%f.bytesPerSample() != 0 {
					l.fail(errors.New("partial audio sample from application"))

					return
				}

				if !l.emit(Frame{Format: f, Data: b[:count]}) {
					return
				}
			}

			if err != nil {
				return
			}
		}
	}()

	return l, nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}

		if n == 0 {
			return io.ErrShortWrite
		}

		b = b[n:]
	}

	return nil
}
