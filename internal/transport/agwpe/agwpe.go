// Package agwpe is the AX.25 transport. It talks to a TNC that offers the
// AGWPE TCP interface: Direwolf (Linux, Windows, Raspberry Pi), UZ7HO
// soundmodem, AGWPE itself, and others. The TNC runs AX.25 connected mode
// (SABM/UA, I-frames, acknowledgements, retries); we only see a byte
// stream per session, plus the callsign of the station on the other end.
//
// Unlike a VARA modem, one TNC connection carries any number of AX.25
// sessions at once; they are told apart by the pair of callsigns.
package agwpe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/transport"
)

// DefaultAddr is where Direwolf listens for AGWPE clients.
const DefaultAddr = "127.0.0.1:8000"

const (
	cmdTimeout = 5 * time.Second
	paclen     = 256 // bytes per 'D' frame; Direwolf's default N1
	pidNoL3    = 0xF0
	// maxQueued is how many frames Write lets the TNC hold before it waits.
	// Keeping the TNC queue short means a dropped link loses little and
	// Close doesn't wait long.
	maxQueued    = 8
	pollInterval = 500 * time.Millisecond
	// Close waits for the TNC to deliver everything (Direwolf discards
	// queued data on disconnect). It gives up when nothing moves this long.
	stallTimeout    = 2 * time.Minute
	discTimeout     = time.Minute
	maxPending      = 1 << 20 // received bytes buffered before the host reads them
	defaultDialTime = 2 * time.Minute
	maxDigis        = 7
)

// ErrClosed is returned after the TNC connection is gone.
var ErrClosed = errors.New("agwpe: TNC connection closed")

var errNoReport = errors.New("agwpe: TNC does not report outstanding frames")

// Config describes how to reach the TNC.
type Config struct {
	// Addr is the TNC's AGWPE port, host:port (Direwolf: 127.0.0.1:8000).
	Addr string
	// Port is the TNC radio port: 0 for the first (Direwolf channel 0).
	Port int
	// MyCalls are the callsigns to answer calls on; the first is used as
	// source when connecting.
	MyCalls []string
	Log     *slog.Logger
}

type key struct{ own, remote string }

// tnc is an open AGWPE connection.
type tnc struct {
	cfg     Config
	log     *slog.Logger
	conn    net.Conn
	writeMu sync.Mutex

	mu       sync.Mutex
	changed  chan struct{} // closed and replaced on every state change
	closed   bool
	sessions map[key]*Conn
	incoming []*Conn         // answered calls not yet accepted
	listen   map[string]bool // callsigns we answer on
	regs     map[string]byte // replies to 'X' (1 = registered)
	ports    string          // reply to 'G'
}

