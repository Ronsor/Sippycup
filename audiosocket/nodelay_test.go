package audiosocket

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"
)

type noDelayConn struct {
	net.Conn
	enabled bool
	err     error
}

func (c *noDelayConn) SetNoDelay(enabled bool) error {
	c.enabled = enabled

	return c.err
}

type wrappedConn struct {
	net.Conn
}

func (c *wrappedConn) NetConn() net.Conn {
	return c.Conn
}

func TestWrapEnablesNoDelay(t *testing.T) {
	for _, mode := range []string{"direct", "wrapped", "tls", "unsupported-option"} {
		t.Run(mode, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			socket := &noDelayConn{Conn: left}
			var transport net.Conn = socket
			switch mode {
			case "wrapped":
				transport = &wrappedConn{Conn: socket}
			case "tls":
				transport = tls.Client(&wrappedConn{Conn: socket}, &tls.Config{})
			case "unsupported-option":
				socket.err = errors.New("socket option unavailable")
			}

			conn := Wrap(transport)
			defer conn.Close()
			if !socket.enabled {
				t.Fatal("wrapping did not attempt to enable TCP_NODELAY")
			}

			if conn.stream != transport {
				t.Fatal("wrapping replaced the original transport")
			}
		})
	}
}
