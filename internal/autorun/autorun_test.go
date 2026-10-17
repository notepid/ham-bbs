package autorun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the helper program.
func TestMain(m *testing.M) {
	switch os.Getenv("AUTORUN_HELPER") {
	case "serve":
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		fmt.Println("ready")
		fmt.Fprintln(os.Stderr, "to stderr")
		<-sig
		fmt.Println("bye")
		os.Exit(0)
	case "crash":
		fmt.Println("boom")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func helper(t *testing.T, mode string) string {
	t.Setenv("AUTORUN_HELPER", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return `"` + exe + `" -test.run=none`
}

func waitFor(t *testing.T, p *Process, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := p.Status(); ok(st) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %+v", what, p.Status())
	return Status{}
}

func TestSplit(t *testing.T) {
	for in, want := range map[string][]string{
		`vardefm modem --ptt cm108`:                             {"vardefm", "modem", "--ptt", "cm108"},
		`  vardefm   modem  `:                                   {"vardefm", "modem"},
		`vardefm modem --audio-in "USB Audio" --ptt 'rts:COM3'`: {"vardefm", "modem", "--audio-in", "USB Audio", "--ptt", "rts:COM3"},
		`"C:\Program Files\direwolf\direwolf.exe" -t 0`:         {`C:\Program Files\direwolf\direwolf.exe`, "-t", "0"},
		`direwolf -c "" -t 0`:                                   {"direwolf", "-c", "", "-t", "0"},
	} {
		got, err := Split(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Split(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", `vardefm "modem`} {
		if _, err := Split(in); err == nil {
			t.Errorf("Split(%q) did not fail", in)
		}
	}
}

func TestStartStop(t *testing.T) {
	p := New("test", quiet)
	cmd := helper(t, "serve")
	if err := p.Start(cmd); err != nil {
		t.Fatal(err)
	}
	st := waitFor(t, p, "output", func(s Status) bool {
		return s.Running && slices.Contains(s.Output, "ready") && slices.Contains(s.Output, "to stderr")
	})
	if err := p.Start(cmd); err != nil || p.Status().PID != st.PID {
		t.Fatalf("starting the same command again restarted it: %v", err)
	}

	start := time.Now()
	p.Stop()
	if time.Since(start) > stopTimeout+2*time.Second {
		t.Fatalf("Stop took %s", time.Since(start))
	}
	st = p.Status()
	if st.Running || st.LastExit != "stopped" {
		t.Fatalf("after Stop: %+v", st)
	}
	// Windows can only interrupt programs that share a console with us.
	if runtime.GOOS != "windows" && !slices.Contains(st.Output, "bye") {
		t.Fatalf("program was not stopped gracefully: %q", st.Output)
	}
}

func TestRestart(t *testing.T) {
	p := New("test", quiet)
	defer p.Stop()
	if err := p.Start(helper(t, "crash")); err != nil {
		t.Fatal(err)
	}
	st := waitFor(t, p, "a restart", func(s Status) bool { return s.Restarts >= 2 })
	if !strings.Contains(st.LastExit, "exit status 3") || !slices.Contains(st.Output, "boom") {
		t.Fatalf("status: %+v", st)
	}
}

func TestNotFound(t *testing.T) {
	p := New("test", quiet)
	defer p.Stop()
	p.Start("no-such-program-hambbs --help")
	st := waitFor(t, p, "the error", func(s Status) bool { return s.LastExit != "" })
	if !strings.Contains(st.LastExit, "not found") {
		t.Fatalf("status: %+v", st)
	}
}

func TestWaitPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	p := New("test", quiet)
	start := time.Now()
	p.WaitPort(context.Background(), addr) // nothing configured: no waiting
	if time.Since(start) > time.Second {
		t.Fatal("WaitPort waited without a program")
	}

	if err := p.Start(helper(t, "serve")); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	go func() {
		time.Sleep(700 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err == nil {
			time.Sleep(5 * time.Second)
			l.Close()
		}
	}()
	start = time.Now()
	p.WaitPort(context.Background(), addr)
	if d := time.Since(start); d < 500*time.Millisecond || d > 5*time.Second {
		t.Fatalf("WaitPort returned after %s", d)
	}
}
