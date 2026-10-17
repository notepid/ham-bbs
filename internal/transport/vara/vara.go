// Package vara is the transport for VARA-compatible modems (VARA FM/HF,
// VardeFM). The modem handles the radio link, error correction and ARQ;
// we talk to it over its TCP host interface:
//
//   - command port (default 8300): CR-terminated ASCII commands and events
//   - data port (command port + 1): the session's raw bytes
//
// A modem carries one session at a time. The callsigns in the CONNECTED
// event come from the radio link, so they serve as the link-verified
// callsign, as with AX.25.
package vara

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"hambbs/internal/callsign"
	"hambbs/internal/transport"
)

const (
	cmdTimeout    = 5 * time.Second
	drainTimeout  = 3 * time.Minute // wait for the modem to send everything before DISCONNECT
	reportTimeout = 5 * time.Second // wait for a BUFFER report newer than the last write
	discTimeout   = time.Minute     // wait for DISCONNECTED before ABORT
	maxPending    = 1 << 20         // received bytes buffered before the host reads them
	// Data and events use separate sockets, so the last bytes of a session
	// can arrive just after DISCONNECTED. Read waits this long for them.
	lateDataGrace   = 500 * time.Millisecond
	defaultDialTime = 3 * time.Minute
)

// ErrClosed is returned after the modem connection is gone.
var ErrClosed = errors.New("vara: modem connection closed")

// Config describes how to reach the modem.
type Config struct {
	// Addr is the modem's command port, host:port. The data port is the
	// next port number.
	Addr string
	// MyCalls are the callsigns the modem answers to; the first is used
	// as source when connecting.
	MyCalls []string
	Log     *slog.Logger
}

func (c Config) dataAddr() (string, error) {
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return "", fmt.Errorf("vara: modem address %q: %w", c.Addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("vara: modem port %q: %w", port, err)
	}
	return net.JoinHostPort(host, strconv.Itoa(p+1)), nil
}

// modem is an open host connection to a VARA modem.
type modem struct {
	cfg  Config
	log  *slog.Logger
	cmd  net.Conn
	data net.Conn

	replies chan string // OK / WRONG
	writeMu sync.Mutex  // serialises commands

	mu        sync.Mutex
	changed   chan struct{} // closed and replaced on every state change
	connected bool
	peers     [2]string // source, destination of current link
	buffer    int       // BUFFER n: bytes not yet acknowledged
	bufEvents int       // BUFFER reports seen
	writeMark int       // bufEvents at the last write; -1 = nothing written this session
	discs     int       // DISCONNECTED events seen
	discAt    time.Time // when the last DISCONNECTED arrived
	in        []byte    // received data not yet read
	closed    bool
	conn      *Conn // active session, if any
}

func open(ctx context.Context, cfg Config) (*modem, error) {
	dataAddr, err := cfg.dataAddr()
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	cmd, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("vara: connect to modem command port: %w", err)
	}
	data, err := d.DialContext(ctx, "tcp", dataAddr)
	if err != nil {
		cmd.Close()
		return nil, fmt.Errorf("vara: connect to modem data port: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	m := &modem{cfg: cfg, log: log, cmd: cmd, data: data, replies: make(chan string, 8), changed: make(chan struct{})}
	go m.readCommands()
	go m.readData()

	if len(cfg.MyCalls) == 0 {
		m.close()
		return nil, errors.New("vara: no callsign configured")
	}
	if err := m.command("MYCALL " + strings.Join(cfg.MyCalls, " ")); err != nil {
		m.close()
		return nil, err
	}
	return m, nil
}

// notify wakes everyone waiting for a state change. Caller holds m.mu.
func (m *modem) notify() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// wait blocks until cond() holds (checked under m.mu), ctx ends, or the
// modem connection closes.
func (m *modem) wait(ctx context.Context, cond func() bool) error {
	for {
		m.mu.Lock()
		if cond() {
			m.mu.Unlock()
			return nil
		}
		if m.closed {
			m.mu.Unlock()
			return ErrClosed
		}
		ch := m.changed
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *modem) readCommands() {
	defer m.close()
	sc := bufio.NewScanner(m.cmd)
	sc.Split(scanCR)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		m.event(line)
	}
}

func (m *modem) event(line string) {
	fields := strings.Fields(line)
	switch strings.ToUpper(fields[0]) {
	case "OK", "WRONG":
		select {
		case m.replies <- strings.ToUpper(fields[0]):
		default:
		}
		return
	case "IAMALIVE":
		return
	}
	m.log.Debug("vara event", "line", line)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch strings.ToUpper(fields[0]) {
	case "CONNECTED":
		if len(fields) >= 3 {
			m.connected = true
			m.peers = [2]string{callsign.Normalize(fields[1]), callsign.Normalize(fields[2])}
			m.buffer = 0
		}
	case "DISCONNECTED":
		m.connected = false
		m.buffer = 0
		m.discs++
		m.discAt = time.Now()
	case "BUFFER":
		if len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				m.buffer = n
				m.bufEvents++
			}
		}
	default:
		return // PTT, BUSY, PENDING, VERSION, ...: nothing to do
	}
	m.notify()
}

