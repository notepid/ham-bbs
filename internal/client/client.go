// Package client implements the HamBBS client: a local mailbox that is
// synchronised with a BBS in short batch sessions, so reading and writing
// happen offline and airtime is spent only on transferring data.
package client

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/notepid/ham-bbs/internal/autorun"
	"github.com/notepid/ham-bbs/internal/buildinfo"
	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/identity"
	"github.com/notepid/ham-bbs/internal/proto"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport"
	"github.com/notepid/ham-bbs/internal/transport/agwpe"
	"github.com/notepid/ham-bbs/internal/transport/tcp"
	"github.com/notepid/ham-bbs/internal/transport/vara"
)

const (
	keyCursor = "cursor"
)

// clientName is sent in HELLO.
func clientName() string { return "hambbs-client/" + buildinfo.String() }

// Client is the local mailbox plus sync engine.
type Client struct {
	Store *store.Store

	mu     sync.Mutex
	cfg    config.Client
	syncMu sync.Mutex // one sync at a time

	lastMu     sync.Mutex
	lastReport *Report

	helper *autorun.Process // starts the modem or TNC; set once by SetHelper
}

// Report summarises one sync session.
type Report struct {
	Started    time.Time
	Duration   time.Duration
	Server     string
	Uploaded   int
	Rejected   []proto.Rejection
	NewHeaders int
	NewBodies  int
	More       bool
	MOTD       string
	Registered bool
	BytesOut   int
	BytesIn    int
	Err        error
}

// New creates a client around an open store.
func New(cfg config.Client, st *store.Store) *Client {
	return &Client{cfg: cfg, Store: st}
}

// Config returns the current configuration.
func (c *Client) Config() config.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// SetConfig replaces and saves the configuration. If the server changes,
// the sync cursor is reset so the new server's messages are fetched.
func (c *Client) SetConfig(cfg config.Client) error {
	c.mu.Lock()
	old := c.cfg
	c.cfg = cfg
	c.mu.Unlock()
	if old.Address != cfg.Address || old.Transport != cfg.Transport || old.Callsign != cfg.Callsign || strings.Join(old.Areas, ",") != strings.Join(cfg.Areas, ",") {
		c.Store.SetInt(keyCursor, 0)
	}
	if err := config.SaveClient(cfg); err != nil {
		return err
	}
	return c.startHelper(cfg)
}

// SetHelper lets the client run the modem or TNC program configured for
// its transport (config.Client.Autorun). Call it once, before use.
func (c *Client) SetHelper(p *autorun.Process) error {
	c.helper = p
	return c.startHelper(c.Config())
}

// Helper returns the process set with SetHelper, or nil.
func (c *Client) Helper() *autorun.Process { return c.helper }

// startHelper starts, replaces or stops the helper program to match cfg.
func (c *Client) startHelper(cfg config.Client) error {
	if c.helper == nil {
		return nil
	}
	cmd, _ := cfg.Autorun()
	return c.helper.Start(cmd)
}

// KeyFromPassword derives the stored key for callsign + password.
func KeyFromPassword(call, password string) string {
	return base64.StdEncoding.EncodeToString(identity.DeriveKey(call, password).Seed())
}

// LastReport returns the most recent sync report, if any.
func (c *Client) LastReport() *Report {
	c.lastMu.Lock()
	defer c.lastMu.Unlock()
	return c.lastReport
}

// Compose queues a message in the outbox.
func (c *Client) Compose(kind, to, subject, body string) (store.OutMsg, error) {
	cfg := c.Config()
	var err error
	switch kind {
	case "P":
		_, err = callsign.ParsePersonal(to)
	case "B":
		_, err = callsign.ParseBulletin(to)
	default:
		err = fmt.Errorf("unknown kind %q", kind)
	}
	if err != nil {
		return store.OutMsg{}, err
	}
	if strings.TrimSpace(subject) == "" {
		return store.OutMsg{}, errors.New("subject is empty")
	}
	now := time.Now()
	o := store.OutMsg{
		ID:      proto.NewMessageID(cfg.Callsign, now.UnixNano()),
		Kind:    kind,
		To:      callsign.Normalize(to),
		Subject: strings.TrimSpace(subject),
		Body:    strings.ReplaceAll(body, "\r\n", "\n"),
		Created: now,
	}
	return o, c.Store.AddOutbox(o)
}

// Sync runs one session against the configured server.
func (c *Client) Sync(ctx context.Context) *Report {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	cfg := c.Config()
	rep := &Report{Started: time.Now(), Server: cfg.Address}
	if c.helper != nil {
		if _, addr := cfg.Autorun(); addr != "" {
			c.helper.WaitPort(ctx, addr) // just started: give it time to open its port
		}
	}
	rep.Err = c.sync(ctx, cfg, rep)
	rep.Duration = time.Since(rep.Started)
	c.lastMu.Lock()
	c.lastReport = rep
	c.lastMu.Unlock()
	return rep
}

func dialer(cfg config.Client) (transport.Dialer, error) {
	switch cfg.Transport {
	case "", "tcp":
		return tcp.Dialer{}, nil
	case "vara":
		return vara.Dialer{Config: vara.Config{Addr: cfg.VaraAddr, MyCalls: []string{cfg.Callsign}}}, nil
	case "ax25":
		return agwpe.Dialer{Config: agwpe.Config{Addr: cfg.AGWAddr, Port: cfg.AGWPort, MyCalls: []string{cfg.Callsign}}}, nil
	}
	return nil, fmt.Errorf("transport %q is not supported", cfg.Transport)
}

