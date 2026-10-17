package bbs_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/notepid/ham-bbs/internal/bbs"
	"github.com/notepid/ham-bbs/internal/client"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport"
	"github.com/notepid/ham-bbs/internal/transport/tcp"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type node struct {
	name string
	srv  *bbs.Server
	st   *store.Store
	addr string
	l    transport.Listener
}

// listen reserves a TCP port for a node before its neighbours are known.
func listen(t *testing.T, name string) *node {
	t.Helper()
	l, err := tcp.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return &node{name: name, l: l, addr: strings.TrimPrefix(l.String(), "tcp ")}
}

// start runs the node's BBS with the given neighbours.
func (n *node) start(t *testing.T, nbs ...config.Neighbor) {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.Callsign, cfg.Node = n.name, n.name
	cfg.Neighbors = nbs
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n.st = st
	n.srv = bbs.New(cfg, st, quiet)
	n.srv.SetDialer("tcp", tcp.Dialer{})
	go n.srv.Serve(n.l)
}

// link describes neighbour peer as seen from n.
func (n *node) link(peer *node, routes ...string) config.Neighbor {
	return config.Neighbor{Node: peer.name, Password: "link-" + min(n.name, peer.name) + max(n.name, peer.name),
		Connect: "tcp:" + peer.addr, Routes: routes}
}

func (n *node) forward(t *testing.T, peer *node) bbs.ForwardResult {
	t.Helper()
	res, err := n.srv.ForwardNow(context.Background(), peer.name)
	if err != nil {
		t.Fatalf("%s -> %s: %v", n.name, peer.name, err)
	}
	return res
}

// queuedSoon waits briefly for n's queue for peer to reach want: the
// answering side finishes its part after the caller has returned.
func (n *node) queuedSoon(peer string, want int) int {
	for i := 0; i < 100 && n.queued(peer) != want; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return n.queued(peer)
}

func (n *node) queued(peer string) int {
	for _, st := range n.srv.Neighbors() {
		if st.Node == peer {
			return st.Queued
		}
	}
	return -1
}

func (n *node) user(t *testing.T, call string) *client.Client {
	t.Helper()
	cfg := config.DefaultClient()
	cfg.SetPath(filepath.Join(t.TempDir(), "client.toml"))
	cfg.Callsign, cfg.Address = call, n.addr
	cfg.Key = client.KeyFromPassword(call, "pw-"+call)
	cfg.InitialDays = 0
	cfg.FetchAreas = []string{"*"}
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return client.New(cfg, st)
}

func sync(t *testing.T, c *client.Client) *client.Report {
	t.Helper()
	rep := c.Sync(context.Background())
	if rep.Err != nil {
		t.Fatalf("sync: %v", rep.Err)
	}
	return rep
}

func message(t *testing.T, st *store.Store, subject string) store.Message {
	t.Helper()
	ms, _ := st.ListMessages(store.ListFilter{Limit: 100})
	for _, m := range ms {
		if m.Subject == subject {
			return m
		}
	}
	t.Fatalf("no message %q", subject)
	return store.Message{}
}

func has(st *store.Store, subject string) bool {
	ms, _ := st.ListMessages(store.ListFilter{Limit: 100})
	return slices.ContainsFunc(ms, func(m store.Message) bool { return m.Subject == subject })
}

