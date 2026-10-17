package bbs

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/notepid/ham-bbs/internal/buildinfo"
	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/identity"
	"github.com/notepid/ham-bbs/internal/proto"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport"
)

// Forwarding between BBS nodes (see docs/protocol.md §7).
//
// Every stored message that has to go to a neighbour gets a row in that
// neighbour's queue. A session exchanges the queues both ways:
//
//	caller:   HELLO(node) OFFER END
//	answerer: WELCOME WANT OFFER END     (or BYE if there is nothing to do)
//	caller:   MESSAGES WANT END           (or BYE)
//	answerer: PUTRESULT MESSAGES BYE
//
// Offered messages the other side already has are dropped from the queue.
// The answerer's last MESSAGES are not acknowledged in the session; they
// stay queued and are offered again next time, when the other side says
// it has them.

const (
	offerLimit     = 100       // messages offered per session and direction
	frameBudget    = 256 << 10 // body bytes per MESSAGES frame
	forwardTimeout = 30 * time.Minute
	retryBacklog   = time.Minute // next call when the queue was not emptied
)

// ErrBusy is returned by ForwardNow while a session with the neighbour is
// already running.
var ErrBusy = errors.New("forwarding with this neighbour is already in progress")

// neighbor is a configured neighbour node.
type neighbor struct {
	config.Neighbor
	key  ed25519.PrivateKey // derived from our callsign and the link password
	peer ed25519.PublicKey  // derived from their callsign and the link password
	run  sync.Mutex         // one outgoing session at a time

	status NeighborStatus // guarded by Server.mu
}

// NeighborStatus is shown in the admin UI.
type NeighborStatus struct {
	Node        string
	Call        string
	Connect     string
	Interval    int
	Queued      int
	Running     bool
	LastAttempt time.Time
	LastOK      time.Time
	LastError   string
	Sent        int // since start
	Received    int
}

// ForwardResult counts what one session moved.
type ForwardResult struct {
	Sent     int
	Received int
	// Backlog is set when we offered a full batch, so more may be waiting.
	Backlog bool
}

func newNeighbors(cfg config.Server) []*neighbor {
	mine := callsign.Base(cfg.Callsign)
	var out []*neighbor
	for _, n := range cfg.Neighbors {
		nb := &neighbor{
			Neighbor: n,
			key:      identity.DeriveKey(mine, n.Password),
			peer:     identity.PublicKey(callsign.Base(n.Call), n.Password),
		}
		nb.status = NeighborStatus{Node: n.Node, Call: n.Call, Connect: n.Connect, Interval: n.Interval}
		out = append(out, nb)
	}
	return out
}

func (s *Server) neighbor(node string) *neighbor {
	node = strings.ToUpper(strings.TrimSpace(node))
	for _, nb := range s.neighbors {
		if nb.Node == node {
			return nb
		}
	}
	return nil
}

// SetDialer registers how to make outgoing calls of a kind ("tcp",
// "ax25", "vara") for forwarding.
func (s *Server) SetDialer(kind string, d transport.Dialer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dialers[kind] = d
}

func (s *Server) dialer(kind string) transport.Dialer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dialers[kind]
}

// Neighbors returns the neighbours' status.
func (s *Server) Neighbors() []NeighborStatus {
	counts, _ := s.store.QueueCounts()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]NeighborStatus, 0, len(s.neighbors))
	for _, nb := range s.neighbors {
		st := nb.status
		st.Queued = counts[nb.Node]
		out = append(out, st)
	}
	return out
}

// Held returns personal mail waiting for a route.
func (s *Server) Held() ([]store.Message, error) {
	return s.store.Queued("", 500)
}

// storeMessage saves an accepted message and queues it for forwarding.
func (s *Server) storeMessage(m proto.Message) (seq int64, dup bool, err error) {
	seq, dup, err = s.store.AddMessage(m)
	if err == nil && !dup {
		s.enqueue(m)
	}
	return seq, dup, err
}

