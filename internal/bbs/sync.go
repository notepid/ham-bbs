package bbs

import (
	"bufio"
	"slices"
	"strings"
	"time"

	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/proto"
	"github.com/notepid/ham-bbs/internal/transport"
)

const maxGet = 200

// serveSync runs a sync-protocol session. The client normally sends its
// whole batch (HELLO, PUT, SYNC, GET, BYE) in one transmission and the
// server answers in one transmission, so a session costs a single link
// turnaround after the banner. Responses are buffered and only flushed at
// END/BYE for the same reason.
func (s *Server) serveSync(c transport.Conn, sess *Session, r *bufio.Reader, w *bufio.Writer, nonce string) {
	w.WriteString(proto.ReadyLine + "\r")

	fail := func(e *proto.Error) {
		proto.WriteFrame(w, proto.TError, e)
		w.Flush()
	}

	setDeadline(c, handshakeTimeout)
	f, err := proto.ReadFrame(r)
	if err != nil {
		return
	}
	var h proto.Hello
	if f.Type != proto.THello || f.Decode(&h) != nil {
		fail(&proto.Error{Code: proto.ErrBadRequest, Text: "expected HELLO"})
		return
	}
	if h.Version != proto.Version {
		fail(&proto.Error{Code: proto.ErrVersion, Text: "unsupported protocol version"})
		return
	}
	if !callsign.Valid(h.Call) {
		fail(&proto.Error{Code: proto.ErrBadRequest, Text: "invalid callsign"})
		return
	}
	call := callsign.Base(h.Call)
	if lc := c.LinkCall(); lc != "" && callsign.Base(lc) != call {
		fail(&proto.Error{Code: proto.ErrForbidden, Text: "callsign does not match link address " + lc})
		return
	}
	if h.Node != "" {
		s.serveForward(c, sess, r, w, nonce, h, call)
		return
	}
	registered, perr := s.login(h, call, nonce)
	if perr != nil {
		s.event("sync login failed for %s via %s: %s", call, c.Kind(), perr.Text)
		fail(perr)
		return
	}
	s.setSessionCall(sess, call)
	s.store.TouchUser(call)
	proto.WriteFrame(w, proto.TWelcome, proto.Welcome{Call: call, Node: s.cfg.Node, Registered: registered, MOTD: s.cfg.MOTD})

	stats := struct{ put, sent int }{}
	defer func() {
		s.event("%s synced via %s: %d up, %d down", call, c.Kind(), stats.put, stats.sent)
	}()

	for {
		setDeadline(c, idleTimeout)
		f, err := proto.ReadFrame(r)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.TPut:
			var p proto.Put
			if f.Decode(&p) != nil {
				fail(&proto.Error{Code: proto.ErrBadRequest, Text: "bad PUT"})
				return
			}
			res := s.handlePut(call, p)
			stats.put += len(res.Accepted)
			proto.WriteFrame(w, proto.TPutResult, res)
		case proto.TSync:
			var q proto.Sync
			if f.Decode(&q) != nil {
				fail(&proto.Error{Code: proto.ErrBadRequest, Text: "bad SYNC"})
				return
			}
			hdrs, full, err := s.handleSync(call, q)
			if err != nil {
				fail(&proto.Error{Code: proto.ErrServer, Text: "database error"})
				return
			}
			stats.sent += len(full.Messages)
			proto.WriteFrame(w, proto.THeaders, hdrs)
			if len(full.Messages) > 0 {
				proto.WriteFrame(w, proto.TMessages, full)
			}
		case proto.TGet:
			var g proto.Get
			if f.Decode(&g) != nil {
				fail(&proto.Error{Code: proto.ErrBadRequest, Text: "bad GET"})
				return
			}
			msgs := s.handleGet(call, g)
			stats.sent += len(msgs.Messages)
			proto.WriteFrame(w, proto.TMessages, msgs)
		case proto.TEnd:
			proto.WriteFrame(w, proto.TEnd, nil)
			if w.Flush() != nil {
				return
			}
		case proto.TBye:
			proto.WriteFrame(w, proto.TBye, nil)
			w.Flush()
			return
		default:
			fail(&proto.Error{Code: proto.ErrBadRequest, Text: "unexpected " + f.Type.String()})
			return
		}
	}
}

func (s *Server) handlePut(call string, p proto.Put) proto.PutResult {
	var res proto.PutResult
	now := time.Now()
	for _, m := range p.Messages {
		nm, err := s.accept(call, m, now)
		if err != nil {
			res.Rejected = append(res.Rejected, proto.Rejection{ID: m.ID, Reason: err.Error()})
			continue
		}
		if _, dup, err := s.storeMessage(nm); err != nil {
			res.Rejected = append(res.Rejected, proto.Rejection{ID: m.ID, Reason: "database error"})
			continue
		} else if !dup {
			s.event("%s posted %s to %s: %s", call, kindName(nm.Kind), nm.To, nm.Subject)
		}
		res.Accepted = append(res.Accepted, m.ID)
	}
	return res
}

func (s *Server) handleSync(call string, q proto.Sync) (proto.Headers, proto.Messages, error) {
	limit := s.cfg.SyncLimit
	if limit <= 0 {
		limit = 200
	}
	if q.Limit > 0 && q.Limit < limit {
		limit = q.Limit
	}
	areas := normalizeAll(q.Areas)
	fetch := normalizeAll(q.FetchAreas)
	fetchAll := slices.Contains(fetch, "*")

	msgs, cursor, more, err := s.store.SyncQuery(call, q.Since, q.After, areas, limit)
	if err != nil {
		return proto.Headers{}, proto.Messages{}, err
	}
	hdrs := proto.Headers{Cursor: cursor, More: more}
	var full proto.Messages
	for _, sm := range msgs {
		m := sm.Message
		area := callsign.ParseAddress(m.To).Name
		if (m.Kind == "P" && q.FetchPersonal) || (m.Kind == "B" && (fetchAll || slices.Contains(fetch, area))) {
			full.Messages = append(full.Messages, m)
			continue
		}
		m.Size = len(m.Body)
		m.Body = ""
		hdrs.Headers = append(hdrs.Headers, m)
	}
	return hdrs, full, nil
}

func (s *Server) handleGet(call string, g proto.Get) proto.Messages {
	var out proto.Messages
	for i, id := range g.IDs {
		if i >= maxGet {
			break
		}
		m, err := s.store.MessageByID(id)
		if err != nil || !s.canSee(call, m.Message) {
			continue
		}
		out.Messages = append(out.Messages, m.Message)
	}
	return out
}

func normalizeAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if a = strings.ToUpper(strings.TrimSpace(a)); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func kindName(k string) string {
	if k == "B" {
		return "bulletin"
	}
	return "mail"
}