// syncTimeout bounds a whole sync. 1200 baud packet moves only about 100
// bytes per second, so it gets more time.
func syncTimeout(transport string) time.Duration {
	if transport == "ax25" {
		return 30 * time.Minute
	}
	return 10 * time.Minute
}

// countingConn tallies bytes so users see what a sync costs in airtime.
type countingConn struct {
	transport.Conn
	in, out *int
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	*c.in += n
	return n, err
}

func (c countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	*c.out += n
	return n, err
}

func (c *Client) sync(ctx context.Context, cfg config.Client, rep *Report) error {
	if !cfg.Configured() {
		return errors.New("client is not configured: set callsign, password and server in Settings")
	}
	seed, err := base64.StdEncoding.DecodeString(cfg.Key)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("stored key is invalid; set your password again in Settings")
	}
	key := ed25519.NewKeyFromSeed(seed)
	call := callsign.Base(cfg.Callsign)

	d, err := dialer(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout(cfg.Transport))
	defer cancel()
	raw, err := d.Dial(ctx, cfg.Address)
	if err != nil {
		return fmt.Errorf("connect %s: %w", cfg.Address, err)
	}
	defer raw.Close()
	go func() { <-ctx.Done(); raw.Close() }()
	conn := countingConn{Conn: raw, in: &rep.BytesIn, out: &rep.BytesOut}

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	lr := &proto.LineReader{R: r, Max: 1024}

	// 1. Banner: find node and nonce.
	var banner proto.Banner
	for {
		line, err := lr.ReadLine()
		if err != nil {
			return fmt.Errorf("reading banner: %w", err)
		}
		if b, ok := proto.ParseBanner(line); ok {
			banner = b
			break
		}
	}

	// 2. Send the whole request batch in one go.
	w.WriteString(proto.Magic + "\r")
	hello := proto.Hello{
		Call:    call,
		Version: proto.Version,
		Sig:     identity.Sign(key, banner.Node, call, banner.Nonce),
		PubKey:  key.Public().(ed25519.PublicKey),
		Client:  clientName(),
	}
	proto.WriteFrame(w, proto.THello, hello)

	queued, err := c.Store.ListOutbox("queued")
	if err != nil {
		return err
	}
	if len(queued) > 0 {
		put := proto.Put{}
		for _, o := range queued {
			put.Messages = append(put.Messages, o.Proto(call))
		}
		proto.WriteFrame(w, proto.TPut, put)
	}

	since := c.Store.GetInt(keyCursor)
	q := proto.Sync{
		Since:         since,
		Areas:         cfg.Areas,
		FetchPersonal: cfg.FetchPersonal,
		FetchAreas:    cfg.FetchAreas,
	}
	if since == 0 && cfg.InitialDays > 0 {
		q.After = time.Now().AddDate(0, 0, -cfg.InitialDays).Unix()
	}
	proto.WriteFrame(w, proto.TSync, q)

	wants, err := c.Store.Wants()
	if err != nil {
		return err
	}
	if len(wants) > 0 {
		proto.WriteFrame(w, proto.TGet, proto.Get{IDs: wants})
	}
	proto.WriteFrame(w, proto.TBye, nil)
	if err := w.Flush(); err != nil {
		return err
	}

	// 3. Skip the terminal prompt and wait for the server to enter frame mode.
	for {
		line, err := lr.ReadLine()
		if err != nil {
			return fmt.Errorf("waiting for server: %w", err)
		}
		if strings.Contains(line, proto.ReadyLine) {
			break
		}
	}

	// 4. Read the response batch.
	var cursor int64 = -1
	for {
		f, err := proto.ReadFrame(r)
		if err != nil {
			return fmt.Errorf("reading response: %w", err)
		}
		switch f.Type {
		case proto.TError:
			var e proto.Error
			f.Decode(&e)
			return &e
		case proto.TWelcome:
			var wl proto.Welcome
			if err := f.Decode(&wl); err != nil {
				return err
			}
			rep.MOTD, rep.Registered = wl.MOTD, wl.Registered
		case proto.TPutResult:
			var pr proto.PutResult
			if err := f.Decode(&pr); err != nil {
				return err
			}
			for _, id := range pr.Accepted {
				c.Store.SetOutboxStatus(id, "sent", "")
			}
			for _, rj := range pr.Rejected {
				c.Store.SetOutboxStatus(rj.ID, "rejected", rj.Reason)
			}
			rep.Uploaded += len(pr.Accepted)
			rep.Rejected = append(rep.Rejected, pr.Rejected...)
		case proto.THeaders:
			var h proto.Headers
			if err := f.Decode(&h); err != nil {
				return err
			}
			for _, m := range h.Headers {
				if err := c.Store.SaveFromServer(m, false); err != nil {
					return err
				}
			}
			rep.NewHeaders += len(h.Headers)
			rep.More = rep.More || h.More
			cursor = h.Cursor
		case proto.TMessages:
			var ms proto.Messages
			if err := f.Decode(&ms); err != nil {
				return err
			}
			for _, m := range ms.Messages {
				if err := c.Store.SaveFromServer(m, true); err != nil {
					return err
				}
				if m.From == call {
					c.Store.SetRead(m.ID, true)
				}
			}
			rep.NewBodies += len(ms.Messages)
		case proto.TEnd:
			// Not expected after BYE; ignore.
		case proto.TBye:
			// Only advance the cursor once everything was stored.
			if cursor >= 0 {
				c.Store.SetInt(keyCursor, cursor)
			}
			c.Store.CleanWants()
			return nil
		}
	}
}
