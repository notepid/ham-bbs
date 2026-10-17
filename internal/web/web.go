// Package web serves the two browser UIs: the sysop admin UI for the
// server and the mail UI for the client. Both are plain server-rendered
// HTML with no JavaScript build step; templates and CSS are embedded.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

//go:embed templates static
var assets embed.FS

var funcs = template.FuncMap{
	"date": func(v any) string {
		var t time.Time
		switch x := v.(type) {
		case time.Time:
			t = x
		case int64:
			if x == 0 {
				return ""
			}
			t = time.Unix(x, 0)
		}
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	"ago": func(t time.Time) string {
		d := time.Since(t).Round(time.Second)
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%ds", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh", int(d.Hours()))
		}
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	},
	"bytes": func(n int) string {
		if n < 1024 {
			return fmt.Sprintf("%d B", n)
		}
		return fmt.Sprintf("%.1f kB", float64(n)/1024)
	},
	"join": strings.Join,
	"kindname": func(k string) string {
		if k == "B" {
			return "Bulletin"
		}
		return "Personal"
	},
	"lower":    strings.ToLower,
	"addrname": func(to string) string { n, _, _ := strings.Cut(to, "@"); return n },
}

// renderer holds one parsed template per page, each combined with the
// shared layout.
type renderer struct {
	pages map[string]*template.Template
	log   *slog.Logger
}

func newRenderer(dir string, log *slog.Logger) *renderer {
	r := &renderer{pages: map[string]*template.Template{}, log: log}
	files, err := fs.Glob(assets, "templates/"+dir+"/*.html")
	if err != nil {
		panic(err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(f[strings.LastIndexByte(f, '/')+1:], ".html")
		if name == "layout" {
			continue
		}
		t := template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/"+dir+"/layout.html", f))
		r.pages[name] = t
	}
	return r
}

// page is passed to every template.
type page struct {
	Title  string
	Active string
	Flash  string
	Error  string
	Header any // UI-wide info for the layout (node name, unread counts...)
	Data   any
}

func (r *renderer) render(w http.ResponseWriter, name string, p page) {
	t, ok := r.pages[name]
	if !ok {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", p); err != nil {
		r.log.Error("render", "page", name, "err", err)
	}
}

func staticHandler() http.Handler {
	sub, _ := fs.Sub(assets, "static")
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// sameOrigin rejects cross-site form posts (CSRF). The UIs listen on
// localhost, where any web page the user visits could otherwise POST.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			src := r.Header.Get("Origin")
			if src == "" {
				src = r.Header.Get("Referer")
			}
			if src != "" {
				u, err := url.Parse(src)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-site request refused", http.StatusForbidden)
					return
				}
			}
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// redirect sends the browser to target with an optional flash message.
func redirect(w http.ResponseWriter, r *http.Request, target, flash string) {
	if flash != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + "flash=" + url.QueryEscape(flash)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// OpenBrowser opens url in the user's default browser.
func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}
