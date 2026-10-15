package libsippycup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

func makeTLS(c *TLSConfig, server bool) (*tls.Config, error) {
	if c == nil {
		return nil, nil
	}

	t := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	switch c.MinVersion {
	case "", "1.2":
	case "1.3":
		t.MinVersion = tls.VersionTLS13
	default:
		return nil, errors.New("TLS min_version must be 1.2 or 1.3")
	}

	if c.CertificateFile != "" || c.PrivateKeyFile != "" {
		cert, e := tls.LoadX509KeyPair(c.CertificateFile, c.PrivateKeyFile)
		if e != nil {
			return nil, e
		}

		t.Certificates = []tls.Certificate{cert}
	}

	if server && len(t.Certificates) == 0 {
		return nil, errors.New("server TLS requires certificate_file and private_key_file")
	}

	loadCA := func(path string) (*x509.CertPool, error) {
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}

		p := x509.NewCertPool()
		if !p.AppendCertsFromPEM(b) {
			return nil, errors.New("CA file contains no certificates")
		}

		return p, nil
	}
	if c.CAFile != "" {
		p, e := loadCA(c.CAFile)
		if e != nil {
			return nil, e
		}

		t.RootCAs = p
	}

	if c.ClientCAFile != "" {
		p, e := loadCA(c.ClientCAFile)
		if e != nil {
			return nil, e
		}

		t.ClientCAs = p
		t.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return t, nil
}

func httpClient(n *Node, connect time.Duration) (*http.Client, error) {
	t, e := makeTLS(n.TLS, false)
	if e != nil {
		return nil, e
	}

	dial := &net.Dialer{Timeout: connect}
	if n.Bind != "" {
		addr, e := net.ResolveTCPAddr("tcp", n.Bind)
		if e != nil {
			return nil, e
		}

		dial.LocalAddr = addr
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = t
	tr.DialContext = dial.DialContext
	tr.TLSHandshakeTimeout = connect

	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func validateHTTPGroups(nodes []*Node) error {
	groups := map[string][]*Node{}
	for _, n := range nodes {
		if n.Type != "audiosocket_ws" && n.Type != "webrtc_http" {
			continue
		}

		if n.Path == "" {
			return fmt.Errorf("%s: HTTP path is required", n.Name)
		}

		if !strings.HasPrefix(n.Path, "/") ||
			strings.ContainsAny(n.Path, "{}?#") ||
			strings.Contains(n.Path, "//") ||
			strings.HasSuffix(n.Path, "/") &&
				n.Path != "/" {
			return fmt.Errorf("%s: invalid HTTP path", n.Name)
		}

		for _, other := range groups[n.Bind] {
			if n.Path == other.Path ||
				other.Type == "webrtc_http" &&
					strings.HasPrefix(n.Path, strings.TrimSuffix(other.Path, "/")+"/sessions/") ||
				n.Type == "webrtc_http" &&
					strings.HasPrefix(other.Path, strings.TrimSuffix(n.Path, "/")+"/sessions/") {
				return fmt.Errorf("conflicting HTTP paths on %s", n.Bind)
			}

			if !reflect.DeepEqual(n.TLS, other.TLS) {
				return fmt.Errorf("HTTP sources sharing %s must have identical TLS settings", n.Bind)
			}

			var a, b string
			if n.Timeouts != nil {
				a = n.Timeouts.Handshake
			}

			if other.Timeouts != nil {
				b = other.Timeouts.Handshake
			}

			if a != b {
				return fmt.Errorf("HTTP sources sharing %s must have identical handshake timeouts", n.Bind)
			}

			var x, y int
			if n.Limits != nil {
				x = n.Limits.MaxConnections
			}

			if other.Limits != nil {
				y = other.Limits.MaxConnections
			}

			if x != y {
				return fmt.Errorf("HTTP sources sharing %s must have identical connection limits", n.Bind)
			}
		}

		groups[n.Bind] = append(groups[n.Bind], n)
	}

	return nil
}

type countedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

// NetConn exposes the transport for optional socket configuration.
func (c *countedConn) NetConn() net.Conn {
	return c.Conn
}

// Close releases the transport and its capacity reservation exactly once.
func (c *countedConn) Close() error {
	e := c.Conn.Close()
	c.once.Do(c.release)

	return e
}

type countedListener struct {
	net.Listener
	e   *Engine
	n   *Node
	key string
}

// Accept enforces global and source limits on accepted connections.
func (l *countedListener) Accept() (net.Conn, error) {
	for {
		c, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}

		snapshot, node := l.e, l.n
		if l.e.config.source(l.n.Name) == l.n {
			snapshot = l.e.active()
			node = snapshot.config.source(l.n.Name)
		}

		release, ok := snapshot.connection(l.key, node)
		if !ok {
			c.Close()
			continue
		}

		return &countedConn{Conn: c, release: release}, nil
	}
}

func (e *Engine) startHTTP(addr string, nodes []*Node) error {
	raw, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	first := nodes[0]
	var listener net.Listener = &countedListener{raw, e, first, addr}
	conf, err := makeTLS(first.TLS, true)
	if err != nil {
		raw.Close()

		return err
	}

	if conf != nil {
		listener = tls.NewListener(listener, conf)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e.serveHTTP(addr, w, r)
		}),
		ReadHeaderTimeout: duration(e.timeouts(first).Handshake, 10*time.Second),
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32768,
		BaseContext: func(net.Listener) context.Context {
			return e.ctx
		},
	}
	e.addCloser(srv)
	e.addCloser(listener)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && e.ctx.Err() == nil {
			e.log.Error("HTTP server failed", "bind", addr, "error", err)
			e.cancel()
		}
	}()
	e.log.Info("HTTP server listening", "address", listener.Addr().String())

	return nil
}

func checkOrigin(n *Node, w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		allowed := false
		for _, v := range n.Origins {
			if v == origin {
				allowed = true
				break
			}
		}

		if !allowed {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}

			allowed = origin == scheme+"://"+r.Host
		}

		if !allowed {
			http.Error(w, "origin not allowed", 403)

			return false
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "Location, Accept-Patch")
	}

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.WriteHeader(204)

		return false
	}

	return true
}