// enqueue queues m for the neighbours it has to go to.
func (s *Server) enqueue(m proto.Message) {
	a := callsign.ParseAddress(m.To)
	if s.isLocal(a.At) {
		return
	}
	switch m.Kind {
	case "B":
		for _, nb := range s.neighbors {
			if !slices.Contains(m.Path, nb.Node) && nb.wantsScope(a.At) {
				s.queue(nb.Node, m.ID)
			}
		}
	case "P":
		nb := s.routeFor(a.At)
		switch {
		case nb == nil:
			s.event("no route for mail %s to %s; held", m.ID, m.To)
			s.queue("", m.ID)
		case slices.Contains(m.Path, nb.Node):
			s.event("mail %s to %s would loop back to %s; held", m.ID, m.To, nb.Node)
			s.queue("", m.ID)
		default:
			s.queue(nb.Node, m.ID)
		}
	}
}

func (s *Server) queue(node, id string) {
	if err := s.store.Enqueue(node, id); err != nil {
		s.log.Error("forward queue", "node", node, "id", id, "err", err)
	}
}

// rerouteHeld moves held mail to a neighbour once a route exists (after a
// configuration change).
func (s *Server) rerouteHeld() {
	held, err := s.store.Queued("", 1000)
	if err != nil {
		return
	}
	for _, m := range held {
		nb := s.routeFor(callsign.ParseAddress(m.To).At)
		if nb == nil || slices.Contains(m.Path, nb.Node) {
			continue
		}
		s.store.Dequeue("", []string{m.ID})
		s.queue(nb.Node, m.ID)
	}
}

// offer builds the offer for nb from its queue.
func (s *Server) offer(nb *neighbor) (proto.Offer, map[string]proto.Message, error) {
	msgs, err := s.store.Queued(nb.Node, offerLimit)
	if err != nil {
		return proto.Offer{}, nil, err
	}
	var o proto.Offer
	byID := make(map[string]proto.Message, len(msgs))
	for _, sm := range msgs {
		m := sm.Message
		m.Seq = 0
		byID[m.ID] = m
		o.Items = append(o.Items, proto.OfferItem{ID: m.ID, Kind: m.Kind, To: m.To, Size: len(m.Body)})
	}
	return o, byID, nil
}

// wants picks the offered messages we lack. Ones that are too large are
// refused right away, without transferring them.
func (s *Server) wants(o proto.Offer) proto.Want {
	var w proto.Want
	for _, it := range o.Items {
		if !idRe.MatchString(it.ID) || s.store.HasMessage(it.ID) || s.store.IsRejected(it.ID) {
			continue
		}
		if s.cfg.MaxMessageBytes > 0 && it.Size > s.cfg.MaxMessageBytes {
			s.store.Reject(it.ID, "too large")
			continue
		}
		w.IDs = append(w.IDs, it.ID)
	}
	return w
}

// confirm drops offered messages the other side did not ask for: it has
// them already.
func (s *Server) confirm(nb *neighbor, offered map[string]proto.Message, w proto.Want) {
	var done []string
	for id := range offered {
		if !slices.Contains(w.IDs, id) {
			done = append(done, id)
		}
	}
	s.store.Dequeue(nb.Node, done)
}

// receive stores messages from nb that we asked for.
func (s *Server) receive(nb *neighbor, msgs []proto.Message, asked proto.Want) (proto.PutResult, int) {
	var res proto.PutResult
	stored := 0
	now := time.Now()
	for _, m := range msgs {
		if !slices.Contains(asked.IDs, m.ID) {
			continue
		}
		nm, err := s.acceptForwarded(nb.Node, m, now)
		if err != nil {
			s.store.Reject(m.ID, err.Error())
			s.event("refused %s from %s: %v", m.ID, nb.Node, err)
			res.Rejected = append(res.Rejected, proto.Rejection{ID: m.ID, Reason: err.Error()})
			continue
		}
		if _, dup, err := s.storeMessage(nm); err != nil {
			res.Rejected = append(res.Rejected, proto.Rejection{ID: m.ID, Reason: "database error"})
			continue
		} else if !dup {
			stored++
		}
		res.Accepted = append(res.Accepted, m.ID)
	}
	return res, stored
}

