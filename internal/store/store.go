// Package store persists users and messages in SQLite (pure Go driver, no
// cgo, so cross-compiling stays trivial). The same schema backs both the
// server and the client's local mailbox; each uses the parts it needs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/notepid/ham-bbs/internal/proto"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is an open database.
type Store struct {
	db *sql.DB
	// localNode is set on a server. Personal mail addressed to another
	// node is then in transit and not shown to local users.
	localNode string
}

// SetLocalNode tells the store which node it belongs to (server only).
func (s *Store) SetLocalNode(node string) { s.localNode = node }

const schema = `
CREATE TABLE IF NOT EXISTS users (
	call      TEXT PRIMARY KEY,
	pubkey    BLOB,
	name      TEXT NOT NULL DEFAULT '',
	admin     INTEGER NOT NULL DEFAULT 0,
	created   INTEGER NOT NULL,
	last_seen INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	id         TEXT NOT NULL UNIQUE,
	kind       TEXT NOT NULL,
	from_call  TEXT NOT NULL,
	to_name    TEXT NOT NULL,
	to_at      TEXT NOT NULL DEFAULT '',
	subject    TEXT NOT NULL DEFAULT '',
	body       TEXT NOT NULL DEFAULT '',
	size       INTEGER NOT NULL DEFAULT 0,
	has_body   INTEGER NOT NULL DEFAULT 1,
	created    INTEGER NOT NULL,
	expires    INTEGER NOT NULL DEFAULT 0,
	origin     TEXT NOT NULL DEFAULT '',
	path       TEXT NOT NULL DEFAULT '',
	received   INTEGER NOT NULL,
	server_seq INTEGER NOT NULL DEFAULT 0,
	read       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS messages_to ON messages(kind, to_name);
CREATE TABLE IF NOT EXISTS reads (
	call TEXT NOT NULL,
	id   TEXT NOT NULL,
	PRIMARY KEY (call, id)
);
CREATE TABLE IF NOT EXISTS outbox (
	id      TEXT PRIMARY KEY,
	kind    TEXT NOT NULL,
	to_addr TEXT NOT NULL,
	subject TEXT NOT NULL,
	body    TEXT NOT NULL,
	created INTEGER NOT NULL,
	status  TEXT NOT NULL DEFAULT 'queued',
	reason  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS wants (
	id TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Open opens (and creates or migrates) the database at path.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	if filepath.VolumeName(abs) != "" { // Windows: file:///C:/...
		u.Path = "/" + u.Path
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + forwardSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenMemory opens a private in-memory database (tests).
func OpenMemory() (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + forwardSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- users ----

// User is a registered BBS user.
type User struct {
	Call     string
	PubKey   []byte
	Name     string
	Admin    bool
	Created  time.Time
	LastSeen time.Time
}

func (s *Store) GetUser(call string) (User, error) {
	var u User
	var created, seen int64
	err := s.db.QueryRow(`SELECT call, pubkey, name, admin, created, last_seen FROM users WHERE call = ?`, call).
		Scan(&u.Call, &u.PubKey, &u.Name, &u.Admin, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.Created, u.LastSeen = unix(created), unix(seen)
	return u, err
}

func (s *Store) CreateUser(call string, pubkey []byte, name string, admin bool) error {
	_, err := s.db.Exec(`INSERT INTO users (call, pubkey, name, admin, created) VALUES (?, ?, ?, ?, ?)`,
		call, pubkey, name, admin, time.Now().Unix())
	return err
}

func (s *Store) SetUserKey(call string, pubkey []byte) error {
	return s.exec1(`UPDATE users SET pubkey = ? WHERE call = ?`, pubkey, call)
}

func (s *Store) SetUserAdmin(call string, admin bool) error {
	return s.exec1(`UPDATE users SET admin = ? WHERE call = ?`, admin, call)
}

func (s *Store) TouchUser(call string) error {
	_, err := s.db.Exec(`UPDATE users SET last_seen = ? WHERE call = ?`, time.Now().Unix(), call)
	return err
}

func (s *Store) DeleteUser(call string) error {
	return s.exec1(`DELETE FROM users WHERE call = ?`, call)
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT call, pubkey, name, admin, created, last_seen FROM users ORDER BY call`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created, seen int64
		if err := rows.Scan(&u.Call, &u.PubKey, &u.Name, &u.Admin, &created, &seen); err != nil {
			return nil, err
		}
		u.Created, u.LastSeen = unix(created), unix(seen)
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- messages ----

