package vara_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"hambbs/internal/transport"
	"hambbs/internal/transport/vara"
	"hambbs/internal/transport/vara/varatest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func pair(t *testing.T, opt varatest.Options) *varatest.Pair {
	t.Helper()
	p, err := varatest.NewPair(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func listen(t *testing.T, st *varatest.Station, calls ...string) *vara.Listener {
	t.Helper()
	l, err := vara.Listen(vara.Config{Addr: st.Addr(), MyCalls: calls, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func dial(t *testing.T, st *varatest.Station, from, to string) (transport.Conn, error) {
	t.Helper()
	d := vara.Dialer{Config: vara.Config{Addr: st.Addr(), MyCalls: []string{from}, Log: quiet}, Timeout: 10 * time.Second}
	return d.Dial(context.Background(), to)
}

// waitListening dials until the listener has registered with its modem.
func dialRetry(t *testing.T, st *varatest.Station, from, to string) transport.Conn {
	t.Helper()
	for i := 0; i < 50; i++ {
		c, err := dial(t, st, from, to)
		if err == nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("could not connect")
	return nil
}

func TestSessionTransfersBinaryBothWays(t *testing.T) {
	p := pair(t, varatest.Options{BytesPerSecond: 20000, Latency: 20 * time.Millisecond})
	l := listen(t, p.B, "LA1BBS")

	accepted := make(chan transport.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	cli := dialRetry(t, p.A, "LA2ABC", "LA1BBS")
	var srv transport.Conn
	select {
	case srv = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("no incoming session")
	}

	if srv.LinkCall() != "LA2ABC" || cli.LinkCall() != "LA1BBS" || srv.Kind() != "vara" {
		t.Fatalf("calls: srv sees %q, client sees %q", srv.LinkCall(), cli.LinkCall())
	}

	// Every byte value, both directions.
	payload := make([]byte, 3000)
	for i := range payload {
		payload[i] = byte(i)
	}
	go srv.Write(payload)
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(cli, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("server->client: %v", err)
	}
	go cli.Write(payload)
	if _, err := io.ReadFull(srv, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("client->server: %v", err)
	}

	// The server writes a tail and closes at once; Close must wait for
	// the modem to deliver it, and the client must see it before EOF.
	tail := []byte(strings.Repeat("73 ", 2000))
	srv.Write(tail)
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	rest, err := io.ReadAll(cli)
	if err != nil || !bytes.Equal(rest, tail) {
		t.Fatalf("tail: got %d of %d bytes, err %v", len(rest), len(tail), err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server Close hung")
	}
	cli.Close()

	if cmds := strings.Join(p.B.Commands(), "|"); !strings.Contains(cmds, "LISTEN ON") || !strings.Contains(cmds, "DISCONNECT") {
		t.Fatalf("server modem commands: %s", cmds)
	}
}

func TestNextSessionAfterDisconnect(t *testing.T) {
	p := pair(t, varatest.Options{})
	l := listen(t, p.B, "LA1BBS")
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello " + c.LinkCall()))
			c.Close()
		}
	}()
	for _, call := range []string{"LA2AAA", "LA3BBB"} {
		c := dialRetry(t, p.A, call, "LA1BBS")
		b, err := io.ReadAll(c)
		c.Close()
		if err != nil || string(b) != "hello "+call {
			t.Fatalf("%s: got %q, %v", call, b, err)
		}
	}
}

func TestDialUnknownStationFails(t *testing.T) {
	p := pair(t, varatest.Options{})
	listen(t, p.B, "LA1BBS")
	time.Sleep(100 * time.Millisecond) // let the listener register
	start := time.Now()
	if _, err := dial(t, p.A, "LA2ABC", "LA9XXX"); err == nil {
		t.Fatal("connect to an unknown callsign succeeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("failed connect took too long")
	}
}

func TestReadDeadline(t *testing.T) {
	p := pair(t, varatest.Options{})
	l := listen(t, p.B, "LA1BBS")
	go func() {
		if c, err := l.Accept(); err == nil {
			time.Sleep(2 * time.Second)
			c.Close()
		}
	}()
	c := dialRetry(t, p.A, "LA2ABC", "LA1BBS")
	defer c.Close()
	c.(transport.Deadliner).SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := c.Read(make([]byte, 10)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}
