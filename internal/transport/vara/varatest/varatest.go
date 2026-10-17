// Package varatest provides a pair of mock VARA modems connected to each
// other, for testing VARA hosts without radios. It implements the subset
// of the VARA TCP host interface HamBBS uses (see docs/vara.md) and models
// a link with a fixed bitrate and latency. It does not model errors,
// retries or half-duplex turnarounds: that is what a real modem (or
// VardeFM's sim-pair) is for.
package varatest

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Options shape the simulated link.
type Options struct {
	// BytesPerSecond limits throughput in each direction; 0 = unlimited.
	BytesPerSecond int
	// Latency is added to connect, disconnect and each data burst.
	Latency time.Duration
}

// Pair is two connected mock modems.
type Pair struct {
	A, B *Station
}

// NewPair starts two mock modems on free local ports.
func NewPair(opt Options) (*Pair, error) {
	a, err := newStation("A", opt)
	if err != nil {
		return nil, err
	}
	b, err := newStation("B", opt)
	if err != nil {
		a.Close()
		return nil, err
	}
	a.peer, b.peer = b, a
	return &Pair{A: a, B: b}, nil
}

// Close stops both modems.
func (p *Pair) Close() {
	p.A.Close()
	p.B.Close()
}

// Station is one mock modem.
type Station struct {
	name  string
	opt   Options
	peer  *Station
	cmdL  net.Listener
	dataL net.Listener

	mu        sync.Mutex
	cmd       net.Conn
	data      net.Conn
	mycalls   []string
	listening bool
	connected bool
	gen       int // link generation, shared by both stations of a link
	txq       []byte
	wake      chan struct{}
	quit      chan struct{}

	// Log records every command received, for assertions.
	logMu sync.Mutex
	log   []string
}

func newStation(name string, opt Options) (*Station, error) {
	// The data port must be the command port + 1.
	for i := 0; i < 50; i++ {
		cmdL, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := cmdL.Addr().(*net.TCPAddr).Port
		dataL, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+1))
		if err != nil {
			cmdL.Close()
			continue
		}
		s := &Station{name: name, opt: opt, cmdL: cmdL, dataL: dataL, wake: make(chan struct{}, 1), quit: make(chan struct{})}
		go s.acceptCmd()
		go s.acceptData()
		go s.pump()
		return s, nil
	}
	return nil, fmt.Errorf("varatest: no free port pair")
}

// Addr is the command port address to give the host.
func (s *Station) Addr() string { return s.cmdL.Addr().String() }

// Commands returns the commands received so far.
func (s *Station) Commands() []string {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	return slices.Clone(s.log)
}

// Close stops the station.
func (s *Station) Close() {
	select {
	case <-s.quit:
		return
	default:
	}
	close(s.quit)
	s.cmdL.Close()
	s.dataL.Close()
	s.mu.Lock()
	if s.cmd != nil {
		s.cmd.Close()
	}
	if s.data != nil {
		s.data.Close()
	}
	s.mu.Unlock()
}

func (s *Station) send(line string) {
	s.mu.Lock()
	c := s.cmd
	s.mu.Unlock()
	if c != nil {
		c.Write([]byte(line + "\r"))
	}
}

func (s *Station) acceptCmd() {
	for {
		c, err := s.cmdL.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.cmd != nil {
			s.cmd.Close()
		}
		s.cmd = c
		s.mu.Unlock()
		go s.readCmd(c)
	}
}

func (s *Station) readCmd(c net.Conn) {
	sc := bufio.NewScanner(c)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, b := range data {
			if b == '\r' || b == '\n' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			s.logMu.Lock()
			s.log = append(s.log, line)
			s.logMu.Unlock()
			s.handle(line)
		}
	}
	// Host went away: drop the link and stop listening, like a real modem
	// whose host application closed.
	s.mu.Lock()
	owner := s.cmd == c
	if owner {
		s.listening = false
	}
	s.mu.Unlock()
	if owner {
		s.teardown(false, s.linkGen())
	}
}