func (m *modem) readData() {
	defer m.close()
	buf := make([]byte, 4096)
	for {
		n, err := m.data.Read(buf)
		if n > 0 {
			m.mu.Lock()
			if len(m.in)+n <= maxPending {
				m.in = append(m.in, buf[:n]...)
			} else {
				m.log.Warn("vara: receive buffer full, dropping data")
			}
			m.notify()
			m.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// command sends one command and waits for OK. A modem that does not
// answer at all is tolerated (not every implementation acknowledges every
// command); WRONG is an error.
func (m *modem) command(c string) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	for len(m.replies) > 0 { // drop stale replies
		<-m.replies
	}
	m.log.Debug("vara command", "cmd", c)
	if _, err := m.cmd.Write([]byte(c + "\r")); err != nil {
		return fmt.Errorf("vara: %s: %w", c, err)
	}
	select {
	case r := <-m.replies:
		if r == "WRONG" {
			return fmt.Errorf("vara: modem rejected %q", c)
		}
	case <-time.After(cmdTimeout):
		m.log.Warn("vara: no reply to command", "cmd", c)
	}
	return nil
}

func (m *modem) isConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

func (m *modem) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.notify()
	m.mu.Unlock()
	m.cmd.Close()
	m.data.Close()
}

// newConn starts a session for the current link. remote is the peer's
// callsign. Caller holds m.mu.
func (m *modem) newConn(remote string) *Conn {
	c := &Conn{m: m, remote: remote}
	m.conn = c
	m.writeMark = -1
	return c
}

// Conn is one VARA session. It implements transport.Conn.
type Conn struct {
	m      *modem
	remote string
	// closeModem is set for dialed sessions, which own their modem link.
	closeModem bool

	mu       sync.Mutex
	deadline time.Time
	done     bool
}

var _ transport.Conn = (*Conn)(nil)
var _ transport.Deadliner = (*Conn)(nil)

func (c *Conn) Kind() string     { return "vara" }
func (c *Conn) Remote() string   { return c.remote + " (vara)" }
func (c *Conn) LinkCall() string { return c.remote }
func (c *Conn) Newline() string  { return "\r" }

// SetReadDeadline implements transport.Deadliner.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

// Read returns received data; after the link drops, buffered data is
// still returned before io.EOF.
func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	ctx := context.Background()
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	m := c.m
	err := m.wait(ctx, func() bool { return len(m.in) > 0 || !m.connected || m.conn != c })
	if err == nil {
		m.mu.Lock()
		late := len(m.in) == 0 && !m.connected && m.conn == c
		until := m.discAt.Add(lateDataGrace)
		m.mu.Unlock()
		if late && time.Now().Before(until) {
			gctx, cancel := context.WithDeadline(ctx, until)
			m.wait(gctx, func() bool { return len(m.in) > 0 })
			cancel()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, os.ErrDeadlineExceeded
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.in) > 0 && m.conn == c {
		n := copy(p, m.in)
		m.in = m.in[n:]
		return n, nil
	}
	if err != nil {
		return 0, err
	}
	return 0, io.EOF
}

// Write queues data for transmission.
func (c *Conn) Write(p []byte) (int, error) {
	m := c.m
	m.mu.Lock()
	ok := m.connected && m.conn == c
	m.mu.Unlock()
	if !ok {
		return 0, io.ErrClosedPipe
	}
	m.mu.Lock()
	m.writeMark = m.bufEvents
	m.mu.Unlock()
	return m.data.Write(p)
}

// Close lets the modem deliver everything written, then disconnects.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return nil
	}
	c.done = true
	c.mu.Unlock()
	m := c.m

	m.mu.Lock()
	connected := m.connected && m.conn == c
	m.mu.Unlock()
	if connected {
		// Data and BUFFER reports travel on different sockets, so a
		// "BUFFER 0" seen right after a write may predate it. Wait for a
		// report newer than the last write (unless the modem never sends
		// one), then for the buffer to empty.
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		m.wait(ctx, func() bool { return !m.connected || m.writeMark < 0 || m.bufEvents > m.writeMark })
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), drainTimeout)
		err := m.wait(ctx, func() bool { return !m.connected || m.buffer == 0 })
		cancel()
		if err == nil {
			m.command("DISCONNECT")
			ctx, cancel = context.WithTimeout(context.Background(), discTimeout)
			err = m.wait(ctx, func() bool { return !m.connected })
			cancel()
		}
		if err != nil && !errors.Is(err, ErrClosed) {
			m.log.Warn("vara: disconnect timed out, aborting", "err", err)
			m.command("ABORT")
		}
	}

	m.mu.Lock()
	if m.conn == c {
		m.conn = nil
		m.in = nil // leftovers of this session must not leak into the next
	}
	m.notify()
	m.mu.Unlock()
	if c.closeModem {
		m.close()
	}
	return nil
}