// selected returns the offered messages the other side wants.
func selected(offered map[string]proto.Message, w proto.Want) []proto.Message {
	var out []proto.Message
	for _, id := range w.IDs {
		if m, ok := offered[id]; ok {
			out = append(out, m)
		}
	}
	return out
}

// writeMessages sends msgs in MESSAGES frames of bounded size.
func writeMessages(w *bufio.Writer, msgs []proto.Message) error {
	for len(msgs) > 0 {
		n, size := 0, 0
		for n < len(msgs) && (n == 0 || size+len(msgs[n].Body) <= frameBudget) {
			size += len(msgs[n].Body)
			n++
		}
		if err := proto.WriteFrame(w, proto.TMessages, proto.Messages{Messages: msgs[:n]}); err != nil {
			return err
		}
		msgs = msgs[n:]
	}
	return nil
}

// batch is what one side sent up to its END or BYE.
type batch struct {
	welcome *proto.Welcome
	offer   proto.Offer
	want    proto.Want
	msgs    []proto.Message
	result  proto.PutResult
	end     proto.Type
}

func readBatch(c transport.Conn, r *bufio.Reader) (batch, error) {
	var b batch
	for {
		setDeadline(c, idleTimeout)
		f, err := proto.ReadFrame(r)
		if err != nil {
			return b, err
		}
		switch f.Type {
		case proto.TWelcome:
			b.welcome = &proto.Welcome{}
			err = f.Decode(b.welcome)
		case proto.TOffer:
			var o proto.Offer
			err = f.Decode(&o)
			b.offer.Items = append(b.offer.Items, o.Items...)
		case proto.TWant:
			var w proto.Want
			err = f.Decode(&w)
			b.want.IDs = append(b.want.IDs, w.IDs...)
		case proto.TMessages:
			var m proto.Messages
			err = f.Decode(&m)
			b.msgs = append(b.msgs, m.Messages...)
		case proto.TPutResult:
			var p proto.PutResult
			err = f.Decode(&p)
			b.result.Accepted = append(b.result.Accepted, p.Accepted...)
			b.result.Rejected = append(b.result.Rejected, p.Rejected...)
		case proto.TError:
			var e proto.Error
			f.Decode(&e)
			return b, &e
		case proto.TEnd, proto.TBye:
			b.end = f.Type
			return b, nil
		default:
			return b, fmt.Errorf("unexpected %s frame", f.Type)
		}
		if err != nil {
			return b, fmt.Errorf("bad %s frame: %w", f.Type, err)
		}
	}
}

// serveForward answers a neighbour's forwarding session. HELLO has been
// read; h.Node names the neighbour.
func (s *Server) serveForward(c transport.Conn, sess *Session, r *bufio.Reader, w *bufio.Writer, nonce string, h proto.Hello, call string) {
	fail := func(e *proto.Error) {
		proto.WriteFrame(w, proto.TError, e)
		w.Flush()
	}
	nb := s.neighbor(h.Node)
	if nb == nil {
		s.event("forward login from unknown node %s (%s) via %s", h.Node, call, c.Kind())
		fail(&proto.Error{Code: proto.ErrAuth, Text: "node " + h.Node + " is not a neighbour of " + s.cfg.Node})
		return
	}
	if callsign.Base(nb.Call) != call {
		fail(&proto.Error{Code: proto.ErrForbidden, Text: "neighbour " + nb.Node + " uses callsign " + nb.Call})
		return
	}
	if !identity.Verify(nb.peer, s.cfg.Node, call, nonce, h.Sig) {
		s.event("forward login from %s failed: wrong link password", nb.Node)
		fail(&proto.Error{Code: proto.ErrAuth, Text: "wrong link password"})
		return
	}
	s.setSessionMode(sess, "forward")
	s.setSessionCall(sess, nb.Node)
	s.setStatus(nb, func(st *NeighborStatus) { st.Running = true; st.LastAttempt = time.Now() })
	res, err := s.answerForward(c, r, w, nb)
	s.finish(nb, "from", res, err)
}

