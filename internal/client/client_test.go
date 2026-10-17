package client_test

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/notepid/ham-bbs/internal/bbs"
	"github.com/notepid/ham-bbs/internal/client"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport/tcp"
)

// startServer runs a BBS on a random local port.
func startServer(t *testing.T, mutate func(*config.Server)) (addr string, srv *bbs.Server) {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.Callsign, cfg.Node, cfg.Name = "LA1BBS", "LA1BBS", "Test BBS"
	cfg.MOTD = "Welcome to the test"
	if mutate != nil {
		mutate(&cfg)
	}
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Keep the server's warnings and errors; show them if the test fails.
	logs := &lockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	srv = bbs.New(cfg, st, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	l, err := tcp.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go srv.Serve(l)
	return strings.TrimPrefix(l.String(), "tcp "), srv
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newClient(t *testing.T, addr, call, password string) *client.Client {
	t.Helper()
	cfg := config.DefaultClient()
	cfg.SetPath(filepath.Join(t.TempDir(), "client.toml"))
	cfg.Callsign, cfg.Address = call, addr
	cfg.Key = client.KeyFromPassword(call, password)
	cfg.InitialDays = 0
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return client.New(cfg, st)
}

func mustSync(t *testing.T, c *client.Client) *client.Report {
	t.Helper()
	rep := c.Sync(context.Background())
	if rep.Err != nil {
		t.Fatalf("sync: %v", rep.Err)
	}
	return rep
}

func TestSyncEndToEnd(t *testing.T) {
	addr, srv := startServer(t, nil)
	alice := newClient(t, addr, "LA1AAA", "alicepw")
	bob := newClient(t, addr, "LA2BBB", "bobpw")

	// First sync registers.
	rep := mustSync(t, alice)
	if !rep.Registered || rep.MOTD != "Welcome to the test" {
		t.Fatalf("first sync: %+v", rep)
	}
	mustSync(t, bob)

	// Alice sends Bob mail and posts a bulletin; one invalid message.
	if _, err := alice.Compose("P", "la2bbb", "Hello Bob", "How are you?\nde LA1AAA"); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Compose("B", "TECH", "Antenna tips", strings.Repeat("Use a balun. ", 50)); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Compose("P", "LA3CCC@OTHERNODE", "Far away", "x"); err != nil {
		t.Fatal(err)
	}
	rep = mustSync(t, alice)
	if rep.Uploaded != 2 || len(rep.Rejected) != 1 {
		t.Fatalf("upload: %+v", rep)
	}
	if st, _ := srv.Store().Stats(); st.Personal != 1 || st.Bulletins != 1 {
		t.Fatalf("server stats %+v", st)
	}

	// Bob gets the personal message in full, the bulletin as header only.
	rep = mustSync(t, bob)
	if rep.NewBodies != 1 || rep.NewHeaders != 1 {
		t.Fatalf("bob sync: %+v", rep)
	}
	inbox, _ := bob.Store.ListMessages(store.ListFilter{Kind: "P"})
	if len(inbox) != 1 || inbox[0].From != "LA1AAA" || inbox[0].Body != "How are you?\nde LA1AAA" || !inbox[0].HasBody {
		t.Fatalf("bob inbox: %+v", inbox)
	}
	bulls, _ := bob.Store.ListMessages(store.ListFilter{Kind: "B"})
	if len(bulls) != 1 || bulls[0].HasBody || bulls[0].Size == 0 {
		t.Fatalf("bob bulletins: %+v", bulls)
	}

	// Nothing new on the next sync.
	if rep = mustSync(t, bob); rep.NewBodies+rep.NewHeaders != 0 {
		t.Fatalf("expected empty sync, got %+v", rep)
	}

	// Bob asks for the bulletin text.
	bob.Store.AddWant(bulls[0].ID)
	if rep = mustSync(t, bob); rep.NewBodies != 1 {
		t.Fatalf("want fetch: %+v", rep)
	}
	m, _ := bob.Store.MessageByID(bulls[0].ID)
	if !m.HasBody || !strings.HasPrefix(m.Body, "Use a balun.") {
		t.Fatalf("bulletin body not stored: %+v", m)
	}
	if w, _ := bob.Store.Wants(); len(w) != 0 {
		t.Fatalf("wants not cleared: %v", w)
	}
}

func TestWrongPassword(t *testing.T) {
	addr, _ := startServer(t, nil)
	mustSync(t, newClient(t, addr, "LA1AAA", "right"))
	rep := newClient(t, addr, "LA1AAA", "wrong").Sync(context.Background())
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "wrong password") {
		t.Fatalf("expected auth failure, got %v", rep.Err)
	}
}

func TestClosedRegistration(t *testing.T) {
	addr, _ := startServer(t, func(c *config.Server) { c.OpenRegistration = false })
	rep := newClient(t, addr, "LA1AAA", "pw").Sync(context.Background())
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "unknown callsign") {
		t.Fatalf("expected registration refusal, got %v", rep.Err)
	}
}

// TestTerminal drives a telnet-style session.
func TestTerminal(t *testing.T) {
	addr, _ := startServer(t, nil)
	alice := newClient(t, addr, "LA1AAA", "alicepw")
	mustSync(t, alice) // registers the account
	alice.Compose("B", "LOCAL", "Club meeting", "Thursday 19:00")
	mustSync(t, alice)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(conn)
	expect := func(s string) {
		t.Helper()
		var got strings.Builder
		buf := make([]byte, 1)
		for !strings.Contains(got.String(), s) {
			if _, err := r.Read(buf); err != nil {
				t.Fatalf("waiting for %q: %v; got %q", s, err, got.String())
			}
			got.Write(buf)
		}
	}
	send := func(s string) { conn.Write([]byte(s + "\r\n")) }

	expect("Callsign: ")
	send("la1aaa")
	expect("Password: ")
	send("alicepw")
	expect("LA1BBS> ")
	send("LB")
	expect("Club meeting")
	expect("LA1BBS> ")
	send("S LA2BBB")
	expect("Subject: ")
	send("Terminal test")
	expect("/EX")
	send("line one")
	send("/ex")
	expect("Stored as message #")
	expect("LA1BBS> ")
	send("LL")
	expect("Terminal test")
	send("B")
	expect("73 de LA1BBS")

	// Bob sees the terminal message via sync.
	bob := newClient(t, addr, "LA2BBB", "bobpw")
	mustSync(t, bob)
	inbox, _ := bob.Store.ListMessages(store.ListFilter{Kind: "P"})
	if len(inbox) != 1 || inbox[0].Subject != "Terminal test" || inbox[0].Body != "line one" {
		t.Fatalf("bob inbox: %+v", inbox)
	}
}
