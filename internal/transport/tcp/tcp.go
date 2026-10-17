// Package tcp is the TCP/IP transport: internet, HamNet/AREDN, and local
// testing. Telnet clients work against it in terminal mode.
package tcp

import (
	"context"
	"net"
	"time"

	"github.com/notepid/ham-bbs/internal/transport"
)

type conn struct {
	net.Conn
}

func (c conn) Kind() string     { return "tcp" }
func (c conn) Remote() string   { return c.Conn.RemoteAddr().String() }
func (c conn) LinkCall() string { return "" }
func (c conn) Newline() string  { return "\r\n" }

// SetReadDeadline is promoted from net.Conn; conn satisfies
// transport.Deadliner.
var _ transport.Deadliner = conn{}

type listener struct {
	l net.Listener
}

// Listen starts a TCP listener on addr (e.g. ":6300").
func Listen(addr string) (transport.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &listener{l: l}, nil
}

func (l *listener) Accept() (transport.Conn, error) {
	c, err := l.l.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(time.Minute)
	}
	return conn{c}, nil
}

func (l *listener) Close() error   { return l.l.Close() }
func (l *listener) String() string { return "tcp " + l.l.Addr().String() }

// Addr returns the bound address (useful when listening on port 0).
func (l *listener) Addr() net.Addr { return l.l.Addr() }

// Dialer dials TCP targets of the form host:port.
type Dialer struct {
	Timeout time.Duration
}

func (d Dialer) Dial(ctx context.Context, target string) (transport.Conn, error) {
	nd := net.Dialer{Timeout: d.Timeout}
	if nd.Timeout == 0 {
		nd.Timeout = 30 * time.Second
	}
	c, err := nd.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	return conn{c}, nil
}