func (s *Server) answerForward(c transport.Conn, r *bufio.Reader, w *bufio.Writer, nb *neighbor) (ForwardResult, error) {
	var res ForwardResult
	proto.WriteFrame(w, proto.TWelcome, proto.Welcome{Call: callsign.Base(s.cfg.Callsign), Node: s.cfg.Node})

	in, err := readBatch(c, r)
	if err != nil {
		return res, err
	}
	want := s.wants(in.offer)
	offer, offered, err := s.offer(nb)
	if err != nil {
		proto.WriteFrame(w, proto.TError, proto.Error{Code: proto.ErrServer, Text: "database error"})
		w.Flush()
		return res, err
	}
	res.Backlog = len(offer.Items) == offerLimit
	if len(want.IDs) == 0 && len(offer.Items) == 0 {
		proto.WriteFrame(w, proto.TBye, nil)
		return res, w.Flush()
	}
	proto.WriteFrame(w, proto.TWant, want)
	proto.WriteFrame(w, proto.TOffer, offer)
	proto.WriteFrame(w, proto.TEnd, nil)
	if err := w.Flush(); err != nil {
		return res, err
	}

	in, err = readBatch(c, r)
	if err != nil {
		return res, err
	}
	s.confirm(nb, offered, in.want)
	put, stored := s.receive(nb, in.msgs, want)
	res.Received = stored
	if in.end == proto.TBye {
		return res, nil
	}
	if len(in.msgs) > 0 {
		proto.WriteFrame(w, proto.TPutResult, put)
	}
	out := selected(offered, in.want)
	if err := writeMessages(w, out); err != nil {
		return res, err
	}
	res.Sent = len(out)
	proto.WriteFrame(w, proto.TBye, nil)
	return res, w.Flush()
}

// ForwardNow calls neighbour node and exchanges messages.
func (s *Server) ForwardNow(ctx context.Context, node string) (ForwardResult, error) {
	nb := s.neighbor(node)
	if nb == nil {
		return ForwardResult{}, fmt.Errorf("%s is not a neighbour", node)
	}
	if !nb.run.TryLock() {
		return ForwardResult{}, ErrBusy
	}
	defer nb.run.Unlock()
	s.setStatus(nb, func(st *NeighborStatus) { st.Running = true; st.LastAttempt = time.Now() })
	res, err := s.callForward(ctx, nb)
	s.finish(nb, "with", res, err)
	return res, err
}

