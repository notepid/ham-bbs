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
	"github.com/notepid/ham-bbs/internal/transport/vara"
	"github.com/notepid/ham-bbs/internal/transport/vara/varatest"
)

// TestSyncOverVARA runs complete sessions through a mock VARA modem pair
// at roughly VARA FM narrow speed.
func TestSyncOverVARA(t *testing.T) {
	p, err := varatest.NewPair(varatest.Options{BytesPerSecond: 1000, Latency: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cfg := config.DefaultServer()
	cfg.Callsign, cfg.Node = "LA1BBS", "LA1BBS"
	st, _ := store.OpenMemory()
	defer st.Close()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := bbs.New(cfg, st, quiet)
	l, err := vara.Listen(vara.Config{Addr: p.B.Addr(), MyCalls: []string{"LA1BBS"}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go srv.Serve(l)
	time.Sleep(200 * time.Millisecond) // listener registers with its modem

	radio := func(call, pw string) *client.Client {
		c := newClient(t, "LA1BBS", call, pw)
		cc := c.Config()
		cc.Transport, cc.VaraAddr = "vara", p.A.Addr()
		if err := c.SetConfig(cc); err != nil {
			t.Fatal(err)
		}
		return c
	}

	alice := radio("LA1AAA", "alicepw")
	alice.Compose("P", "LA2BBB", "Over the air", "Hello via VARA")
	alice.Compose("B", "LOCAL", "Net tonight", strings.Repeat("Net at 20:00 on the repeater. ", 20))
	rep := mustSync(t, alice)
	if !rep.Registered || rep.Uploaded != 2 {
		t.Fatalf("alice: %+v", rep)
	}
	t.Logf("alice sync: %v, %d B out, %d B in", rep.Duration.Round(time.Millisecond), rep.BytesOut, rep.BytesIn)

	bob := radio("LA2BBB", "bobpw")
	rep = mustSync(t, bob)
	if rep.NewBodies != 1 || rep.NewHeaders != 1 {
		t.Fatalf("bob: %+v", rep)
	}
	inbox, _ := bob.Store.ListMessages(store.ListFilter{Kind: "P"})
	if len(inbox) != 1 || inbox[0].Body != "Hello via VARA" {
		t.Fatalf("bob inbox: %+v", inbox)
	}
	t.Logf("bob sync: %v, %d B out, %d B in", rep.Duration.Round(time.Millisecond), rep.BytesOut, rep.BytesIn)

	// A human with a plain VARA terminal is identified by the link callsign.
	d := vara.Dialer{Config: vara.Config{Addr: p.A.Addr(), MyCalls: []string{"LA3CCC"}, Log: quiet}}
	conn, err := d.Dial(context.Background(), "LA1BBS")
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("LB\rB\r"))
	out, _ := io.ReadAll(conn)
	conn.Close()
	for _, want := range []string{"Hello LA3CCC", "Net tonight", "73 de LA1BBS"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("terminal output lacks %q:\n%s", want, out)
		}
	}
}
