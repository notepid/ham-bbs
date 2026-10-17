package store

import (
	"database/sql"
	"errors"
	"strconv"
	"time"

	"hambbs/internal/proto"
)

// Client-side mailbox operations.

// SaveFromServer stores a message received from the server. A header-only
// record is upgraded in place when the body arrives later.
func (s *Store) SaveFromServer(m proto.Message, hasBody bool) error {
	serverSeq := m.Seq
	_, dup, err := s.insertMessage(m, hasBody, serverSeq)
	if err != nil || !dup || !hasBody {
		return err
	}
	_, err = s.db.Exec(`UPDATE messages SET body = ?, size = ?, has_body = 1 WHERE id = ? AND has_body = 0`,
		m.Body, len(m.Body), m.ID)
	return err
}

// SetRead sets the local read flag.
func (s *Store) SetRead(id string, read bool) error {
	return s.exec1(`UPDATE messages SET read = ? WHERE id = ?`, read, id)
}

// UnreadCounts returns unread personal and bulletin counts.
func (s *Store) UnreadCounts() (personal, bulletins int, err error) {
	err = s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM messages WHERE kind = 'P' AND read = 0),
		(SELECT COUNT(*) FROM messages WHERE kind = 'B' AND read = 0)`).Scan(&personal, &bulletins)
	return
}

// ---- outbox ----

// OutMsg is a message composed locally, waiting for or done with upload.
type OutMsg struct {
	ID      string
	Kind    string
	To      string
	Subject string
	Body    string
	Created time.Time
	Status  string // queued, sent, rejected
	Reason  string
}

func (o OutMsg) Proto(from string) proto.Message {
	return proto.Message{ID: o.ID, Kind: o.Kind, From: from, To: o.To, Subject: o.Subject, Body: o.Body, Created: o.Created.Unix()}
}

func (s *Store) AddOutbox(o OutMsg) error {
	_, err := s.db.Exec(`INSERT INTO outbox (id, kind, to_addr, subject, body, created) VALUES (?, ?, ?, ?, ?, ?)`,
		o.ID, o.Kind, o.To, o.Subject, o.Body, o.Created.Unix())
	return err
}

func (s *Store) ListOutbox(status string) ([]OutMsg, error) {
	q := `SELECT id, kind, to_addr, subject, body, created, status, reason FROM outbox`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutMsg
	for rows.Next() {
		var o OutMsg
		var created int64
		if err := rows.Scan(&o.ID, &o.Kind, &o.To, &o.Subject, &o.Body, &created, &o.Status, &o.Reason); err != nil {
			return nil, err
		}
		o.Created = time.Unix(created, 0)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) SetOutboxStatus(id, status, reason string) error {
	return s.exec1(`UPDATE outbox SET status = ?, reason = ? WHERE id = ?`, status, reason, id)
}

func (s *Store) DeleteOutbox(id string) error {
	return s.exec1(`DELETE FROM outbox WHERE id = ?`, id)
}

// ---- wanted bodies ----

func (s *Store) AddWant(id string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO wants (id) VALUES (?)`, id)
	return err
}

func (s *Store) Wants() ([]string, error) {
	rows, err := s.db.Query(`SELECT w.id FROM wants w JOIN messages m ON m.id = w.id WHERE m.has_body = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) IsWanted(id string) bool {
	var x int
	return s.db.QueryRow(`SELECT 1 FROM wants WHERE id = ?`, id).Scan(&x) == nil
}

// CleanWants drops wants whose bodies have arrived or whose message is gone.
func (s *Store) CleanWants() error {
	_, err := s.db.Exec(`DELETE FROM wants WHERE id NOT IN (SELECT id FROM messages WHERE has_body = 0)`)
	return err
}

// ---- key/value ----

func (s *Store) Get(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) GetInt(key string) int64 {
	v, err := s.Get(key)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (s *Store) SetInt(key string, n int64) error {
	return s.Set(key, strconv.FormatInt(n, 10))
}
