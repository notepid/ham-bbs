package store

import (
	"time"
)

// Server-side forwarding state.
//
// fwdq holds one row per message and neighbour node it still has to go to.
// Rows for neighbour "" are personal mail waiting for a route. A row is
// removed when the neighbour confirms it has the message.
//
// rejected remembers messages we refused from a neighbour, so they are not
// asked for again when the neighbour offers them next time.

const forwardSchema = `
CREATE TABLE IF NOT EXISTS fwdq (
	neighbor TEXT NOT NULL,
	id       TEXT NOT NULL,
	added    INTEGER NOT NULL,
	PRIMARY KEY (neighbor, id)
);
CREATE TABLE IF NOT EXISTS rejected (
	id     TEXT PRIMARY KEY,
	reason TEXT NOT NULL,
	at     INTEGER NOT NULL
);
`

// rejectedKeep is how long a refused message ID is remembered.
const rejectedKeep = 90 * 24 * time.Hour

// Enqueue queues message id for neighbour node ("" = no route yet).
func (s *Store) Enqueue(node, id string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO fwdq (neighbor, id, added) VALUES (?, ?, ?)`, node, id, time.Now().Unix())
	return err
}

// Dequeue removes ids from node's queue.
func (s *Store) Dequeue(node string, ids []string) error {
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM fwdq WHERE neighbor = ? AND id = ?`, node, id); err != nil {
			return err
		}
	}
	return nil
}

// Queued returns up to limit messages queued for node, oldest first.
// Expired messages are dropped from the queue instead.
func (s *Store) Queued(node string, limit int) ([]Message, error) {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`DELETE FROM fwdq WHERE neighbor = ? AND id NOT IN
		(SELECT id FROM messages WHERE expires = 0 OR expires > ?)`, node, now); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages WHERE id IN (SELECT id FROM fwdq WHERE neighbor = ?)
		ORDER BY seq LIMIT ?`, node, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// QueueCounts returns the number of queued messages per neighbour.
func (s *Store) QueueCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT neighbor, COUNT(*) FROM fwdq GROUP BY neighbor`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var n string
		var c int
		if err := rows.Scan(&n, &c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

// HasMessage reports whether a message with this ID is stored.
func (s *Store) HasMessage(id string) bool {
	var x int
	return s.db.QueryRow(`SELECT 1 FROM messages WHERE id = ?`, id).Scan(&x) == nil
}

// Reject remembers that a message offered by a neighbour was refused.
func (s *Store) Reject(id, reason string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO rejected (id, reason, at) VALUES (?, ?, ?)`, id, reason, time.Now().Unix())
	return err
}

// IsRejected reports whether id was refused before.
func (s *Store) IsRejected(id string) bool {
	var x int
	return s.db.QueryRow(`SELECT 1 FROM rejected WHERE id = ?`, id).Scan(&x) == nil
}

// cleanForward drops queue rows of deleted messages and old rejections.
func (s *Store) cleanForward(now time.Time) {
	s.db.Exec(`DELETE FROM fwdq WHERE id NOT IN (SELECT id FROM messages)`)
	s.db.Exec(`DELETE FROM rejected WHERE at < ?`, now.Add(-rejectedKeep).Unix())
}
