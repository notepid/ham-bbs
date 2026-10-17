// Package autorun starts a helper program, such as a modem (VardeFM) or a
// TNC (Direwolf), together with HamBBS. It restarts the program when it
// stops and shuts it down cleanly, so it can release the radio's PTT.
package autorun

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	firstBackoff = 2 * time.Second
	maxBackoff   = time.Minute
	stopTimeout  = 5 * time.Second // after the interrupt, before killing
	keepLines    = 30
)

// Process supervises one helper program. The zero value is not usable; use New.
type Process struct {
	name string
	log  *slog.Logger

	mu      sync.Mutex
	command string
	cancel  context.CancelFunc
	done    chan struct{}
	st      Status
	lines   []string
}

// Status describes the program for the UIs.
type Status struct {
	Name     string
	Command  string
	Running  bool
	PID      int
	Since    time.Time // when it was started, or when it stopped
	LastExit string    // why it stopped last time
	Restarts int
	Output   []string // last lines it printed
}

// New returns a stopped Process. name is used in log lines, e.g. "modem".
func New(name string, log *slog.Logger) *Process {
	return &Process{name: name, log: log}
}

// Start runs command (a command line, see Split), replacing what was
// running before. An empty command stops the program. Starting the
// command that is already running does nothing.
func (p *Process) Start(command string) error {
	command = strings.TrimSpace(command)
	p.mu.Lock()
	same := command == p.command && p.done != nil
	p.mu.Unlock()
	if same {
		return nil
	}
	p.Stop()
	if command == "" {
		return nil
	}
	args, err := Split(command)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	p.command, p.cancel, p.done = command, cancel, done
	p.st = Status{Command: command}
	p.lines = nil
	p.mu.Unlock()
	go func() {
		defer close(done)
		p.loop(ctx, args)
	}()
	return nil
}

// Stop shuts the program down and waits for it to exit.
func (p *Process) Stop() {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.command, p.cancel, p.done = "", nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Status returns a snapshot.
func (p *Process) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.st
	st.Name = p.name
	st.Output = append([]string(nil), p.lines...)
	return st
}

// WaitPort waits until addr accepts TCP connections, if the program was
// started less than a minute ago: a modem needs a moment before its port
// is open. It returns at once if nothing runs or the port is open.
func (p *Process) WaitPort(ctx context.Context, addr string) {
	st := p.Status()
	if st.Command == "" {
		return
	}
	deadline := st.Since.Add(time.Minute)
	if st.Since.IsZero() {
		deadline = time.Now().Add(time.Minute)
	}
	for time.Now().Before(deadline) {
		var d net.Dialer
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			c.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (p *Process) loop(ctx context.Context, args []string) {
	backoff := firstBackoff
	for {
		started := time.Now()
		err := p.runOnce(ctx, args)
		if ctx.Err() != nil {
			p.setStopped("stopped")
			return
		}
		why := "exited"
		if err != nil {
			why = err.Error()
		}
		if time.Since(started) > maxBackoff {
			backoff = firstBackoff
		}
		p.log.Warn("helper program stopped; restarting", "name", p.name, "err", why, "in", backoff)
		p.mu.Lock()
		p.st.Restarts++
		p.mu.Unlock()
		p.setStopped(why)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (p *Process) runOnce(ctx context.Context, args []string) error {
	path, err := Resolve(args[0])
	if err != nil {
		return err
	}
	cmd := exec.Command(path, args[1:]...)
	setup(cmd)
	out, w := io.Pipe()
	cmd.Stdout, cmd.Stderr = w, w // one pipe, so lines stay in order
	cmd.WaitDelay = time.Second   // don't wait for children holding the pipe
	if err := cmd.Start(); err != nil {
		return err
	}
	attach(cmd.Process)
	p.mu.Lock()
	p.st.Running, p.st.PID, p.st.Since = true, cmd.Process.Pid, time.Now()
	p.mu.Unlock()
	p.log.Info("helper program started", "name", p.name, "pid", cmd.Process.Pid, "command", strings.Join(args, " "))

	copied := make(chan struct{})
	go func() {
		defer close(copied)
		p.copyOutput(out)
	}()
	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if err := interrupt(cmd.Process); err != nil {
				cmd.Process.Kill()
				return
			}
			select {
			case <-exited:
			case <-time.After(stopTimeout):
				p.log.Warn("helper program did not stop; killing it", "name", p.name)
				cmd.Process.Kill()
			}
		case <-exited:
		}
	}()
	err = cmd.Wait()
	close(exited)
	w.Close()
	<-copied
	return err
}

func (p *Process) copyOutput(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		p.log.Info(p.name + ": " + line)
		p.mu.Lock()
		p.lines = append(p.lines, line)
		if len(p.lines) > keepLines {
			p.lines = p.lines[len(p.lines)-keepLines:]
		}
		p.mu.Unlock()
	}
	io.Copy(io.Discard, r)
}

func (p *Process) setStopped(why string) {
	p.mu.Lock()
	p.st.Running, p.st.PID, p.st.Since, p.st.LastExit = false, 0, time.Now(), why
	p.mu.Unlock()
}

// Split splits a command line into words. Double or single quotes group
// words with spaces; backslashes are kept as they are, so Windows paths
// work: "C:\Program Files\direwolf\direwolf.exe" -t 0
func Split(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		quote rune
		in    bool
	)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("missing closing %c in %q", quote, s)
	}
	if in {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return nil, errors.New("empty command")
	}
	return words, nil
}

// Resolve finds a program. A bare name like "vardefm" is looked for next
// to the HamBBS program first (release archives include VardeFM there),
// then on the PATH.
func Resolve(name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		return name, nil
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands := []string{filepath.Join(dir, name)}
		if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
			cands = []string{filepath.Join(dir, name+".exe")}
		}
		for _, c := range cands {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
				return c, nil
			}
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found next to HamBBS or on the PATH", name)
	}
	return path, nil
}