func (s *Station) handle(line string) {
	f := strings.Fields(strings.ToUpper(line))
	switch f[0] {
	case "MYCALL":
		if len(f) < 2 {
			s.send("WRONG")
			return
		}
		s.mu.Lock()
		s.mycalls = f[1:]
		s.mu.Unlock()
		s.send("OK")
	case "LISTEN":
		s.mu.Lock()
		s.listening = len(f) > 1 && f[1] == "ON"
		s.mu.Unlock()
		s.send("OK")
	case "VERSION":
		s.send("OK")
		s.send("VERSION HamBBS mock VARA")
	case "CONNECT":
		if len(f) < 3 {
			s.send("WRONG")
			return
		}
		s.send("OK")
		go s.connect(f[1], f[2])
	case "DISCONNECT":
		s.send("OK")
		go s.teardown(true, s.linkGen())
	case "ABORT":
		s.send("OK")
		go s.teardown(false, s.linkGen())
	default:
		s.send("WRONG")
	}
}

func (s *Station) connect(src, dst string) {
	p := s.peer
	time.Sleep(s.opt.Latency)
	p.mu.Lock()
	ok := p.listening && !p.connected && slices.Contains(p.mycalls, dst)
	gen := p.gen + 1
	if ok {
		p.connected = true
		p.gen = gen
	}
	p.mu.Unlock()
	if !ok {
		time.Sleep(s.opt.Latency)
		s.send("DISCONNECTED")
		return
	}
	s.mu.Lock()
	s.connected = true
	s.gen = gen
	s.txq = nil
	s.mu.Unlock()
	p.send("PENDING")
	ev := fmt.Sprintf("CONNECTED %s %s", src, dst)
	p.send(ev)
	s.send(ev)
}

func (s *Station) linkGen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// teardown ends link generation gen on both sides; graceful waits until
// our queued data has been delivered. A request for an earlier link that
// is already gone is ignored, as a real modem ignores a stale DISCONNECT.
func (s *Station) teardown(graceful bool, gen int) {
	if graceful {
		for {
			s.mu.Lock()
			n := len(s.txq)
			s.mu.Unlock()
			if n == 0 {
				break
			}
			select {
			case <-s.quit:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		time.Sleep(s.opt.Latency)
	}
	for _, st := range []*Station{s, s.peer} {
		st.mu.Lock()
		was := st.connected && st.gen == gen
		if !was {
			st.mu.Unlock()
			continue
		}
		st.connected = false
		st.txq = nil
		st.mu.Unlock()
		if was {
			st.send("DISCONNECTED")
		}
	}
}

func (s *Station) acceptData() {
	for {
		c, err := s.dataL.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.data != nil {
			s.data.Close()
		}
		s.data = c
		s.mu.Unlock()
		go s.readData(c)
	}
}

func (s *Station) readData(c net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			s.mu.Lock()
			if s.connected {
				s.txq = append(s.txq, buf[:n]...)
			}
			q := len(s.txq)
			s.mu.Unlock()
			s.send(fmt.Sprintf("BUFFER %d", q))
			select {
			case s.wake <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

// pump moves queued data to the peer at the configured rate.
func (s *Station) pump() {
	for {
		select {
		case <-s.quit:
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if len(s.txq) == 0 || !s.connected {
				s.mu.Unlock()
				break
			}
			n := len(s.txq)
			if r := s.opt.BytesPerSecond; r > 0 && n > r/4+1 {
				n = r/4 + 1 // bursts of about 250 ms
			}
			chunk := slices.Clone(s.txq[:n])
			s.mu.Unlock()

			d := s.opt.Latency
			if r := s.opt.BytesPerSecond; r > 0 {
				d += time.Duration(n) * time.Second / time.Duration(r)
			}
			select {
			case <-s.quit:
				return
			case <-time.After(d):
			}

			s.peer.mu.Lock()
			pd, up := s.peer.data, s.peer.connected
			s.peer.mu.Unlock()
			if up && pd != nil {
				pd.Write(chunk)
			}
			s.mu.Lock()
			if len(s.txq) >= n {
				s.txq = s.txq[n:]
			}
			q := len(s.txq)
			s.mu.Unlock()
			s.send(fmt.Sprintf("BUFFER %d", q))
		}
	}
}
