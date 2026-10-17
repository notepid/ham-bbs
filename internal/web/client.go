package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/client"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/store"
	"github.com/notepid/ham-bbs/internal/transport/agwpe"
)

// ClientUI is the user's mail UI.
type ClientUI struct {
	c       *client.Client
	r       *renderer
	syncing atomic.Bool
	log     *slog.Logger
}

// NewClientUI builds the client UI handler.
func NewClientUI(c *client.Client, log *slog.Logger) http.Handler {
	u := &ClientUI{c: c, r: newRenderer("client", log), log: log}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /{$}", u.inbox)
	mux.HandleFunc("GET /bulletins", u.bulletins)
	mux.HandleFunc("GET /msg/{id}", u.message)
	mux.HandleFunc("POST /msg/{id}/want", u.want)
	mux.HandleFunc("POST /msg/{id}/unread", u.unread)
	mux.HandleFunc("GET /compose", u.compose)
	mux.HandleFunc("POST /compose", u.send)
	mux.HandleFunc("GET /outbox", u.outbox)
	mux.HandleFunc("POST /outbox/{id}/delete", u.deleteOutbox)
	mux.HandleFunc("GET /sync", u.syncPage)
	mux.HandleFunc("POST /sync", u.startSync)
	mux.HandleFunc("GET /settings", u.settings)
	mux.HandleFunc("POST /settings", u.saveSettings)
	return sameOrigin(mux)
}

type clientHeader struct {
	Callsign       string
	Server         string
	UnreadPersonal int
	UnreadBulletin int
	Queued         int
	Syncing        bool
	Configured     bool
}

func (u *ClientUI) page(r *http.Request, title, active string, data any) page {
	cfg := u.c.Config()
	h := clientHeader{Callsign: cfg.Callsign, Server: cfg.Address, Syncing: u.syncing.Load(), Configured: cfg.Configured()}
	h.UnreadPersonal, h.UnreadBulletin, _ = u.c.Store.UnreadCounts()
	if q, err := u.c.Store.ListOutbox("queued"); err == nil {
		h.Queued = len(q)
	}
	return page{Title: title, Active: active, Flash: r.URL.Query().Get("flash"), Header: h, Data: data}
}

func (u *ClientUI) inbox(w http.ResponseWriter, r *http.Request) {
	if !u.c.Config().Configured() {
		redirect(w, r, "/settings", "Welcome! Enter your callsign, a password and your BBS to get started.")
		return
	}
	ms, err := u.c.Store.ListMessages(store.ListFilter{Kind: "P", Limit: 500})
	p := u.page(r, "Inbox", "inbox", ms)
	if err != nil {
		p.Error = err.Error()
	}
	u.r.render(w, "inbox", p)
}

func (u *ClientUI) bulletins(w http.ResponseWriter, r *http.Request) {
	area := strings.ToUpper(r.URL.Query().Get("area"))
	ms, err := u.c.Store.ListMessages(store.ListFilter{Kind: "B", Area: area, Limit: 500})
	areas, _ := u.c.Store.Areas()
	p := u.page(r, "Bulletins", "bull", map[string]any{"Messages": ms, "Areas": areas, "Area": area})
	if err != nil {
		p.Error = err.Error()
	}
	u.r.render(w, "bulletins", p)
}

func (u *ClientUI) message(w http.ResponseWriter, r *http.Request) {
	m, err := u.c.Store.MessageByID(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if m.HasBody && !m.Read {
		u.c.Store.SetRead(m.ID, true)
	}
	u.r.render(w, "message", u.page(r, m.Subject, map[string]string{"P": "inbox", "B": "bull"}[m.Kind], map[string]any{
		"M":      m,
		"Wanted": u.c.Store.IsWanted(m.ID),
	}))
}

func (u *ClientUI) want(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u.c.Store.AddWant(id)
	redirect(w, r, "/msg/"+id, "The text will be fetched on the next sync.")
}

func (u *ClientUI) unread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := u.c.Store.MessageByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u.c.Store.SetRead(id, false)
	if m.Kind == "B" {
		redirect(w, r, "/bulletins", "")
		return
	}
	redirect(w, r, "/", "")
}

func (u *ClientUI) compose(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind != "B" {
		kind = "P"
	}
	areas, _ := u.c.Store.Areas()
	u.r.render(w, "compose", u.page(r, "Write", "compose", map[string]any{
		"Kind": kind, "To": q.Get("to"), "Subject": q.Get("subject"), "Body": q.Get("body"), "Areas": areas,
	}))
}