// Message is a stored message.
type Message struct {
	proto.Message
	HasBody   bool
	Received  time.Time
	ServerSeq int64 // client side: the server's sequence number
	Read      bool  // client side: read flag
}

const msgCols = `seq, id, kind, from_call, to_name, to_at, subject, body, size, has_body, created, expires, origin, path, received, server_seq, read`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var toName, toAt, path string
	var received int64
	err := sc.Scan(&m.Seq, &m.ID, &m.Kind, &m.From, &toName, &toAt, &m.Subject, &m.Body, &m.Size,
		&m.HasBody, &m.Created, &m.Expires, &m.Origin, &path, &received, &m.ServerSeq, &m.Read)
	if err != nil {
		return m, err
	}
	m.To = toName
	if toAt != "" {
		m.To += "@" + toAt
	}
	if path != "" {
		m.Path = strings.Split(path, ",")
	}
	m.Received = unix(received)
	return m, nil
}

func splitTo(to string) (name, at string) {
	name, at, _ = strings.Cut(to, "@")
	return
}

// AddMessage stores a message. If a message with the same ID exists it is
// left untouched and dup is true.
func (s *Store) AddMessage(m proto.Message) (seq int64, dup bool, err error) {
	return s.insertMessage(m, true, 0)
}

func (s *Store) insertMessage(m proto.Message, hasBody bool, serverSeq int64) (int64, bool, error) {
	name, at := splitTo(m.To)
	size := m.Size
	if hasBody {
		size = len(m.Body)
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages
		(id, kind, from_call, to_name, to_at, subject, body, size, has_body, created, expires, origin, path, received, server_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Kind, m.From, name, at, m.Subject, m.Body, size, hasBody, m.Created, m.Expires,
		m.Origin, strings.Join(m.Path, ","), time.Now().Unix(), serverSeq)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var seq int64
		err := s.db.QueryRow(`SELECT seq FROM messages WHERE id = ?`, m.ID).Scan(&seq)
		return seq, true, err
	}
	seq, err := res.LastInsertId()
	return seq, false, err
}

func (s *Store) MessageByID(id string) (Message, error) {
	return s.oneMessage(`SELECT `+msgCols+` FROM messages WHERE id = ?`, id)
}

func (s *Store) MessageBySeq(seq int64) (Message, error) {
	return s.oneMessage(`SELECT `+msgCols+` FROM messages WHERE seq = ?`, seq)
}