func open(ctx context.Context, cfg Config) (*tnc, error) {
	if cfg.Port < 0 || cfg.Port > 255 {
		return nil, fmt.Errorf("agwpe: radio port %d out of range", cfg.Port)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("agwpe: connect to TNC: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	t := &tnc{
		cfg: cfg, log: log, conn: conn, changed: make(chan struct{}),
		sessions: map[key]*Conn{}, listen: map[string]bool{}, regs: map[string]byte{},
	}
	go t.read()
	return t, nil
}

// notify wakes everyone waiting for a state change. Caller holds t.mu.
func (t *tnc) notify() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// wait blocks until cond() holds (checked under t.mu), ctx ends, or the
// TNC connection closes.
func (t *tnc) wait(ctx context.Context, cond func() bool) error {
	for {
		t.mu.Lock()
		if cond() {
			t.mu.Unlock()
			return nil
		}
		if t.closed {
			t.mu.Unlock()
			return ErrClosed
		}
		ch := t.changed
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *tnc) send(f Frame) error {
	f.Port = byte(t.cfg.Port)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.log.Debug("agwpe send", "frame", f.String())
	if err := WriteFrame(t.conn, f); err != nil {
		return fmt.Errorf("agwpe: write to TNC: %w", err)
	}
	return nil
}

func (t *tnc) read() {
	defer t.close()
	for {
		f, err := ReadFrame(t.conn)
		if err != nil {
			return
		}
		t.handle(f)
	}
}

func (t *tnc) handle(f Frame) {
	if f.Kind != 'D' {
		t.log.Debug("agwpe recv", "frame", f.String(), "text", strings.TrimRight(string(f.Data), "\r\x00"))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.Kind != 'G' && f.Kind != 'R' && int(f.Port) != t.cfg.Port {
		return // another radio port
	}
	// In 'C', 'D' and 'd' frames from the TNC, From is the remote station.
	k := key{own: f.To, remote: f.From}
	switch f.Kind {
	case 'C':
		if s := t.sessions[k]; s != nil {
			s.connected = true
		} else if t.listen[f.To] {
			c := &Conn{t: t, own: f.To, remote: f.From, connected: true}
			t.sessions[k] = c
			t.incoming = append(t.incoming, c)
		} else {
			return
		}
	case 'D':
		s := t.sessions[k]
		if s == nil {
			return
		}
		if len(s.in)+len(f.Data) > maxPending {
			t.log.Warn("agwpe: receive buffer full, dropping data", "remote", f.From)
			return
		}
		s.in = append(s.in, f.Data...)
	case 'd':
		s := t.sessions[k]
		if s == nil {
			return
		}
		s.connected = false
		s.ended = true
		s.reason = strings.TrimSpace(strings.Trim(string(f.Data), "\x00*\r "))
		delete(t.sessions, k)
	case 'Y':
		// Here From is our own callsign.
		s := t.sessions[key{own: f.From, remote: f.To}]
		if s == nil || len(f.Data) < 4 {
			return
		}
		s.outstanding = int(binary.LittleEndian.Uint32(f.Data))
		s.reports++
	case 'X':
		if len(f.Data) >= 1 {
			t.regs[f.From] = f.Data[0]
		}
	case 'G':
		t.ports = strings.TrimRight(string(f.Data), "\x00")
	default:
		return
	}
	t.notify()
}

func (t *tnc) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	for k, s := range t.sessions {
		s.connected = false
		s.ended = true
		delete(t.sessions, k)
	}
	t.notify()
	t.mu.Unlock()
	t.conn.Close()
}

// portInfo asks for the TNC's radio ports ("2;Port1 ...;Port2 ...;").
// TNCs that don't answer are tolerated.
func (t *tnc) portInfo(ctx context.Context) string {
	if err := t.send(Frame{Kind: 'G'}); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	t.wait(ctx, func() bool { return t.ports != "" })
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ports
}

// register asks the TNC to pass us calls for call.
func (t *tnc) register(ctx context.Context, call string) error {
	t.mu.Lock()
	t.listen[call] = true
	delete(t.regs, call)
	t.mu.Unlock()
	if err := t.send(Frame{Kind: 'X', From: call}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	err := t.wait(ctx, func() bool { _, ok := t.regs[call]; return ok })
	if errors.Is(err, ErrClosed) {
		return err
	}
	t.mu.Lock()
	ok, answered := t.regs[call]
	t.mu.Unlock()
	if !answered {
		t.log.Warn("agwpe: TNC did not confirm callsign registration", "call", call)
		return nil
	}
	if ok != 1 {
		return fmt.Errorf("agwpe: TNC refused to register %s (is another program using that callsign?)", call)
	}
	return nil
}

// Conn is one AX.25 session. It implements transport.Conn.
type Conn struct {
	t           *tnc
	own, remote string
	via         []string
	closeTNC    bool // dialed sessions own their TNC connection

	// Guarded by t.mu.
	connected   bool
	ended       bool
	reason      string // TNC's text when the link ended
	in          []byte
	outstanding int // frames the TNC holds (last report + frames sent since)
	reports     int // 'Y' replies seen

	mu       sync.Mutex
	deadline time.Time
	done     bool
	noReport bool // the TNC never answered 'Y'
}

var _ transport.Conn = (*Conn)(nil)
var _ transport.Deadliner = (*Conn)(nil)

func (c *Conn) Kind() string { return "ax25" }
func (c *Conn) Remote() string {
	if len(c.via) > 0 {
		return c.remote + " via " + strings.Join(c.via, ",") + " (ax25)"
	}
	return c.remote + " (ax25)"
}
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
	t := c.t
	err := t.wait(ctx, func() bool { return len(c.in) > 0 || !c.connected })
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, os.ErrDeadlineExceeded
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(c.in) > 0 {
		n := copy(p, c.in)
		c.in = c.in[n:]
		return n, nil
	}
	if err != nil && !errors.Is(err, ErrClosed) {
		return 0, err
	}
	return 0, io.EOF
}

// Write hands data to the TNC in frames of at most paclen bytes. It
// blocks while the TNC already holds maxQueued frames for this session.
func (c *Conn) Write(p []byte) (int, error) {
	t := c.t
	n := 0
	for len(p) > 0 {
		if err := c.throttle(); err != nil {
			return n, err
		}
		chunk := p[:min(paclen, len(p))]
		t.mu.Lock()
		ok := c.connected
		t.mu.Unlock()
		if !ok {
			return n, io.ErrClosedPipe
		}
		if err := t.send(Frame{Kind: 'D', PID: pidNoL3, From: c.own, To: c.remote, Data: chunk}); err != nil {
			return n, err
		}
		t.mu.Lock()
		c.outstanding++
		t.mu.Unlock()
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

func (c *Conn) throttle() error {
	t := c.t
	for {
		t.mu.Lock()
		queued, ok := c.outstanding, c.connected
		t.mu.Unlock()
		if !ok {
			return io.ErrClosedPipe
		}
		if queued < maxQueued {
			return nil
		}
		n, err := c.queryOutstanding()
		if errors.Is(err, errNoReport) {
			return nil // can't tell; let the TNC queue it
		}
		if err != nil {
			return err
		}
		if n < maxQueued {
			return nil
		}
		c.pause()
	}
}

// pause waits pollInterval, or less if the link drops.
func (c *Conn) pause() {
	ctx, cancel := context.WithTimeout(context.Background(), pollInterval)
	defer cancel()
	c.t.wait(ctx, func() bool { return !c.connected })
}

// queryOutstanding asks the TNC how many frames of this session are
// queued or sent but not yet acknowledged.
func (c *Conn) queryOutstanding() (int, error) {
	c.mu.Lock()
	noReport := c.noReport
	c.mu.Unlock()
	if noReport {
		return 0, errNoReport
	}
	t := c.t
	t.mu.Lock()
	seen := c.reports
	t.mu.Unlock()
	if err := t.send(Frame{Kind: 'Y', From: c.own, To: c.remote}); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	err := t.wait(ctx, func() bool { return c.reports > seen || !c.connected })
	if errors.Is(err, context.DeadlineExceeded) {
		t.log.Warn("agwpe: TNC does not answer outstanding-frames queries; the end of a session may be cut off")
		c.mu.Lock()
		c.noReport = true
		c.mu.Unlock()
		return 0, errNoReport
	}
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !c.connected {
		return 0, io.ErrClosedPipe
	}
	return c.outstanding, nil
}

// Close lets the TNC deliver everything written, then disconnects.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return nil
	}
	c.done = true
	c.mu.Unlock()
	t := c.t

	t.mu.Lock()
	connected := c.connected
	t.mu.Unlock()
	if connected {
		// Direwolf throws away queued data when asked to disconnect, so
		// wait until every frame is acknowledged.
		last, progress := -1, time.Now()
		for {
			n, err := c.queryOutstanding()
			if err != nil || n == 0 {
				break
			}
			if n != last {
				last, progress = n, time.Now()
			} else if time.Since(progress) > stallTimeout {
				t.log.Warn("agwpe: link stalled, disconnecting with data unsent", "remote", c.remote, "frames", n)
				break
			}
			c.pause()
		}
		t.mu.Lock()
		connected = c.connected
		t.mu.Unlock()
		if connected && t.send(Frame{Kind: 'd', From: c.own, To: c.remote}) == nil {
			ctx, cancel := context.WithTimeout(context.Background(), discTimeout)
			if err := t.wait(ctx, func() bool { return !c.connected }); errors.Is(err, context.DeadlineExceeded) {
				t.log.Warn("agwpe: no disconnect confirmation", "remote", c.remote)
			}
			cancel()
		}
	}

	t.mu.Lock()
	k := key{c.own, c.remote}
	if t.sessions[k] == c {
		delete(t.sessions, k)
	}
	c.connected = false
	c.ended = true
	c.in = nil
	t.notify()
	t.mu.Unlock()
	if c.closeTNC {
		t.close()
	}
	return nil
}

var (
	digiRe = regexp.MustCompile(`^[A-Z0-9]{1,6}(-([0-9]|1[0-5]))?$`)
	sepRe  = regexp.MustCompile(`[\s,]+`)
)

// ParseTarget parses "CALL", "CALL via DIGI1,DIGI2" or "CALL v DIGI".
func ParseTarget(s string) (dest string, via []string, err error) {
	f := sepRe.Split(strings.TrimSpace(strings.ToUpper(s)), -1)
	if len(f) == 0 || f[0] == "" {
		return "", nil, errors.New("no callsign given")
	}
	dest = f[0]
	if !callsign.Valid(dest) || !digiRe.MatchString(dest) {
		return "", nil, fmt.Errorf("%q is not an AX.25 callsign", dest)
	}
	rest := f[1:]
	if len(rest) > 0 && (rest[0] == "VIA" || rest[0] == "V") {
		rest = rest[1:]
	} else if len(rest) > 0 {
		return "", nil, fmt.Errorf("expected \"via\" after %s", dest)
	}
	for _, d := range rest {
		if !digiRe.MatchString(d) {
			return "", nil, fmt.Errorf("%q is not a valid digipeater", d)
		}
		via = append(via, d)
	}
	if len(via) > maxDigis {
		return "", nil, fmt.Errorf("at most %d digipeaters", maxDigis)
	}
	return dest, via, nil
}

// Dialer connects to a station through the TNC; one TNC connection per
// session.
type Dialer struct {
	Config
	// Timeout bounds the connect attempt (default 2 minutes). The TNC
	// usually gives up sooner, after its retry count.
	Timeout time.Duration
}

// Dial connects to target, given as for ParseTarget.
func (d Dialer) Dial(ctx context.Context, target string) (transport.Conn, error) {
	dest, via, err := ParseTarget(target)
	if err != nil {
		return nil, fmt.Errorf("agwpe: %w", err)
	}
	if len(d.MyCalls) == 0 {
		return nil, errors.New("agwpe: no callsign configured")
	}
	own := callsign.Normalize(d.MyCalls[0])
	if !digiRe.MatchString(own) {
		return nil, fmt.Errorf("agwpe: %q is not an AX.25 callsign", own)
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = defaultDialTime
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	t, err := open(ctx, d.Config)
	if err != nil {
		return nil, err
	}
	c := &Conn{t: t, own: own, remote: dest, via: via, closeTNC: true}
	t.mu.Lock()
	t.sessions[key{own, dest}] = c
	t.mu.Unlock()
	f := Frame{Kind: 'C', From: own, To: dest}
	if len(via) > 0 {
		f.Kind, f.Data = 'v', viaData(via)
	}
	if err := t.send(f); err != nil {
		t.close()
		return nil, err
	}
	err = t.wait(ctx, func() bool { return c.connected || c.ended })
	t.mu.Lock()
	connected, reason := c.connected, c.reason
	t.mu.Unlock()
	switch {
	case err == nil && connected:
		return c, nil
	case err == nil:
		t.close()
		if reason != "" {
			return nil, fmt.Errorf("agwpe: %s did not answer (%s)", dest, reason)
		}
		return nil, fmt.Errorf("agwpe: %s did not answer", dest)
	default:
		if ctx.Err() != nil {
			t.send(Frame{Kind: 'd', From: own, To: dest})
		}
		t.close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("agwpe: no answer from %s", dest)
		}
		return nil, err
	}
}

// Listener answers incoming connections. It keeps one TNC connection and
// reconnects if the TNC goes away.
type Listener struct {
	cfg    Config
	calls  []string
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	t  *tnc
}

// Listen starts answering calls to cfg.MyCalls.
func Listen(cfg Config) (*Listener, error) {
	var calls []string
	for _, c := range cfg.MyCalls {
		c = callsign.Normalize(c)
		if !digiRe.MatchString(c) {
			return nil, fmt.Errorf("agwpe: %q is not an AX.25 callsign", c)
		}
		calls = append(calls, c)
	}
	if len(calls) == 0 {
		return nil, errors.New("agwpe: no callsign configured")
	}
	if cfg.Port < 0 || cfg.Port > 255 {
		return nil, fmt.Errorf("agwpe: radio port %d out of range", cfg.Port)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Listener{cfg: cfg, calls: calls, log: log, ctx: ctx, cancel: cancel}, nil
}

func (l *Listener) String() string {
	return fmt.Sprintf("ax25 %s port %d as %s", l.cfg.Addr, l.cfg.Port, strings.Join(l.calls, ","))
}

// Close stops listening and ends the sessions in progress.
func (l *Listener) Close() error {
	l.cancel()
	l.mu.Lock()
	t := l.t
	l.mu.Unlock()
	if t != nil {
		for _, c := range l.calls {
			t.send(Frame{Kind: 'x', From: c})
		}
		t.close()
	}
	return nil
}

// tnc returns a working TNC connection, (re)connecting as needed.
func (l *Listener) tnc() (*tnc, error) {
	backoff := time.Second
	for {
		l.mu.Lock()
		t := l.t
		l.mu.Unlock()
		if t != nil {
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if !closed {
				return t, nil
			}
		}
		if l.ctx.Err() != nil {
			return nil, net.ErrClosed
		}
		t, err := l.setup()
		if err == nil {
			l.mu.Lock()
			l.t = t
			l.mu.Unlock()
			return t, nil
		}
		l.log.Warn("AX.25 TNC not ready, retrying", "addr", l.cfg.Addr, "err", err, "in", backoff)
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

func (l *Listener) setup() (*tnc, error) {
	t, err := open(l.ctx, l.cfg)
	if err != nil {
		return nil, err
	}
	ports := t.portInfo(l.ctx)
	if n, err := strconv.Atoi(strings.SplitN(ports, ";", 2)[0]); err == nil && l.cfg.Port >= n {
		t.close()
		return nil, fmt.Errorf("agwpe: radio port %d does not exist; the TNC has %d (%s)", l.cfg.Port, n, ports)
	}
	for _, c := range l.calls {
		if err := t.register(l.ctx, c); err != nil {
			t.close()
			return nil, err
		}
	}
	l.log.Info("AX.25 TNC ready", "addr", l.cfg.Addr, "port", l.cfg.Port, "mycall", strings.Join(l.calls, " "), "ports", ports)
	return t, nil
}

// Accept waits for the next incoming session.
func (l *Listener) Accept() (transport.Conn, error) {
	for {
		t, err := l.tnc()
		if err != nil {
			return nil, err
		}
		err = t.wait(l.ctx, func() bool { return len(t.incoming) > 0 })
		if errors.Is(err, ErrClosed) {
			if l.ctx.Err() != nil {
				return nil, net.ErrClosed
			}
			l.log.Warn("AX.25 TNC connection lost")
			continue
		}
		if err != nil {
			return nil, net.ErrClosed
		}
		t.mu.Lock()
		c := t.incoming[0]
		t.incoming = t.incoming[1:]
		t.mu.Unlock()
		return c, nil
	}
}
