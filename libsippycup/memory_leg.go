package libsippycup

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
)

// MemoryLeg is one end of a bounded, bidirectional in-process call. Writes are
// read by its peer. Audio buffers are copied on Write, so callers may reuse
// them after Write returns; readers own returned buffers. State notifications
// use a separate two-entry queue and never consume media capacity.
type MemoryLeg struct {
	ctx    context.Context
	cancel context.CancelFunc
	format Format
	read   <-chan Frame
	write  chan<- Frame
	states <-chan CallState
	notify chan<- CallState
	mu     sync.Mutex
	state  CallState
}

// NewLegPair creates two connected legs with the same explicit format and a
// positive queue capacity in frames per direction. Closing either end or
// canceling ctx closes both ends and interrupts blocked reads and writes.
// Applications must eventually close the pair, even after setup fails.
// Opus frames use the pair's stream format, including Channels, even when a
// stereo stream contains a mono-encoded packet. See Frame for channel semantics.
func NewLegPair(ctx context.Context, format Format, capacity int) (*MemoryLeg, *MemoryLeg, error) {
	if capacity < 1 {
		return nil, nil, errors.New("leg queue capacity must be positive")
	}
	normalized, err := formatFor(format.Codec, format.Rate, format.Channels)
	if err != nil {
		return nil, nil, err
	}

	if normalized != format {
		return nil, nil, errors.New("leg format must specify codec, sample rate, and channels")
	}
	ctx, cancel := context.WithCancel(ctx)
	a, b := make(chan Frame, capacity), make(chan Frame, capacity)
	as, bs := make(chan CallState, 2), make(chan CallState, 2)
	return &MemoryLeg{ctx: ctx, cancel: cancel, format: format, read: a, write: b, states: as, notify: bs},
		&MemoryLeg{ctx: ctx, cancel: cancel, format: format, read: b, write: a, states: bs, notify: as}, nil
}

// Format returns the pair's immutable media format.
func (l *MemoryLeg) Format() Format { return l.format }

// Done closes when either peer closes or the pair's context is canceled.
func (l *MemoryLeg) Done() <-chan struct{} { return l.ctx.Done() }

// Close closes both ends without waiting for queued frames to drain.
func (l *MemoryLeg) Close() error {
	l.cancel()
	return nil
}

// Read receives one media frame. Closure discards remaining queued frames.
func (l *MemoryLeg) Read(ctx context.Context) (Frame, error) {
	if l.ctx.Err() != nil {
		return Frame{}, io.EOF
	}

	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-l.ctx.Done():
		return Frame{}, io.EOF
	case f := <-l.read:
		return f, nil
	}
}

// Write copies and sends one frame, applying backpressure when the queue is full.
// Digit frames carry native DTMF; audio must use the pair's negotiated format.
func (l *MemoryLeg) Write(ctx context.Context, f Frame) error {
	if l.ctx.Err() != nil {
		return io.ErrClosedPipe
	}

	if f.Digit == 0 {
		if f.Format != l.format || (f.Format.Codec != "opus" &&
			len(f.Data)%(l.format.bytesPerSample()*l.format.Channels) != 0) {
			return errors.New("invalid memory leg audio format or sample alignment")
		}
		if f.Format.Codec == "opus" {
			if _, err := opusSamples(f.Data); err != nil {
				return err
			}
		}
		f.Data = append([]byte(nil), f.Data...)
	} else {
		if !strings.ContainsRune(dtmfKeys, rune(f.Digit)) {
			return errors.New("invalid DTMF digit")
		}
		f.Data = nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ctx.Done():
		return io.ErrClosedPipe
	case l.write <- f:
		return nil
	}
}

// ReadState receives the next early/answered notification from the peer.
func (l *MemoryLeg) ReadState(ctx context.Context) (CallState, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-l.ctx.Done():
		return 0, io.EOF
	case state := <-l.states:
		return state, nil
	}
}

// WriteState sends a monotonic call-state transition. Duplicates are ignored;
// returning from answered to early is an error.
func (l *MemoryLeg) WriteState(ctx context.Context, state CallState) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	if l.ctx.Err() != nil {
		return io.ErrClosedPipe
	}

	if state != StateEarly && state != StateAnswered {
		return errors.New("invalid call state")
	}

	if state < l.state {
		return errors.New("call cannot return to early media")
	}

	if state != l.state {
		l.notify <- state // At most two distinct transitions, with capacity two.
		l.state = state
	}

	return nil
}
