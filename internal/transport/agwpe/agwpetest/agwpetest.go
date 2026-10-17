// Package agwpetest provides mock AGWPE TNCs (Direwolf stand-ins) that
// share a simulated radio channel, for testing AX.25 hosts without radios.
// It implements the part of the AGWPE interface HamBBS uses and mirrors
// Direwolf's behaviour where it matters: connect replies, outstanding
// frame counts ('Y'), and a disconnect that discards queued data. It does
// not model collisions, errors or retries.
package agwpetest

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/notepid/ham-bbs/internal/transport/agwpe"
)

// Options shape the simulated channel.
type Options struct {
	// BytesPerSecond limits throughput per link direction; 0 = unlimited.
	BytesPerSecond int
	// Latency is added to connects, disconnects and the start of each burst.
	Latency time.Duration
	// NoAnswer is how long a connect to an unknown station takes to fail
	// (Direwolf: FRACK × RETRY). Default 200 ms.
	NoAnswer time.Duration
	// NoOutstanding makes the TNC ignore 'Y' queries, like some old TNCs.
	NoOutstanding bool
}

// Net is a simulated radio channel shared by mock TNCs.
type Net struct {
	opt Options

	mu       sync.Mutex
	stations []*Station
	links    []*link
	log      []string
}

// NewNet creates an empty channel.
func NewNet(opt Options) *Net {
	if opt.NoAnswer == 0 {
		opt.NoAnswer = 200 * time.Millisecond
	}
	return &Net{opt: opt}
}

// Station is one mock TNC with an AGWPE port.
type Station struct {
	net  *Net
	name string
	l    net.Listener

	mu      sync.Mutex
	clients map[*client]bool
}

type client struct {
	st      *Station
	conn    net.Conn
	writeMu sync.Mutex
	regs    map[string]bool // guarded by Net.mu
}

// end is one side of a link.
type end struct {
	cl          *client
	port        byte
	own, remote string
	queue       [][]byte // frames waiting to go over the air
	pending     int      // queued or in flight
	wake        chan struct{}
}

type link struct {
	a, b *end
	via  []string
	quit chan struct{}
	down bool
}

// AddStation starts a mock TNC on a free local port.
func (n *Net) AddStation(name string) (*Station, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Station{net: n, name: name, l: l, clients: map[*client]bool{}}
	n.mu.Lock()
	n.stations = append(n.stations, s)
	n.mu.Unlock()
	go s.serve()
	return s, nil
}

// Close stops every station.
func (n *Net) Close() {
	n.mu.Lock()
	sts := append([]*Station(nil), n.stations...)
	n.mu.Unlock()
	for _, s := range sts {
		s.Close()
	}
}

// Log returns connect and disconnect requests seen on the channel, e.g.
// "C LA1AAA>LA1BBS via LA1DIG".
func (n *Net) Log() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.log...)
}

// Links returns the number of links up.
func (n *Net) Links() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.links)
}

// Addr is the station's AGWPE address.
func (s *Station) Addr() string { return s.l.Addr().String() }

// Close stops the TNC: the listener and every client connection.
func (s *Station) Close() {
	s.l.Close()
	s.mu.Lock()
	cls := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		cls = append(cls, c)
	}
	s.mu.Unlock()
	for _, c := range cls {
		c.conn.Close()
	}
}

func (s *Station) serve() {
	for {
		conn, err := s.l.Accept()
		if err != nil {
			return
		}
		c := &client{st: s, conn: conn, regs: map[string]bool{}}
		s.mu.Lock()
		s.clients[c] = true
		s.mu.Unlock()
		go c.run()
	}
}

func (c *client) send(f agwpe.Frame) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	agwpe.WriteFrame(c.conn, f)
}

func (c *client) run() {
	n := c.st.net
	defer func() {
		c.conn.Close()
		c.st.mu.Lock()
		delete(c.st.clients, c)
		c.st.mu.Unlock()
		// The TNC drops the client's links, as Direwolf does.
		n.mu.Lock()
		for _, l := range n.links {
			if l.a.cl == c || l.b.cl == c {
				n.teardown(l)
			}
		}
		n.mu.Unlock()
	}()
	for {
		f, err := agwpe.ReadFrame(c.conn)
		if err != nil {
			return
		}
		c.handle(f)
	}
}