// Dialer connects to a station through the modem; one modem host
// connection per session.
type Dialer struct {
	Config
	// Timeout bounds the connect attempt (default 3 minutes).
	Timeout time.Duration
}

// Dial connects to the station with callsign target.
func (d Dialer) Dial(ctx context.Context, target string) (transport.Conn, error) {
	target = callsign.Normalize(target)
	if !callsign.Valid(target) {
		return nil, fmt.Errorf("vara: %q is not a callsign", target)
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = defaultDialTime
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	m, err := open(ctx, d.Config)
	if err != nil {
		return nil, err
	}
	m.command("LISTEN OFF")
	m.mu.Lock()
	discs := m.discs
	m.mu.Unlock()
	if err := m.command("CONNECT " + callsign.Normalize(d.MyCalls[0]) + " " + target); err != nil {
		m.close()
		return nil, err
	}
	// Wait for CONNECTED, or DISCONNECTED (no answer).
	err = m.wait(ctx, func() bool { return m.connected || m.discs > discs })
	if err == nil && !m.isConnected() {
		m.close()
		return nil, fmt.Errorf("vara: %s did not answer", target)
	}
	if err != nil {
		if ctx.Err() != nil {
			m.command("ABORT")
		}
		m.close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("vara: no answer from %s", target)
		}
		return nil, err
	}
	m.mu.Lock()
	c := m.newConn(target)
	m.mu.Unlock()
	c.closeModem = true
	return c, nil
}

// Listener answers incoming connections. It keeps one host connection to
// the modem and reconnects if the modem goes away.
type Listener struct {
	cfg    Config
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	m  *modem
}

// Listen starts answering calls to cfg.MyCalls.
func Listen(cfg Config) (*Listener, error) {
	if len(cfg.MyCalls) == 0 {
		return nil, errors.New("vara: no callsign configured")
	}
	if _, err := cfg.dataAddr(); err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Listener{cfg: cfg, log: log, ctx: ctx, cancel: cancel}, nil
}

func (l *Listener) String() string {
	return fmt.Sprintf("vara %s as %s", l.cfg.Addr, strings.Join(l.cfg.MyCalls, ","))
}

// Close stops listening.
func (l *Listener) Close() error {
	l.cancel()
	l.mu.Lock()
	m := l.m
	l.mu.Unlock()
	if m != nil {
		m.command("LISTEN OFF")
		m.close()
	}
	return nil
}

// modem returns a working modem connection, (re)connecting as needed.
func (l *Listener) modem() (*modem, error) {
	backoff := time.Second
	for {
		l.mu.Lock()
		m := l.m
		l.mu.Unlock()
		if m != nil {
			m.mu.Lock()
			closed := m.closed
			m.mu.Unlock()
			if !closed {
				return m, nil
			}
		}
		if l.ctx.Err() != nil {
			return nil, net.ErrClosed
		}
		m, err := open(l.ctx, l.cfg)
		if err == nil {
			err = m.command("LISTEN ON")
		}
		if err == nil {
			l.log.Info("vara modem ready", "addr", l.cfg.Addr, "mycall", strings.Join(l.cfg.MyCalls, " "))
			l.mu.Lock()
			l.m = m
			l.mu.Unlock()
			return m, nil
		}
		if m != nil {
			m.close()
		}
		l.log.Warn("vara modem not reachable, retrying", "addr", l.cfg.Addr, "err", err, "in", backoff)
		select {
		case <-l.ctx.Done():
			return nil, net.ErrClosed
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// Accept waits for the next incoming session.
func (l *Listener) Accept() (transport.Conn, error) {
	for {
		m, err := l.modem()
		if err != nil {
			return nil, err
		}
		// Wait until a link is up and no session is using it.
		err = m.wait(l.ctx, func() bool { return m.connected && m.conn == nil })
		if errors.Is(err, ErrClosed) {
			l.log.Warn("vara modem connection lost")
			continue
		}
		if err != nil {
			return nil, net.ErrClosed
		}
		m.mu.Lock()
		if !m.connected || m.conn != nil {
			m.mu.Unlock()
			continue
		}
		// The caller is the source of the CONNECTED event.
		c := m.newConn(m.peers[0])
		m.mu.Unlock()
		return c, nil
	}
}

// scanCR splits on CR, LF or CRLF.
func scanCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
