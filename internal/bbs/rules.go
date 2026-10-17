package bbs

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/proto"
)

const (
	maxSubject = 80
	maxPath    = 30 // nodes a forwarded message may have passed
)

var (
	idRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{6,32}$`)
	nodeRe = regexp.MustCompile(`^[A-Z0-9_.#-]{1,40}$`)
)

// accept validates and normalizes a message submitted by user `from`.
func (s *Server) accept(from string, m proto.Message, now time.Time) (proto.Message, error) {
	if !idRe.MatchString(m.ID) {
		return m, errors.New("bad message ID")
	}
	m.From = from
	m.Seq = 0
	m.Size = 0

	switch m.Kind {
	case "P":
		a, err := callsign.ParsePersonal(m.To)
		if err != nil {
			return m, err
		}
		if s.isLocal(a.At) {
			a.At = s.cfg.Node
		} else if s.routeFor(a.At) == nil {
			return m, fmt.Errorf("no route to %s: this BBS does not forward there", a.At)
		}
		m.To = a.String()
		m.Expires = 0
	case "B":
		a, err := callsign.ParseBulletin(m.To)
		if err != nil {
			return m, err
		}
		m.To = a.String()
		m.Expires = 0
		if s.cfg.BulletinDays > 0 {
			m.Expires = now.Add(time.Duration(s.cfg.BulletinDays) * 24 * time.Hour).Unix()
		}
	default:
		return m, fmt.Errorf("unknown kind %q", m.Kind)
	}

	if err := s.checkContent(&m, now); err != nil {
		return m, err
	}
	m.Origin = s.cfg.Node
	m.Path = []string{s.cfg.Node}
	return m, nil
}

// acceptForwarded validates a message received from neighbour node from.
// Unlike accept, it keeps author, dates and origin, and extends the path.
func (s *Server) acceptForwarded(from string, m proto.Message, now time.Time) (proto.Message, error) {
	if !idRe.MatchString(m.ID) {
		return m, errors.New("bad message ID")
	}
	m.Seq = 0
	m.Size = 0
	if !callsign.Valid(m.From) {
		return m, fmt.Errorf("invalid sender %q", m.From)
	}
	m.From = callsign.Base(m.From)

	switch m.Kind {
	case "P":
		a, err := callsign.ParsePersonal(m.To)
		if err != nil {
			return m, err
		}
		if a.At == "" || s.isLocal(a.At) {
			a.At = s.cfg.Node // e.g. LA1BBS.NOR: deliver here under the plain name
		}
		m.To = a.String()
		m.Expires = 0
	case "B":
		a, err := callsign.ParseBulletin(m.To)
		if err != nil {
			return m, err
		}
		m.To = a.String()
		if m.Expires > 0 && m.Expires <= now.Unix() {
			return m, errors.New("expired")
		}
	default:
		return m, fmt.Errorf("unknown kind %q", m.Kind)
	}
	if err := s.checkContent(&m, now); err != nil {
		return m, err
	}

	m.Origin = strings.ToUpper(strings.TrimSpace(m.Origin))
	if m.Origin == "" {
		m.Origin = from
	}
	if !nodeRe.MatchString(m.Origin) {
		return m, fmt.Errorf("invalid origin %q", m.Origin)
	}
	p := make([]string, 0, len(m.Path)+2)
	for _, n := range m.Path {
		n = strings.ToUpper(strings.TrimSpace(n))
		if !nodeRe.MatchString(n) {
			return m, fmt.Errorf("invalid path entry %q", n)
		}
		p = append(p, n)
	}
	if slices.Contains(p, s.cfg.Node) {
		return m, fmt.Errorf("loop: already passed %s", s.cfg.Node)
	}
	if !slices.Contains(p, from) {
		p = append(p, from)
	}
	if len(p) >= maxPath {
		return m, errors.New("path too long")
	}
	m.Path = append(p, s.cfg.Node)
	return m, nil
}

// checkContent normalizes subject and body and checks size and dates.
func (s *Server) checkContent(m *proto.Message, now time.Time) error {
	m.Subject = cleanLine(m.Subject)
	if m.Subject == "" {
		return errors.New("subject is empty")
	}
	if utf8.RuneCountInString(m.Subject) > maxSubject {
		m.Subject = string([]rune(m.Subject)[:maxSubject])
	}

	m.Body = strings.ReplaceAll(m.Body, "\r\n", "\n")
	m.Body = strings.ReplaceAll(m.Body, "\r", "\n")
	m.Body = strings.TrimRight(m.Body, "\n \t")
	if !utf8.ValidString(m.Body) {
		return errors.New("body is not valid UTF-8")
	}
	if s.cfg.MaxMessageBytes > 0 && len(m.Body) > s.cfg.MaxMessageBytes {
		return fmt.Errorf("body too large (%d > %d bytes)", len(m.Body), s.cfg.MaxMessageBytes)
	}

	if m.Created <= 0 || m.Created > now.Add(24*time.Hour).Unix() {
		m.Created = now.Unix()
	}
	return nil
}

// cleanLine trims a single-line field and drops control characters.
func cleanLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// canSee reports whether call may read m.
func (s *Server) canSee(call string, m proto.Message) bool {
	if m.Kind == "B" || m.From == call {
		return true
	}
	a := callsign.ParseAddress(m.To)
	return a.Name == call && s.isLocal(a.At)
}

// isLocal reports whether a node address (the part after @) means this
// BBS. Hierarchical addresses like LA1BBS.OSL.NOR.EU are matched on their
// first label.
func (s *Server) isLocal(at string) bool {
	return at == "" || firstLabel(at) == s.cfg.Node
}

func firstLabel(at string) string {
	l, _, _ := strings.Cut(at, ".")
	return l
}

// routeFor picks the neighbour that personal mail for node `at` goes to:
// the node itself if it is a neighbour, else the first neighbour with a
// matching route pattern, else one with the default route "*".
func (s *Server) routeFor(at string) *neighbor {
	node := firstLabel(at)
	for _, nb := range s.neighbors {
		if nb.Node == node {
			return nb
		}
	}
	for _, nb := range s.neighbors {
		for _, r := range nb.Routes {
			if r == "*" {
				continue
			}
			if ok, _ := path.Match(r, at); ok || r == node {
				return nb
			}
		}
	}
	for _, nb := range s.neighbors {
		if slices.Contains(nb.Routes, "*") {
			return nb
		}
	}
	return nil
}

// wantsScope reports whether bulletins for scope go to nb.
func (nb *neighbor) wantsScope(scope string) bool {
	for _, p := range nb.Scopes {
		if ok, _ := path.Match(p, scope); ok {
			return true
		}
	}
	return false
}
