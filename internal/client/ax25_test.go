package client_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/notepid/ham-bbs/internal/bbs"
	"github.com/notepid/ham-bbs/internal/client"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport/agwpe"
	"github.com/notepid/ham-bbs/internal/transport/agwpe/agwpetest"
)

// TestSyncOverAX25 runs complete sessions through mock AGWPE TNCs. The
// channel runs faster than 1200 baud packet (about 100 B/s) to keep the
// test short; the protocol does not care.
func TestSyncOverAX25(t *testing.T) {
	n := agwpetest.NewNet(agwpetest.Options{BytesPerSecond: 2000, Latency: 50 * time.Millisecond})
	defer n.Close()
	user, err := n.AddStation("user")
	if err != nil {
		t.Fatal(err)
	}
	bbsTNC, err := n.AddStation("bbs")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultServer()
	cfg.Callsign, cfg.Node = "LA1BBS", "LA1BBS"
	st, _ := store.OpenMemory()
	defer st.Close()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := bbs.New(cfg, st, quiet)
	l, err := agwpe.Listen(agwpe.Config{Addr: bbsTNC.Addr(), MyCalls: []string{"LA1BBS"}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go srv.Serve(l)
	time.Sleep(200 * time.Millisecond) // listener registers with its TNC

	radio := func(call, pw string) *client.Client {
		c := newClient(t, "LA1BBS", call, pw)
		cc := c.Config()
		cc.Transport, cc.AGWAddr = "ax25", user.Addr()
		if err := c.SetConfig(cc); err != nil {
			t.Fatal(err)
		}
		return c
	}

	alice := radio("LA1AAA", "alicepw")
	alice.Compose("P", "LA2BBB", "Over the air", "Hello via AX.25")
	alice.Compose("B", "LOCAL", "Net tonight", strings.Repeat("Net at 20:00 on the repeater. ", 40))
	rep := mustSync(t, alice)
	if !rep.Registered || rep.Uploaded != 2 {
		t.Fatalf("alice: %+v", rep)
	}
	t.Logf("alice sync: %v, %d B out, %d B in", rep.Duration.Round(time.Millisecond), rep.BytesOut, rep.BytesIn)

	// A human on a packet terminal and a sync client at the same time:
	// AX.25 carries several sessions through one TNC.
	termOut := make(chan string)
	go func() {
		d := agwpe.Dialer{Config: agwpe.Config{Addr: user.Addr(), MyCalls: []string{"LA3CCC-7"}, Log: quiet}}
		conn, err := d.Dial(context.Background(), "LA1BBS")
		if err != nil {
			termOut <- err.Error()
			return
		}
		conn.Write([]byte("LB\r"))
		time.Sleep(300 * time.Millisecond)
		conn.Write([]byte("B\r"))
		out, _ := io.ReadAll(conn)
		conn.Close()
		termOut <- string(out)
	}()

	bob := radio("LA2BBB", "bobpw")
	rep = mustSync(t, bob)
	if rep.NewBodies != 1 || rep.NewHeaders != 1 {
		t.Fatalf("bob: %+v", rep)
	}
	inbox, _ := bob.Store.ListMessages(store.ListFilter{Kind: "P"})
	if len(inbox) != 1 || inbox[0].Body != "Hello via AX.25" {
		t.Fatalf("bob inbox: %+v", inbox)
	}
	t.Logf("bob sync: %v, %d B out, %d B in", rep.Duration.Round(time.Millisecond), rep.BytesOut, rep.BytesIn)

	out := <-termOut
	for _, want := range []string{"Hello LA3CCC-7", "Net tonight", "73 de LA1BBS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("terminal output lacks %q:\n%s", want, out)
		}
	}
}
