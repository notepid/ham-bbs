package bbs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/proto"
)

const maxSubject = 80

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{6,32}$`)

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
		if a.At != "" && a.At != s.cfg.Node {
			return m, fmt.Errorf("forwarding to %s is not supported yet", a.At)
		}
		a.At = s.cfg.Node
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

	m.Subject = cleanLine(m.Subject)
	if m.Subject == "" {
		return m, errors.New("subject is empty")
	}
	if utf8.RuneCountInString(m.Subject) > maxSubject {
		m.Subject = string([]rune(m.Subject)[:maxSubject])
	}

	m.Body = strings.ReplaceAll(m.Body, "\r\n", "\n")
	m.Body = strings.ReplaceAll(m.Body, "\r", "\n")
	m.Body = strings.TrimRight(m.Body, "\n \t")
	if !utf8.ValidString(m.Body) {
		return m, errors.New("body is not valid UTF-8")
	}
	if s.cfg.MaxMessageBytes > 0 && len(m.Body) > s.cfg.MaxMessageBytes {
		return m, fmt.Errorf("body too large (%d > %d bytes)", len(m.Body), s.cfg.MaxMessageBytes)
	}

	if m.Created <= 0 || m.Created > now.Add(24*time.Hour).Unix() {
		m.Created = now.Unix()
	}
	m.Origin = s.cfg.Node
	m.Path = []string{s.cfg.Node}
	return m, nil
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
func canSee(call string, m proto.Message) bool {
	if m.Kind == "B" {
		return true
	}
	return callsign.ParseAddress(m.To).Name == call || m.From == call
}