func (c *client) handle(f agwpe.Frame) {
	n := c.st.net
	switch f.Kind {
	case 'R':
		d := make([]byte, 8)
		binary.LittleEndian.PutUint32(d, 2005)
		binary.LittleEndian.PutUint32(d[4:], 127)
		c.send(agwpe.Frame{Kind: 'R', Data: d})
	case 'G':
		c.send(agwpe.Frame{Kind: 'G', Data: []byte("1;Port1 mock soundcard mono;\x00")})
	case 'X':
		n.mu.Lock()
		ok := byte(1)
		for o := range c.st.clients {
			if o != c && o.regs[f.From] {
				ok = 0
			}
		}
		if ok == 1 {
			c.regs[f.From] = true
		}
		n.mu.Unlock()
		c.send(agwpe.Frame{Port: f.Port, Kind: 'X', From: f.From, Data: []byte{ok}})
	case 'x':
		n.mu.Lock()
		delete(c.regs, f.From)
		n.mu.Unlock()
	case 'C', 'v':
		via := []string(nil)
		if f.Kind == 'v' {
			via = agwpe.DecodeVia(f.Data)
		}
		n.connect(c, f.Port, f.From, f.To, via)
	case 'D':
		n.mu.Lock()
		if e := n.find(c, f.From, f.To); e != nil {
			e.queue = append(e.queue, f.Data)
			e.pending++
			select {
			case e.wake <- struct{}{}:
			default:
			}
		}
		n.mu.Unlock()
	case 'Y':
		if n.opt.NoOutstanding {
			return
		}
		n.mu.Lock()
		count := 0
		if e := n.find(c, f.From, f.To); e != nil {
			count = e.pending
		}
		n.mu.Unlock()
		d := make([]byte, 4)
		binary.LittleEndian.PutUint32(d, uint32(count))
		c.send(agwpe.Frame{Port: f.Port, Kind: 'Y', From: f.From, To: f.To, Data: d})
	case 'd':
		n.mu.Lock()
		n.log = append(n.log, fmt.Sprintf("d %s>%s", f.From, f.To))
		var l *link
		if e := n.find(c, f.From, f.To); e != nil {
			l = e.link(n)
			e.queue, e.pending = nil, 0 // Direwolf discards what is not yet sent
		}
		n.mu.Unlock()
		if l != nil {
			time.AfterFunc(n.opt.Latency, func() {
				n.mu.Lock()
				n.teardown(l)
				n.mu.Unlock()
			})
		}
	}
}

// find returns the end of a link owned by c. Caller holds n.mu.
func (n *Net) find(c *client, own, remote string) *end {
	for _, l := range n.links {
		for _, e := range []*end{l.a, l.b} {
			if e.cl == c && e.own == own && e.remote == remote {
				return e
			}
		}
	}
	return nil
}

func (e *end) link(n *Net) *link {
	for _, l := range n.links {
		if l.a == e || l.b == e {
			return l
		}
	}
	return nil
}

func (n *Net) connect(c *client, port byte, own, dest string, via []string) {
	n.mu.Lock()
	entry := fmt.Sprintf("C %s>%s", own, dest)
	if len(via) > 0 {
		entry += " via " + strings.Join(via, ",")
	}
	n.log = append(n.log, entry)
	var target *client
	for _, s := range n.stations {
		if s == c.st {
			continue // a TNC does not hear itself
		}
		s.mu.Lock()
		for o := range s.clients {
			if o.regs[dest] {
				target = o
			}
		}
		s.mu.Unlock()
	}
	n.mu.Unlock()

	if target == nil {
		time.AfterFunc(n.opt.NoAnswer, func() {
			c.send(agwpe.Frame{Port: port, Kind: 'd', From: dest, To: own,
				Data: []byte("*** DISCONNECTED RETRYOUT With " + dest + "\r\x00")})
		})
		return
	}
	time.AfterFunc(n.opt.Latency, func() {
		l := &link{
			a:    &end{cl: c, port: port, own: own, remote: dest, wake: make(chan struct{}, 1)},
			b:    &end{cl: target, port: port, own: dest, remote: own, wake: make(chan struct{}, 1)},
			via:  via,
			quit: make(chan struct{}),
		}
		n.mu.Lock()
		n.links = append(n.links, l)
		n.mu.Unlock()
		target.send(agwpe.Frame{Port: port, Kind: 'C', From: own, To: dest,
			Data: []byte("*** CONNECTED To Station " + own + "\r\x00")})
		c.send(agwpe.Frame{Port: port, Kind: 'C', From: dest, To: own,
			Data: []byte("*** CONNECTED With Station " + dest + "\r\x00")})
		go n.pump(l, l.a, l.b)
		go n.pump(l, l.b, l.a)
	})
}

// pump moves frames from one end to the other at the channel's speed.
func (n *Net) pump(l *link, from, to *end) {
	for {
		n.mu.Lock()
		var data []byte
		if len(from.queue) > 0 && !l.down {
			data = from.queue[0]
			from.queue = from.queue[1:]
		}
		n.mu.Unlock()
		if data == nil {
			select {
			case <-from.wake:
				// Start of a burst: key up, wait for the other side.
				if !sleep(l, n.opt.Latency) {
					return
				}
				continue
			case <-l.quit:
				return
			}
		}
		if bps := n.opt.BytesPerSecond; bps > 0 {
			if !sleep(l, time.Duration(len(data))*time.Second/time.Duration(bps)) {
				return
			}
		}
		n.mu.Lock()
		if l.down {
			n.mu.Unlock()
			return
		}
		from.pending--
		n.mu.Unlock()
		to.cl.send(agwpe.Frame{Port: to.port, Kind: 'D', PID: 0xF0, From: to.remote, To: to.own, Data: data})
	}
}

func sleep(l *link, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-l.quit:
		return false
	}
}

// teardown ends a link and tells both clients. Caller holds n.mu.
func (n *Net) teardown(l *link) {
	if l.down {
		return
	}
	l.down = true
	close(l.quit)
	for i, x := range n.links {
		if x == l {
			n.links = append(n.links[:i], n.links[i+1:]...)
			break
		}
	}
	for _, e := range []*end{l.a, l.b} {
		go e.cl.send(agwpe.Frame{Port: e.port, Kind: 'd', From: e.remote, To: e.own,
			Data: []byte("*** DISCONNECTED From Station " + e.remote + "\r\x00")})
	}
}
