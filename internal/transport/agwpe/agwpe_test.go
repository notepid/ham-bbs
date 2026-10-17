package agwpe_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/notepid/ham-bbs/internal/transport/agwpe"
	"github.com/notepid/ham-bbs/internal/transport/agwpe/agwpetest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestFrameRoundTrip(t *testing.T) {
	in := agwpe.Frame{Port: 1, Kind: 'D', PID: 0xF0, From: "LA1AAA-7", To: "LA1BBS", Data: []byte("hello\r")}
	var buf bytes.Buffer
	if err := agwpe.WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 36+6 {
		t.Fatalf("encoded %d bytes", buf.Len())
	}
	out, err := agwpe.ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Port != 1 || out.Kind != 'D' || out.PID != 0xF0 || out.From != "LA1AAA-7" || out.To != "LA1BBS" || string(out.Data) != "hello\r" {
		t.Fatalf("got %+v", out)
	}
}

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		in   string
		dest string
		via  []string
		bad  bool
	}{
		{in: "la1bbs", dest: "LA1BBS"},
		{in: "LA1BBS-2", dest: "LA1BBS-2"},
		{in: "LA1BBS via LA1DIG", dest: "LA1BBS", via: []string{"LA1DIG"}},
		{in: "LA1BBS v LA1DIG,LA2DIG-1", dest: "LA1BBS", via: []string{"LA1DIG", "LA2DIG-1"}},
		{in: "LA1BBS VIA LA1DIG, LA2DIG", dest: "LA1BBS", via: []string{"LA1DIG", "LA2DIG"}},
		{in: "", bad: true},
		{in: "bbs.example.org:6300", bad: true},
		{in: "LA1BBS LA1DIG", bad: true},
		{in: "LA1BBS-16", bad: true},
		{in: "LA1BBSX1", bad: true}, // 7 characters: too long for AX.25
		{in: "LA1BBS via A,B,C,D,E,F,G,H", bad: true},
	} {
		dest, via, err := agwpe.ParseTarget(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: expected error", tc.in)
			}
			continue
		}
		if err != nil || dest != tc.dest || !slices.Equal(via, tc.via) {
			t.Errorf("%q: got %q %q %v", tc.in, dest, via, err)
		}
	}
}

