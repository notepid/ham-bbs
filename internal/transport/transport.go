// Package transport abstracts the links a BBS session can run over:
// TCP today, AX.25 via AGWPE (Direwolf, soundmodem) and HF modems later.
// The BBS core only sees a byte stream plus who is on the other end.
package transport

import (
	"context"
	"io"
	"time"
)

// Conn is one connected session.
type Conn interface {
	io.ReadWriteCloser
	// Kind names the transport, e.g. "tcp" or "ax25".
	Kind() string
	// Remote describes the peer for logs (address or callsign).
	Remote() string
	// LinkCall is the callsign verified by the link layer (the AX.25
	// source address), or "" when the link does not carry one (TCP).
	LinkCall() string
	// Newline is the line ending human terminals expect on this link.
	Newline() string
}

// Listener accepts incoming sessions.
type Listener interface {
	Accept() (Conn, error)
	Close() error
	String() string
}

// Dialer opens outgoing sessions (used by the client and, later, by
// node-to-node forwarding).
type Dialer interface {
	Dial(ctx context.Context, target string) (Conn, error)
}

// Deadliner is implemented by connections that support read deadlines.
type Deadliner interface {
	SetReadDeadline(t time.Time) error
}