func (s *Store) oneMessage(q string, args ...any) (Message, error) {
	m, err := scanMessage(s.db.QueryRow(q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (s *Store) DeleteMessage(id string) error {
	if _, err := s.db.Exec(`DELETE FROM reads WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM fwdq WHERE id = ?`, id); err != nil {
		return err
	}
	return s.exec1(`DELETE FROM messages WHERE id = ?`, id)
}

// PurgeExpired deletes bulletins whose expiry has passed.
func (s *Store) PurgeExpired(now time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM messages WHERE expires > 0 AND expires < ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	s.db.Exec(`DELETE FROM reads WHERE id NOT IN (SELECT id FROM messages)`)
	s.cleanForward(now)
	return res.RowsAffected()
}

// SyncQuery returns messages visible to call with seq > since, oldest
// first, at most limit. cursor is the value the client should send as
// since next time.
func (s *Store) SyncQuery(call string, since, after int64, areas []string, limit int) (msgs []Message, cursor int64, more bool, err error) {
	where, args := s.visibleWhere(call, areas, false)
	q := `SELECT ` + msgCols + ` FROM messages WHERE seq > ? AND created > ? AND (expires = 0 OR expires > ?) AND ` + where +
		` ORDER BY seq LIMIT ?`
	args = append([]any{since, after, time.Now().Unix()}, args...)
	args = append(args, limit+1)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, 0, false, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	if len(msgs) > limit {
		msgs = msgs[:limit]
		return msgs, msgs[len(msgs)-1].Seq, true, nil
	}
	// Nothing more visible: jump the cursor to the newest message overall.
	cursor = since
	var max sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM messages`).Scan(&max); err != nil {
		return nil, 0, false, err
	}
	if max.Valid && max.Int64 > cursor {
		cursor = max.Int64
	}
	return msgs, cursor, false, nil
}

func (s *Store) visibleWhere(call string, areas []string, includeSent bool) (string, []any) {
	b := `(kind = 'B'`
	var args []any
	if len(areas) > 0 {
		b += ` AND to_name IN (` + placeholders(len(areas)) + `)`
		for _, a := range areas {
			args = append(args, a)
		}
	}
	b += `)`
	if call == "" {
		return b, args
	}
	mine, margs := s.addressedTo(call)
	if includeSent {
		return `((kind = 'P' AND (` + mine + ` OR from_call = ?)) OR ` + b + `)`, append(append(margs, call), args...)
	}
	return `((kind = 'P' AND ` + mine + `) OR ` + b + `)`, append(margs, args...)
}

// addressedTo matches personal mail delivered to call on this node.
func (s *Store) addressedTo(call string) (string, []any) {
	if s.localNode == "" {
		return `to_name = ?`, []any{call}
	}
	return `(to_name = ? AND to_at IN ('', ?))`, []any{call, s.localNode}
}

// ListFilter selects messages for listings.
type ListFilter struct {
	Call   string // only messages visible to this user; "" = all (admin)
	Kind   string // "P", "B" or ""
	Area   string // bulletin area
	Unread bool   // only messages Call has not read (server) / unread (client)
	Mine   bool   // only personal mail to Call (otherwise Call's sent mail is included)
	Limit  int
}

// ListMessages lists messages newest first.
func (s *Store) ListMessages(f ListFilter) ([]Message, error) {
	var conds []string
	var args []any
	if f.Call != "" {
		if f.Mine {
			w, a := s.addressedTo(f.Call)
			conds = append(conds, `kind = 'P' AND `+w)
			args = append(args, a...)
		} else {
			w, a := s.visibleWhere(f.Call, nil, true)
			conds = append(conds, w)
			args = append(args, a...)
		}
		if f.Unread {
			conds = append(conds, `id NOT IN (SELECT id FROM reads WHERE call = ?)`)
			args = append(args, f.Call)
		}
	} else if f.Unread {
		conds = append(conds, `read = 0`)
	}
	if f.Kind != "" {
		conds = append(conds, `kind = ?`)
		args = append(args, f.Kind)
	}
	if f.Area != "" {
		conds = append(conds, `kind = 'B' AND to_name = ?`)
		args = append(args, f.Area)
	}
	q := `SELECT ` + msgCols + ` FROM messages`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created DESC, seq DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
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

// MarkRead records that call has read message id (server side).
func (s *Store) MarkRead(call, id string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO reads (call, id) VALUES (?, ?)`, call, id)
	return err
}

// Area is a bulletin area with its message count.
type Area struct {
	Name  string
	Count int
	Last  time.Time
}

// Areas lists bulletin areas in use.
func (s *Store) Areas() ([]Area, error) {
	rows, err := s.db.Query(`SELECT to_name, COUNT(*), MAX(created) FROM messages WHERE kind = 'B' GROUP BY to_name ORDER BY to_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Area
	for rows.Next() {
		var a Area
		var last int64
		if err := rows.Scan(&a.Name, &a.Count, &last); err != nil {
			return nil, err
		}
		a.Last = unix(last)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Stats summarises the database.
type Stats struct {
	Users, Personal, Bulletins int
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM messages WHERE kind = 'P'),
		(SELECT COUNT(*) FROM messages WHERE kind = 'B')`).Scan(&st.Users, &st.Personal, &st.Bulletins)
	return st, err
}

// ---- helpers ----

func (s *Store) exec1(q string, args ...any) error {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func unix(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0)
}