// TestForwardChain runs a line of three nodes, A - B - C. A and C only
// know B; B routes mail between them.
func TestForwardChain(t *testing.T) {
	a, b, c := listen(t, "LA1AA"), listen(t, "LA2BB"), listen(t, "LA3CC")
	a.start(t, a.link(b, "*"))
	b.start(t, b.link(a), b.link(c))
	c.start(t, c.link(b, "*"))

	alice := a.user(t, "LA1ALI")
	zed := c.user(t, "LA3ZED")
	sync(t, zed) // registers zed at C

	alice.Compose("B", "TECH@ALL", "Network bulletin", "Goes everywhere")
	alice.Compose("B", "LOCAL", "Club bulletin", "Stays on A")
	alice.Compose("P", "LA3ZED@LA3CC", "Hello Zed", "Mail across two hops")
	if rep := sync(t, alice); rep.Uploaded != 3 {
		t.Fatalf("alice upload: %+v", rep)
	}
	if n := a.queued("LA2BB"); n != 2 {
		t.Fatalf("A has %d queued for B, want 2", n)
	}

	// A calls B.
	if res := a.forward(t, b); res.Sent != 2 || res.Received != 0 {
		t.Fatalf("A->B: %+v", res)
	}
	if a.queued("LA2BB") != 0 || b.queuedSoon("LA3CC", 2) != 2 || b.queued("LA1AA") != 0 {
		t.Fatalf("queues after A->B: A>B %d, B>C %d, B>A %d", a.queued("LA2BB"), b.queued("LA3CC"), b.queued("LA1AA"))
	}
	if has(b.st, "Club bulletin") {
		t.Fatal("local bulletin was forwarded")
	}

	// A user at B with zed's callsign must not see mail in transit.
	if ms, _ := b.st.ListMessages(store.ListFilter{Call: "LA3ZED", Kind: "P"}); len(ms) != 0 {
		t.Fatalf("transit mail visible at B: %+v", ms)
	}

	// C calls B: B answers and sends in its last transmission.
	if res := c.forward(t, b); res.Received != 2 {
		t.Fatalf("C->B: %+v", res)
	}
	m := message(t, c.st, "Network bulletin")
	if strings.Join(m.Path, ",") != "LA1AA,LA2BB,LA3CC" || m.Origin != "LA1AA" || m.From != "LA1ALI" {
		t.Fatalf("bulletin at C: path %v origin %s from %s", m.Path, m.Origin, m.From)
	}
	// B could not see the acknowledgement yet; the next session settles it.
	if res := c.forward(t, b); res.Sent != 0 || res.Received != 0 {
		t.Fatalf("second C->B moved messages: %+v", res)
	}
	if n := b.queuedSoon("LA3CC", 0); n != 0 {
		t.Fatalf("B still has %d queued for C", n)
	}
	// Nothing echoes back to A.
	if res := a.forward(t, b); res.Sent != 0 || res.Received != 0 {
		t.Fatalf("A->B echo: %+v", res)
	}

	if rep := sync(t, zed); rep.NewBodies != 2 {
		t.Fatalf("zed sync: %+v", rep)
	}
	inbox, _ := zed.Store.ListMessages(store.ListFilter{Kind: "P"})
	if len(inbox) != 1 || inbox[0].Body != "Mail across two hops" {
		t.Fatalf("zed inbox: %+v", inbox)
	}

	// Zed replies; B calls A this time.
	zed.Compose("P", "LA1ALI@LA1AA", "Re: Hello Zed", "Got it")
	sync(t, zed)
	if res := c.forward(t, b); res.Sent != 1 {
		t.Fatalf("C->B reply: %+v", res)
	}
	if res := b.forward(t, a); res.Sent != 1 {
		t.Fatalf("B->A reply: %+v", res)
	}
	sync(t, alice)
	inbox, _ = alice.Store.ListMessages(store.ListFilter{Kind: "P"})
	if len(inbox) != 1 || inbox[0].Subject != "Re: Hello Zed" {
		t.Fatalf("alice inbox: %+v", inbox)
	}
}

func TestForwardWrongPassword(t *testing.T) {
	a, b := listen(t, "LA1AA"), listen(t, "LA2BB")
	la := a.link(b)
	la.Password = "wrong"
	a.start(t, la)
	b.start(t, b.link(a))
	_, err := a.srv.ForwardNow(context.Background(), "LA2BB")
	if err == nil || !strings.Contains(err.Error(), "wrong link password") {
		t.Fatalf("err = %v", err)
	}
}

func TestForwardUnknownNode(t *testing.T) {
	a, b := listen(t, "LA1AA"), listen(t, "LA2BB")
	a.start(t, a.link(b))
	b.start(t) // B does not know A
	_, err := a.srv.ForwardNow(context.Background(), "LA2BB")
	if err == nil || !strings.Contains(err.Error(), "not a neighbour") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoRoute(t *testing.T) {
	a := listen(t, "LA1AA")
	a.start(t)
	alice := a.user(t, "LA1ALI")
	alice.Compose("P", "LA9XX@LA9BBS", "Far away", "?")
	rep := sync(t, alice)
	if len(rep.Rejected) != 1 || !strings.Contains(rep.Rejected[0].Reason, "no route") {
		t.Fatalf("rejected: %+v", rep.Rejected)
	}
}

// TestForwardScopes: a neighbour only gets the bulletin scopes it is
// configured for.
func TestForwardScopes(t *testing.T) {
	a, b := listen(t, "LA1AA"), listen(t, "LA2BB")
	lb := a.link(b)
	lb.Scopes = []string{"LA"}
	a.start(t, lb)
	b.start(t, b.link(a))
	alice := a.user(t, "LA1ALI")
	alice.Compose("B", "TECH@LA", "Norway only", "x")
	alice.Compose("B", "TECH@EU", "Europe", "x")
	sync(t, alice)
	if res := a.forward(t, b); res.Sent != 1 {
		t.Fatalf("A->B: %+v", res)
	}
	if !has(b.st, "Norway only") || has(b.st, "Europe") {
		t.Fatal("wrong bulletins forwarded")
	}
}