func (u *ClientUI) send(w http.ResponseWriter, r *http.Request) {
	kind, to, subj, body := r.FormValue("kind"), r.FormValue("to"), r.FormValue("subject"), r.FormValue("body")
	if _, err := u.c.Compose(kind, to, subj, body); err != nil {
		areas, _ := u.c.Store.Areas()
		p := u.page(r, "Write", "compose", map[string]any{"Kind": kind, "To": to, "Subject": subj, "Body": body, "Areas": areas})
		p.Error = err.Error()
		u.r.render(w, "compose", p)
		return
	}
	redirect(w, r, "/outbox", "Queued. It will be sent on the next sync.")
}

func (u *ClientUI) outbox(w http.ResponseWriter, r *http.Request) {
	os, err := u.c.Store.ListOutbox("")
	p := u.page(r, "Outbox", "outbox", os)
	if err != nil {
		p.Error = err.Error()
	}
	u.r.render(w, "outbox", p)
}

func (u *ClientUI) deleteOutbox(w http.ResponseWriter, r *http.Request) {
	u.c.Store.DeleteOutbox(r.PathValue("id"))
	redirect(w, r, "/outbox", "Removed.")
}

func (u *ClientUI) syncPage(w http.ResponseWriter, r *http.Request) {
	u.r.render(w, "sync", u.page(r, "Sync", "sync", u.c.LastReport()))
}

func (u *ClientUI) startSync(w http.ResponseWriter, r *http.Request) {
	if u.syncing.CompareAndSwap(false, true) {
		go func() {
			defer u.syncing.Store(false)
			rep := u.c.Sync(context.Background())
			if rep.Err != nil {
				u.log.Warn("sync failed", "err", rep.Err)
			} else {
				u.log.Info("sync done", "up", rep.Uploaded, "headers", rep.NewHeaders, "bodies", rep.NewBodies,
					"bytes_out", rep.BytesOut, "bytes_in", rep.BytesIn, "took", rep.Duration)
			}
		}()
	}
	redirect(w, r, "/sync", "")
}

func (u *ClientUI) settings(w http.ResponseWriter, r *http.Request) {
	u.r.render(w, "settings", u.page(r, "Settings", "settings", u.c.Config()))
}

func (u *ClientUI) saveSettings(w http.ResponseWriter, r *http.Request) {
	cfg := u.c.Config()
	fail := func(msg string) {
		p := u.page(r, "Settings", "settings", cfg)
		p.Error = msg
		u.r.render(w, "settings", p)
	}
	call := callsign.Normalize(r.FormValue("callsign"))
	if !callsign.Valid(call) {
		fail("That doesn't look like a callsign.")
		return
	}
	call = callsign.Base(call)
	pw := r.FormValue("password")
	if call != cfg.Callsign && pw == "" {
		fail("Enter your password when changing callsign.")
		return
	}
	cfg.Callsign = call
	if pw != "" {
		cfg.Key = client.KeyFromPassword(call, pw)
	}
	cfg.Transport = r.FormValue("transport")
	cfg.Address = strings.TrimSpace(r.FormValue("address"))
	switch cfg.Transport {
	case "tcp":
	case "vara":
		cfg.Address = callsign.Normalize(cfg.Address)
		if !callsign.Valid(cfg.Address) {
			fail("For a radio connection, enter the BBS callsign as address, e.g. LA1ABC.")
			return
		}
		cfg.VaraAddr = strings.TrimSpace(r.FormValue("vara_addr"))
		if cfg.VaraAddr == "" {
			cfg.VaraAddr = config.DefaultClient().VaraAddr
		}
	case "ax25":
		dest, via, err := agwpe.ParseTarget(cfg.Address)
		if err != nil {
			fail("For packet radio, enter the BBS callsign as address, e.g. LA1ABC or LA1ABC via LA2DIG (" + err.Error() + ").")
			return
		}
		cfg.Address = dest
		if len(via) > 0 {
			cfg.Address += " via " + strings.Join(via, ",")
		}
		cfg.AGWAddr = strings.TrimSpace(r.FormValue("agw_addr"))
		if cfg.AGWAddr == "" {
			cfg.AGWAddr = config.DefaultClient().AGWAddr
		}
		port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("agw_port")))
		if err != nil || port < 0 || port > 255 {
			fail("The TNC radio port is a number: 0 for the first port.")
			return
		}
		cfg.AGWPort = port
	default:
		fail("Choose a connection type.")
		return
	}
	cfg.FetchPersonal = r.FormValue("fetch_personal") != ""
	cfg.FetchAreas = splitList(r.FormValue("fetch_areas"))
	cfg.Areas = splitList(r.FormValue("areas"))
	if cfg.Address == "" {
		fail("Enter the BBS address.")
		return
	}
	if !cfg.Configured() {
		fail("Enter a password.")
		return
	}
	if err := u.c.SetConfig(cfg); err != nil {
		fail("Could not save: " + err.Error())
		return
	}
	redirect(w, r, "/settings", "Saved.")
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToUpper(s), func(r rune) bool { return r == ',' || r == ' ' }) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}