func (s *Server) callForward(ctx context.Context, nb *neighbor) (ForwardResult, error) {
	var res ForwardResult
	kind, target := nb.ConnectKind()
	if kind == "" {
		return res, fmt.Errorf("no connect address for %s; it calls us", nb.Node)
	}
	d := s.dialer(kind)
	if d == nil {
		return res, fmt.Errorf("%s is not enabled on this server", kind)
	}
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	conn, err := d.Dial(ctx, target)
	if err != nil {
		return res, fmt.Errorf("connect: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	lr := &proto.LineReader{R: r, Max: 1024}

	setDeadline(conn, handshakeTimeout)
	var banner proto.Banner
	for {
		line, err := lr.ReadLine()
		if err != nil {
			return res, fmt.Errorf("reading banner: %w", err)
		}
		if b, ok := proto.ParseBanner(line); ok {
			banner = b
			break
		}
	}
	if !strings.EqualFold(banner.Node, nb.Node) {
		return res, fmt.Errorf("reached node %s instead of %s", banner.Node, nb.Node)
	}

	offer, offered, err := s.offer(nb)
	if err != nil {
		return res, err
	}
	res.Backlog = len(offer.Items) == offerLimit
	call := callsign.Base(s.cfg.Callsign)
	w.WriteString(proto.Magic + "\r")
	proto.WriteFrame(w, proto.THello, proto.Hello{
		Call:    call,
		Version: proto.Version,
		Sig:     identity.Sign(nb.key, banner.Node, call, banner.Nonce),
		Client:  "hambbs-server/" + buildinfo.String(),
		Node:    s.cfg.Node,
	})
	proto.WriteFrame(w, proto.TOffer, offer)
	proto.WriteFrame(w, proto.TEnd, nil)
	if err := w.Flush(); err != nil {
		return res, err
	}

	for {
		line, err := lr.ReadLine()
		if err != nil {
			return res, fmt.Errorf("waiting for %s: %w", nb.Node, err)
		}
		if strings.Contains(line, proto.ReadyLine) {
			break
		}
	}
	in, err := readBatch(conn, r)
	if err != nil {
		return res, err
	}
	if in.welcome == nil {
		return res, errors.New("no WELCOME from " + nb.Node)
	}
	s.confirm(nb, offered, in.want)
	if in.end == proto.TBye {
		return res, nil // nothing to do either way
	}
	want := s.wants(in.offer)
	out := selected(offered, in.want)
	if len(out) == 0 && len(want.IDs) == 0 {
		proto.WriteFrame(w, proto.TBye, nil)
		return res, w.Flush()
	}
	if err := writeMessages(w, out); err != nil {
		return res, err
	}
	proto.WriteFrame(w, proto.TWant, want)
	proto.WriteFrame(w, proto.TEnd, nil)
	if err := w.Flush(); err != nil {
		return res, err
	}

	in, err = readBatch(conn, r)
	if err != nil {
		return res, err
	}
	done := append(in.result.Accepted, make([]string, 0, len(in.result.Rejected))...)
	for _, rj := range in.result.Rejected {
		s.event("%s refused %s: %s", nb.Node, rj.ID, rj.Reason)
		done = append(done, rj.ID)
	}
	s.store.Dequeue(nb.Node, done)
	res.Sent = len(in.result.Accepted)
	_, res.Received = s.receive(nb, in.msgs, want)
	if in.end != proto.TBye {
		proto.WriteFrame(w, proto.TBye, nil)
		w.Flush()
	}
	return res, nil
}

func (s *Server) setStatus(nb *neighbor, f func(*NeighborStatus)) {
	s.mu.Lock()
	f(&nb.status)
	s.mu.Unlock()
}

// finish records the outcome of a session in either direction.
func (s *Server) finish(nb *neighbor, dir string, res ForwardResult, err error) {
	s.setStatus(nb, func(st *NeighborStatus) {
		st.Running = false
		st.Sent += res.Sent
		st.Received += res.Received
		if err != nil {
			st.LastError = err.Error()
		} else {
			st.LastOK, st.LastError = time.Now(), ""
		}
	})
	if err != nil {
		s.event("forwarding %s %s failed: %v", dir, nb.Node, err)
		return
	}
	s.event("forwarded %s %s: %d sent, %d received", dir, nb.Node, res.Sent, res.Received)
}

// runForwarding calls neighbours on their schedule until ctx is done.
func (s *Server) runForwarding(ctx context.Context) {
	s.rerouteHeld()
	for _, nb := range s.neighbors {
		if nb.Connect != "" && nb.Interval > 0 {
			go s.schedule(ctx, nb)
		}
	}
}

func (s *Server) schedule(ctx context.Context, nb *neighbor) {
	// The first call comes soon after start. Jitter keeps two nodes with
	// the same interval from calling each other at the same moment.
	delay := 20*time.Second + rand.N(40*time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		res, err := s.ForwardNow(ctx, nb.Node)
		interval := time.Duration(nb.Interval) * time.Minute
		delay = interval + rand.N(interval/10+1)
		if err == nil && res.Backlog {
			delay = retryBacklog
		}
	}
}

// ForwardInBackground starts a session with node now (admin UI).
func (s *Server) ForwardInBackground(node string) error {
	nb := s.neighbor(node)
	if nb == nil {
		return fmt.Errorf("%s is not a neighbour", node)
	}
	if nb.Connect == "" {
		return fmt.Errorf("%s has no connect address; it calls us", nb.Node)
	}
	go s.ForwardNow(s.runCtx(), nb.Node)
	return nil
}
