// Package bbs is the BBS server core. It accepts sessions from any
// transport, detects whether the peer is a sync client or a human at a
// terminal, and serves both from the same store.
package bbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/identity"
	"github.com/notepid/ham-bbs/internal/proto"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport"
)

const (
	handshakeTimeout = 2 * time.Minute
	idleTimeout      = 10 * time.Minute
	maxEvents        = 200
)

// Server is a running BBS.
type Server struct {
	cfg   config.Server
	store *store.Store
	log   *slog.Logger

	started time.Time
	nextID  atomic.Int64

	mu       sync.Mutex
	sessions map[int64]*Session
	events   []Event

	// Argon2 key derivation uses 64 MiB; bound concurrent terminal
	// password checks so a flood of logins can't exhaust memory.
	kdfSem chan struct{}

	neighbors []*neighbor
	dialers   map[string]transport.Dialer // guarded by mu
	ctx       context.Context             // set by Run; guarded by mu
}

// Session describes a connected peer (for the admin UI).
type Session struct {
	ID        int64
	Call      string
	Transport string
	Remote    string
	Mode      string // "sync", "terminal" or "forward"
	Started   time.Time
}

// Event is a line in the activity log shown in the admin UI.
type Event struct {
	Time time.Time
	Text string
}

// New creates a server.
func New(cfg config.Server, st *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	st.SetLocalNode(cfg.Node)
	return &Server{
		cfg:       cfg,
		store:     st,
		log:       log,
		started:   time.Now(),
		sessions:  map[int64]*Session{},
		kdfSem:    make(chan struct{}, 2),
		neighbors: newNeighbors(cfg),
		dialers:   map[string]transport.Dialer{},
	}
}

// runCtx is the context Run was started with (Background before that).
func (s *Server) runCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// Config returns the server configuration.
func (s *Server) Config() config.Server { return s.cfg }

// Store returns the underlying store.
func (s *Server) Store() *store.Store { return s.store }

// Started returns the start time.
func (s *Server) Started() time.Time { return s.started }

// Sessions returns a snapshot of connected sessions.
func (s *Server) Sessions() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		out = append(out, *ss)
	}
	return out
}

// Events returns recent activity, newest first.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	for i, e := range s.events {
		out[len(s.events)-1-i] = e
	}
	return out
}

func (s *Server) event(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	s.log.Info(text)
	s.mu.Lock()
	s.events = append(s.events, Event{Time: time.Now(), Text: text})
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
	s.mu.Unlock()
}

// Run performs housekeeping and scheduled forwarding until ctx is done.
func (s *Server) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	s.runForwarding(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := s.store.PurgeExpired(time.Now()); err != nil {
			s.log.Error("purge expired", "err", err)
		} else if n > 0 {
			s.event("purged %d expired bulletins", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Serve accepts sessions from l until it is closed.
func (s *Server) Serve(l transport.Listener) error {
	s.log.Info("listening", "on", l.String())
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.Handle(c)
	}
}

// Handle runs one session to completion.
func (s *Server) Handle(c transport.Conn) {
	defer c.Close()
	sess := &Session{
		ID:        s.nextID.Add(1),
		Transport: c.Kind(),
		Remote:    c.Remote(),
		Started:   time.Now(),
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, sess.ID)
		s.mu.Unlock()
	}()
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("session panic", "remote", sess.Remote, "panic", r)
		}
	}()

	nonce := identity.NewNonce()
	nl := c.Newline()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	lr := &proto.LineReader{R: r, Max: 512}

	// The banner identifies the station (callsign) and carries the login
	// nonce, so a sync client can authenticate without an extra round trip.
	fmt.Fprint(w, proto.FormatBanner(s.cfg.Node, nonce, s.bannerText())+nl)
	if call := c.LinkCall(); call != "" {
		fmt.Fprintf(w, "Hello %s, type H for help.%s%s", call, nl, s.prompt())
	} else {
		fmt.Fprint(w, "Callsign: ")
	}
	if err := w.Flush(); err != nil {
		return
	}

	setDeadline(c, handshakeTimeout)
	first, err := lr.ReadLine()
	if err != nil {
		return
	}
	if strings.TrimSpace(first) == proto.Magic {
		s.setSessionMode(sess, "sync")
		s.serveSync(c, sess, r, w, nonce)
		return
	}
	s.setSessionMode(sess, "terminal")
	t := &terminal{srv: s, c: c, sess: sess, lr: lr, w: w, nl: nl}
	t.run(first)
}

func (s *Server) bannerText() string {
	name := s.cfg.Name
	if name == "" {
		name = s.cfg.Node + " BBS"
	}
	return fmt.Sprintf("%s de %s (HamBBS)", name, s.cfg.Callsign)
}

func (s *Server) prompt() string { return s.cfg.Node + "> " }

func (s *Server) setSessionMode(sess *Session, mode string) {
	s.mu.Lock()
	sess.Mode = mode
	s.mu.Unlock()
}

func (s *Server) setSessionCall(sess *Session, call string) {
	s.mu.Lock()
	sess.Call = call
	s.mu.Unlock()
}

func setDeadline(c transport.Conn, d time.Duration) {
	if dl, ok := c.(transport.Deadliner); ok {
		dl.SetReadDeadline(time.Now().Add(d))
	}
}

// login resolves a user from a verified signature. For unknown callsigns
// (or accounts created by a terminal visit, which have no key yet) and open
// registration, the presented public key is adopted.
func (s *Server) login(h proto.Hello, call, nonce string) (registered bool, perr *proto.Error) {
	u, err := s.store.GetUser(call)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && len(u.PubKey) == 0):
		if !s.cfg.OpenRegistration {
			return false, &proto.Error{Code: proto.ErrUnregistered, Text: "unknown callsign; ask the sysop for an account"}
		}
		if !identity.Verify(h.PubKey, s.cfg.Node, call, nonce, h.Sig) {
			return false, &proto.Error{Code: proto.ErrAuth, Text: "bad signature"}
		}
		if errors.Is(err, store.ErrNotFound) {
			err = s.store.CreateUser(call, h.PubKey, "", false)
		} else {
			err = s.store.SetUserKey(call, h.PubKey)
		}
		if err != nil {
			return false, &proto.Error{Code: proto.ErrServer, Text: "could not register"}
		}
		s.event("%s registered", call)
		return true, nil
	case err != nil:
		return false, &proto.Error{Code: proto.ErrServer, Text: "database error"}
	}
	if !identity.Verify(u.PubKey, s.cfg.Node, call, nonce, h.Sig) {
		return false, &proto.Error{Code: proto.ErrAuth, Text: "wrong password for " + call}
	}
	return false, nil
}

// checkPassword verifies a terminal password against the stored key.
func (s *Server) checkPassword(u store.User, password string) bool {
	if len(u.PubKey) == 0 {
		return false
	}
	s.kdfSem <- struct{}{}
	defer func() { <-s.kdfSem }()
	return string(identity.PublicKey(u.Call, password)) == string(u.PubKey)
}