func setup(t *testing.T, opt agwpetest.Options) (*agwpetest.Net, *agwpetest.Station, *agwpe.Listener) {
	t.Helper()
	n := agwpetest.NewNet(opt)
	t.Cleanup(n.Close)
	a, err := n.AddStation("A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := n.AddStation("B")
	if err != nil {
		t.Fatal(err)
	}
	l, err := agwpe.Listen(agwpe.Config{Addr: b.Addr(), MyCalls: []string{"LA1BBS"}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return n, a, l
}

// accept runs Accept in the background (it also brings the listener up).
func accept(l *agwpe.Listener) chan io.ReadWriteCloser {
	ch := make(chan io.ReadWriteCloser, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			ch <- c
		}
		close(ch)
	}()
	return ch
}

func dial(t *testing.T, addr, target string) io.ReadWriteCloser {
	t.Helper()
	d := agwpe.Dialer{Config: agwpe.Config{Addr: addr, MyCalls: []string{"LA1AAA"}, Log: quiet}, Timeout: 5 * time.Second}
	var c io.ReadWriteCloser
	var err error
	for i := 0; i < 20; i++ { // the listener may still be registering
		if c, err = d.Dial(context.Background(), target); err == nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(err)
	return nil
}

// TestSessionDeliversEverything checks that Close waits until the TNC has
// sent every frame: Direwolf drops queued data when asked to disconnect.
func TestSessionDeliversEverything(t *testing.T) {
	n, a, l := setup(t, agwpetest.Options{BytesPerSecond: 20000, Latency: 20 * time.Millisecond})
	incoming := accept(l)
	c := dial(t, a.Addr(), "LA1BBS")
	srv := <-incoming
	if srv == nil {
		t.Fatal("no incoming session")
	}
	conn := srv.(interface{ LinkCall() string })
	if conn.LinkCall() != "LA1AAA" {
		t.Fatalf("link call %q", conn.LinkCall())
	}

	payload := bytes.Repeat([]byte("0123456789abcdef"), 1000) // 16 kB, 63 frames
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(c)
		done <- b
	}()
	if _, err := srv.Write(payload); err != nil {
		t.Fatal(err)
	}
	srv.Close() // must not cut off the tail
	select {
	case got := <-done:
		if !bytes.Equal(got, payload) {
			t.Fatalf("got %d bytes, want %d", len(got), len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client did not see the end of the session")
	}
	c.Close()
	if !slices.Contains(n.Log(), "d LA1BBS>LA1AAA") {
		t.Fatalf("log: %v", n.Log())
	}
}

func TestDialNoAnswer(t *testing.T) {
	_, a, _ := setup(t, agwpetest.Options{})
	d := agwpe.Dialer{Config: agwpe.Config{Addr: a.Addr(), MyCalls: []string{"LA1AAA"}, Log: quiet}}
	start := time.Now()
	_, err := d.Dial(context.Background(), "LA9XYZ")
	if err == nil || !strings.Contains(err.Error(), "RETRYOUT") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("dial waited for its timeout instead of the TNC's answer")
	}
}

func TestDialViaDigipeaters(t *testing.T) {
	n, a, l := setup(t, agwpetest.Options{})
	incoming := accept(l)
	c := dial(t, a.Addr(), "LA1BBS via LA1DIG,LA2DIG")
	defer c.Close()
	if s := <-incoming; s != nil {
		defer s.Close()
	}
	if r := c.(interface{ Remote() string }).Remote(); r != "LA1BBS via LA1DIG,LA2DIG (ax25)" {
		t.Fatalf("remote %q", r)
	}
	if !slices.Contains(n.Log(), "C LA1AAA>LA1BBS via LA1DIG,LA2DIG") {
		t.Fatalf("log: %v", n.Log())
	}
}

// TestConcurrentSessions runs two sessions over one TNC connection.
func TestConcurrentSessions(t *testing.T) {
	_, a, l := setup(t, agwpetest.Options{BytesPerSecond: 5000})
	echo := func(c io.ReadWriteCloser) {
		buf := make([]byte, 100)
		n, _ := c.Read(buf)
		c.Write(bytes.ToUpper(buf[:n]))
		c.Close()
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go echo(c)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	results := make(chan string, 2)
	for _, call := range []string{"LA1AAA", "LA2BBB-5"} {
		go func() {
			d := agwpe.Dialer{Config: agwpe.Config{Addr: a.Addr(), MyCalls: []string{call}, Log: quiet}, Timeout: 5 * time.Second}
			c, err := d.Dial(context.Background(), "LA1BBS")
			if err != nil {
				results <- err.Error()
				return
			}
			c.Write([]byte("hello from " + call))
			b, _ := io.ReadAll(c)
			c.Close()
			results <- string(b)
		}()
	}
	got := []string{<-results, <-results}
	slices.Sort(got)
	if !slices.Equal(got, []string{"HELLO FROM LA1AAA", "HELLO FROM LA2BBB-5"}) {
		t.Fatalf("got %q", got)
	}
}

func TestReadDeadline(t *testing.T) {
	_, a, l := setup(t, agwpetest.Options{})
	incoming := accept(l)
	c := dial(t, a.Addr(), "LA1BBS")
	defer c.Close()
	srv := <-incoming
	defer srv.Close()
	srv.(interface{ SetReadDeadline(time.Time) error }).SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := srv.Read(make([]byte, 10)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// TestTNCRestart: the listener reconnects after the TNC goes away, and
// sessions in progress end.
func TestTNCRestart(t *testing.T) {
	n := agwpetest.NewNet(agwpetest.Options{})
	defer n.Close()
	a, _ := n.AddStation("A")
	b, _ := n.AddStation("B")
	l, err := agwpe.Listen(agwpe.Config{Addr: b.Addr(), MyCalls: []string{"LA1BBS"}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	incoming := accept(l)
	c := dial(t, a.Addr(), "LA1BBS")
	srv := <-incoming
	b.Close()
	if _, err := io.ReadAll(srv); err != nil {
		t.Fatalf("read after TNC loss: %v", err)
	}
	srv.Close()
	c.Close()

	// The listener keeps trying; a TNC that comes back at another address
	// is not reachable, so just check Accept is still waiting, then close.
	got := accept(l)
	select {
	case s := <-got:
		t.Fatalf("unexpected session %v", s)
	case <-time.After(300 * time.Millisecond):
	}
	l.Close()
	if s := <-got; s != nil {
		t.Fatal("Accept returned a session after Close")
	}
}

// TestNoOutstandingReports: a TNC without 'Y' support still works.
func TestNoOutstandingReports(t *testing.T) {
	_, a, l := setup(t, agwpetest.Options{NoOutstanding: true})
	incoming := accept(l)
	c := dial(t, a.Addr(), "LA1BBS")
	srv := <-incoming
	go func() {
		srv.Write([]byte(strings.Repeat("x", 3000)))
		time.Sleep(200 * time.Millisecond)
		srv.Close()
	}()
	b, _ := io.ReadAll(c)
	c.Close()
	if len(b) != 3000 {
		t.Fatalf("got %d bytes", len(b))
	}
}
