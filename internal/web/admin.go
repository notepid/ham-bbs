package web

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/notepid/ham-bbs/internal/autorun"
	"github.com/notepid/ham-bbs/internal/bbs"
	"github.com/notepid/ham-bbs/internal/callsign"
	"github.com/notepid/ham-bbs/internal/config"
	"github.com/notepid/ham-bbs/internal/identity"
	"github.com/notepid/ham-bbs/internal/store"
)

// Admin is the sysop web UI.
type Admin struct {
	srv     *bbs.Server
	r       *renderer
	helpers []*autorun.Process
}

// NewAdmin builds the admin UI handler. helpers are the programs (modem,
// TNC) the server started; the dashboard shows how they are doing.
func NewAdmin(srv *bbs.Server, log *slog.Logger, helpers ...*autorun.Process) http.Handler {
	a := &Admin{srv: srv, r: newRenderer("admin", log), helpers: helpers}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /users", a.users)
	mux.HandleFunc("POST /users", a.addUser)
	mux.HandleFunc("POST /users/{call}/password", a.setPassword)
	mux.HandleFunc("POST /users/{call}/admin", a.toggleAdmin)
	mux.HandleFunc("POST /users/{call}/delete", a.deleteUser)
	mux.HandleFunc("GET /messages", a.messages)
	mux.HandleFunc("GET /messages/{id}", a.message)
	mux.HandleFunc("POST /messages/{id}/delete", a.deleteMessage)
	mux.HandleFunc("GET /neighbors", a.neighbors)
	mux.HandleFunc("POST /neighbors/{node}/forward", a.forwardNow)

	var h http.Handler = sameOrigin(mux)
	if hash := srv.Config().Admin.PasswordHash; hash != "" {
		h = basicAuth(hash, h)
	}
	return h
}

func basicAuth(hash string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte("sysop")) != 1 || !config.CheckPassword(hash, pw) {
			w.Header().Set("WWW-Authenticate", `Basic realm="HamBBS admin (user: sysop)"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type adminHeader struct {
	Node     string
	Callsign string
}

func (a *Admin) page(r *http.Request, title, active string, data any) page {
	cfg := a.srv.Config()
	return page{
		Title:  title,
		Active: active,
		Flash:  r.URL.Query().Get("flash"),
		Header: adminHeader{Node: cfg.Node, Callsign: cfg.Callsign},
		Data:   data,
	}
}

func (a *Admin) dashboard(w http.ResponseWriter, r *http.Request) {
	st, _ := a.srv.Store().Stats()
	areas, _ := a.srv.Store().Areas()
	cfg := a.srv.Config()
	var helpers []autorun.Status
	for _, p := range a.helpers {
		helpers = append(helpers, p.Status())
	}
	a.r.render(w, "dashboard", a.page(r, "Dashboard", "dash", map[string]any{
		"Config":   cfg,
		"Helpers":  helpers,
		"Stats":    st,
		"Areas":    areas,
		"Sessions": a.srv.Sessions(),
		"Events":   a.srv.Events(),
		"Uptime":   time.Since(a.srv.Started()).Round(time.Minute).String(),
	}))
}

func (a *Admin) users(w http.ResponseWriter, r *http.Request) {
	us, err := a.srv.Store().ListUsers()
	p := a.page(r, "Users", "users", us)
	if err != nil {
		p.Error = err.Error()
	}
	a.r.render(w, "users", p)
}

func (a *Admin) addUser(w http.ResponseWriter, r *http.Request) {
	call := callsign.Normalize(r.FormValue("call"))
	pw := r.FormValue("password")
	if !callsign.Valid(call) {
		redirect(w, r, "/users", "Invalid callsign.")
		return
	}
	call = callsign.Base(call)
	var key []byte
	if pw != "" {
		key = identity.PublicKey(call, pw)
	}
	if err := a.srv.Store().CreateUser(call, key, strings.TrimSpace(r.FormValue("name")), r.FormValue("admin") != ""); err != nil {
		redirect(w, r, "/users", "Could not add "+call+" (already exists?).")
		return
	}
	redirect(w, r, "/users", "Added "+call+".")
}

func (a *Admin) setPassword(w http.ResponseWriter, r *http.Request) {
	call := r.PathValue("call")
	pw := r.FormValue("password")
	var key []byte // empty password: clear the key so the next client login claims it
	if pw != "" {
		key = identity.PublicKey(call, pw)
	}
	if err := a.srv.Store().SetUserKey(call, key); err != nil {
		redirect(w, r, "/users", "Could not update "+call+".")
		return
	}
	if pw == "" {
		redirect(w, r, "/users", "Password for "+call+" cleared; the next client login sets it.")
		return
	}
	redirect(w, r, "/users", "Password for "+call+" changed.")
}

func (a *Admin) toggleAdmin(w http.ResponseWriter, r *http.Request) {
	call := r.PathValue("call")
	u, err := a.srv.Store().GetUser(call)
	if err == nil {
		err = a.srv.Store().SetUserAdmin(call, !u.Admin)
	}
	if err != nil {
		redirect(w, r, "/users", "Could not update "+call+".")
		return
	}
	redirect(w, r, "/users", "Updated "+call+".")
}

func (a *Admin) deleteUser(w http.ResponseWriter, r *http.Request) {
	call := r.PathValue("call")
	if err := a.srv.Store().DeleteUser(call); err != nil {
		redirect(w, r, "/users", "Could not delete "+call+".")
		return
	}
	redirect(w, r, "/users", "Deleted "+call+".")
}

func (a *Admin) neighbors(w http.ResponseWriter, r *http.Request) {
	held, err := a.srv.Held()
	p := a.page(r, "Neighbours", "nbrs", map[string]any{"Neighbors": a.srv.Neighbors(), "Held": held})
	if err != nil {
		p.Error = err.Error()
	}
	a.r.render(w, "neighbors", p)
}

func (a *Admin) forwardNow(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")
	if err := a.srv.ForwardInBackground(node); err != nil {
		redirect(w, r, "/neighbors", err.Error())
		return
	}
	redirect(w, r, "/neighbors", "Calling "+node+"; reload to see the result.")
}

func (a *Admin) messages(w http.ResponseWriter, r *http.Request) {
	f := store.ListFilter{Kind: r.URL.Query().Get("kind"), Area: strings.ToUpper(r.URL.Query().Get("area")), Limit: 300}
	ms, err := a.srv.Store().ListMessages(f)
	areas, _ := a.srv.Store().Areas()
	p := a.page(r, "Messages", "msgs", map[string]any{"Messages": ms, "Areas": areas, "Filter": f})
	if err != nil {
		p.Error = err.Error()
	}
	a.r.render(w, "messages", p)
}

func (a *Admin) message(w http.ResponseWriter, r *http.Request) {
	m, err := a.srv.Store().MessageByID(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	a.r.render(w, "message", a.page(r, m.Subject, "msgs", m))
}

func (a *Admin) deleteMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.srv.Store().DeleteMessage(id); err != nil {
		redirect(w, r, "/messages", "Could not delete message.")
		return
	}
	redirect(w, r, "/messages", "Message deleted.")
}
