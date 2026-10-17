package bbs_test

import (
	"testing"
	"time"

	"github.com/notepid/ham-bbs/internal/transport"
	"github.com/notepid/ham-bbs/internal/transport/agwpe"
	"github.com/notepid/ham-bbs/internal/transport/agwpe/agwpetest"
	"github.com/notepid/ham-bbs/internal/transport/vara"
	"github.com/notepid/ham-bbs/internal/transport/vara/varatest"
)

// radioListener is a listener the server can also dial out through.
type radioListener interface {
	transport.Listener
	transport.Dialer
}

// addRadio serves l on n and uses it for outgoing calls of kind.
func (n *node) addRadio(t *testing.T, kind string, l radioListener) {
	t.Helper()
	t.Cleanup(func() { l.Close() })
	go n.srv.Serve(l)
	n.srv.SetDialer(kind, l)
}

// exchange posts a bulletin on each node and forwards in both directions
// with A calling B.
func exchange(t *testing.T, a, b *node) {
	t.Helper()
	alice := a.user(t, "LA1ALI")
	bob := b.user(t, "LA2BOB")
	alice.Compose("B", "TECH@ALL", "From A", "Hello B")
	bob.Compose("B", "TECH@ALL", "From B", "Hello A")
	sync(t, alice)
	sync(t, bob)

	var res struct{ Sent, Received int }
	for i := 0; i < 20; i++ { // the listeners may still be registering
		r, err := a.srv.ForwardNow(t.Context(), b.name)
		if err == nil {
			res.Sent, res.Received = r.Sent, r.Received
			break
		}
		if i == 19 {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if res.Sent != 1 || res.Received != 1 {
		t.Fatalf("A->B: %+v", res)
	}
	if !has(b.st, "From A") || !has(a.st, "From B") {
		t.Fatal("bulletins did not cross")
	}
}

func TestForwardOverAX25(t *testing.T) {
	net := agwpetest.NewNet(agwpetest.Options{BytesPerSecond: 2000, Latency: 20 * time.Millisecond})
	defer net.Close()
	sa, _ := net.AddStation("a")
	sb, _ := net.AddStation("b")

	a, b := listen(t, "LA1AA"), listen(t, "LA2BB")
	la := a.link(b)
	la.Connect = "ax25:LA2BB"
	a.start(t, la)
	b.start(t, b.link(a))
	for _, x := range []struct {
		n    *node
		addr string
	}{{a, sa.Addr()}, {b, sb.Addr()}} {
		l, err := agwpe.Listen(agwpe.Config{Addr: x.addr, MyCalls: []string{x.n.name}, Log: quiet})
		if err != nil {
			t.Fatal(err)
		}
		x.n.addRadio(t, "ax25", l)
	}
	exchange(t, a, b)
}

func TestForwardOverVARA(t *testing.T) {
	p, err := varatest.NewPair(varatest.Options{BytesPerSecond: 2000, Latency: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	a, b := listen(t, "LA1AA"), listen(t, "LA2BB")
	la := a.link(b)
	la.Connect = "vara:LA2BB"
	a.start(t, la)
	b.start(t, b.link(a))
	for _, x := range []struct {
		n    *node
		addr string
	}{{a, p.A.Addr()}, {b, p.B.Addr()}} {
		l, err := vara.Listen(vara.Config{Addr: x.addr, MyCalls: []string{x.n.name}, Log: quiet})
		if err != nil {
			t.Fatal(err)
		}
		x.n.addRadio(t, "vara", l)
	}
	exchange(t, a, b)
}
