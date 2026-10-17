package libsippycup

import "context"

// earlyMediaMode returns the effective per-leg policy. Explicit configuration
// is required to change answer timing for WebRTC or enable AudioSocket signaling.
func earlyMediaMode(n *Node) string {
	if n.EarlyMedia != "" {
		return n.EarlyMedia
	}

	switch n.Type {
	case "sip", "inproc":
		return "native"
	case "audiosocket", "audiosocket_ws":
		return "answer"
	default:
		return "disabled"
	}
}

// discardEarly drains a pending leg without starting its outbound keepalives.
// This also lets a native AudioSocket peer deliver its answer after audio that
// would otherwise fill the input queue and block the state-message reader.
func discardEarly(ctx context.Context, leg Leg) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if channel, ok := leg.(*channelLeg); ok {
				select {
				case <-channel.frames:
				case <-channel.ended:
					return
				case <-ctx.Done():
					return
				}
			} else if _, err := leg.Read(ctx); err != nil {
				return
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
