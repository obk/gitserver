package web

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"go-git-server/internal/gitrepo"
	"go-git-server/internal/render"
	"go-git-server/internal/store"
)

var templateFuncs = template.FuncMap{
	"date":     func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	"datefull": func(t time.Time) string { return t.Format("Mon, 02 Jan 2006 15:04:05 -0700") },
	"ago":      timeAgo,
	"short":    func(h string) string { return h[:min(len(h), 8)] },
	"inc":      func(i int) int { return i + 1 },
	"join":     strings.Join,
	"size":     humanSize,
	"ev":       func(repo string, c gitrepo.Commit) eventData { return eventData{repo, c} },
}

type eventData struct {
	Repo string
	C    gitrepo.Commit
}

// page is the data passed to every template.
type page struct {
	Site     string
	AssetVer string
	Title    string
	Path     string
	User     *store.User
	CSRF     string
	Tab      string
	Repo     *gitrepo.Repo
	Clone    string // SSH clone URL
	CloneWeb string // HTTPS clone URL (public repositories only)
	IsOwner  bool
	Data     any
	Flash    string // shown once at the top of the page (see setFlash)
}

func (s *Server) newPage(r *http.Request, title string, data any) *page {
	p := &page{Site: s.cfg.SiteName, AssetVer: s.assetVer, Title: title, Path: r.URL.RequestURI(), User: currentUser(r), Data: data}
	if sess := currentSession(r); sess != nil {
		p.CSRF = sess.csrf
		if c, err := r.Cookie(s.cookieName()); err == nil {
			p.Flash = s.sessions.takeFlash(c.Value)
		}
	}
	return p
}

func (s *Server) repoPage(r *http.Request, repo *gitrepo.Repo, tab, title string, data any) *page {
	if title == "" {
		title = repo.FullName()
	} else {
		title += " - " + repo.FullName()
	}
	p := s.newPage(r, title, data)
	p.Repo = repo
	p.Tab = tab
	p.Clone = s.cloneURL(r, repo)
	if repo.Public {
		p.CloneWeb = s.baseURL(r) + repo.Path()
	}
	p.IsOwner = repo.IsOwner(p.User)
	return p
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p *page) {
	var buf bytes.Buffer
	if err := s.tmpl[name].Execute(&buf, p); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) error(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, status, "error", s.newPage(r, http.StatusText(status), msg))
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.error(w, r, http.StatusNotFound, "The page you are looking for does not exist.")
}

func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFileFS(w, r, assets, "static/style.css")
}

func (s *Server) handleHighlightCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(render.HighlightCSS())
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// fileMode renders a git tree mode the way ls does.
func fileMode(mode string) string {
	switch mode {
	case "040000":
		return "d---------"
	case "100755":
		return "-rwxr-xr-x"
	case "120000":
		return "lrwxrwxrwx"
	case "160000":
		return "m---------"
	default:
		return "-rw-r--r--"
	}
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month")
	default:
		return plural(int(d.Hours()/24/365), "year")
	}
}
