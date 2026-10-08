package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

//go:embed templates static
var assets embed.FS

type Config struct {
	DataDir    string
	Listen     string
	BaseURL    string // public URL of the web UI, e.g. https://git.example.com
	SSHHost    string // host in clone URLs (git@HOST:~user/repo); default: host of BaseURL
	SiteName   string
	TLSCert    string
	TLSKey     string
	Insecure   bool // allow cookies over plain HTTP (local testing only)
	TrustProxy bool // take the client IP from X-Forwarded-For
	// RealIPHeader names a header holding the client IP, set by the reverse
	// proxy (e.g. X-Real-IP). It takes precedence over X-Forwarded-For.
	RealIPHeader string
}

type Server struct {
	cfg      Config
	store    *Store
	sessions *sessionStore
	limiter  *limiter
	alerts   *codeAlerts
	pending  *pendingSignups
	box      *secretBox // encrypts TOTP secrets
	reposDir string
	gitPath  string
	tmpl     map[string]*template.Template
	assetVer string // changes whenever the stylesheets change (cache busting)
}

func NewServer(cfg Config) (*Server, error) {
	store, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	box, err := loadSecretBox(cfg.DataDir, store)
	if err != nil {
		return nil, err
	}
	if err := encryptTOTPSecrets(store, box); err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		box:      box,
		store:    store,
		sessions: newSessionStore(),
		limiter:  newLimiter(10, 15*time.Minute),
		alerts:   newCodeAlerts(),
		pending:  newPendingSignups(),
		reposDir: filepath.Join(cfg.DataDir, "repos"),
		gitPath:  gitPath,
		tmpl:     make(map[string]*template.Template),
	}
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		return nil, err
	}
	s.assetVer = hashToken(string(css) + string(highlightCSS()))[:12]
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(filepath.Base(p), ".html")
		if name == "base" {
			continue
		}
		t, err := template.New("base.html").Funcs(templateFuncs).ParseFS(assets, "templates/base.html", p)
		if err != nil {
			return nil, err
		}
		s.tmpl[name] = t
	}
	return s, nil
}

func (s *Server) cookieName() string {
	if s.cfg.Insecure {
		return "gs_session"
	}
	return "__Host-gs_session"
}

// sshHost is the host name shown in clone URLs.
func (s *Server) sshHost(r *http.Request) string {
	if s.cfg.SSHHost != "" {
		return s.cfg.SSHHost
	}
	if u, err := url.Parse(s.cfg.BaseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return r.Host
}

// cloneURL is the SSH remote for a repository, e.g. git@git.example.com:~obk/example.
func (s *Server) cloneURL(r *http.Request, repo *Repo) string {
	return "git@" + s.sshHost(r) + ":" + repo.FullName()
}

// Handler serves the web UI. Git itself is served over SSH (see ssh.go).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /static/style.css", s.handleStyle)
	mux.HandleFunc("GET /static/highlight.css", s.handleHighlightCSS)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /signup", s.handleSignupForm)
	mux.HandleFunc("POST /signup", s.handleSignup)
	mux.HandleFunc("POST /signup/confirm", s.handleSignupConfirm)
	mux.HandleFunc("GET /create", s.requireUser(s.handleCreateForm))
	mux.HandleFunc("POST /create", s.requireUser(s.handleCreate))
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /settings/keys", s.requireUser(s.handleKeys))
	mux.HandleFunc("POST /settings/keys", s.requireUser(s.handleKeyAdd))
	mux.HandleFunc("POST /settings/keys/delete", s.requireUser(s.handleKeyDelete))
	mux.HandleFunc("GET /settings/password", s.requireUser(s.handlePassword))
	mux.HandleFunc("POST /settings/password", s.requireUser(s.handlePasswordChange))
	mux.HandleFunc("GET /settings/invites", s.requireAdmin(s.handleInvites))
	mux.HandleFunc("POST /settings/invites", s.requireAdmin(s.handleInviteCreate))
	mux.HandleFunc("POST /settings/invites/revoke", s.requireAdmin(s.handleInviteRevoke))
	mux.HandleFunc("GET /{owner}", s.handleUserPage)
	mux.HandleFunc("GET /{owner}/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+url.PathEscape(r.PathValue("owner")), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /{owner}/{repo}/{rest...}", s.handleRepo)
	mux.HandleFunc("POST /{owner}/{repo}/settings", s.requireUser(s.handleRepoSettings))
	mux.HandleFunc("POST /{owner}/{repo}/delete", s.requireUser(s.handleRepoDelete))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) })

	h := s.requireSSHKey(mux)
	h = s.withSession(h)
	h = http.NewCrossOriginProtection().Handler(h)
	web := h
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := gitHTTPRe.FindStringSubmatch(r.URL.Path); m != nil {
			s.serveGitHTTP(w, r, m[1], m[2], m[3])
			return
		}
		web.ServeHTTP(w, r)
	})
	return s.logRequests(s.secureHeaders(h))
}

// Read-only git over HTTPS: the repository's web URL is also its clone URL,
// e.g. git clone https://git.example.com/~obk/example (".git" is optional).
// Only public repositories are served; pushing is SSH-only.
var gitHTTPRe = regexp.MustCompile(`^/~([a-z0-9][a-z0-9_-]{0,31})/([A-Za-z0-9][A-Za-z0-9._-]{0,99}?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

// cloneSlots limits concurrent HTTPS clones/fetches (separate from the web UI).
var cloneSlots = make(chan struct{}, max(2, runtime.NumCPU()))

// cloneTimeout ends an HTTPS clone or fetch that takes longer, so clients
// that stop reading cannot hold the clone slots forever.
const cloneTimeout = 30 * time.Minute

func (s *Server) serveGitHTTP(w http.ResponseWriter, r *http.Request, owner, name, endpoint string) {
	plain := func(status int, msg string) {
		// git shows text/plain error bodies to the user as "remote: ...".
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		fmt.Fprintln(w, msg)
	}
	sshURL := "git@" + s.sshHost(r) + ":~" + owner + "/" + name
	switch {
	case endpoint == "git-receive-pack" ||
		(endpoint == "info/refs" && r.URL.Query().Get("service") == "git-receive-pack"):
		plain(http.StatusForbidden, "Pushing over HTTPS is not supported; push over SSH instead:\n  git remote set-url --push origin "+sshURL)
		return
	case endpoint == "info/refs" && r.Method == http.MethodGet && r.URL.Query().Get("service") == "git-upload-pack":
	case endpoint == "git-upload-pack" && r.Method == http.MethodPost:
	default:
		plain(http.StatusNotFound, "Only git clone/fetch over the smart HTTP protocol is supported here.")
		return
	}
	repo, err := loadRepo(s.reposDir, owner, name)
	if err != nil || !repo.Public {
		// Same answer for private and missing repositories.
		plain(http.StatusNotFound, "Repository not found. Private repositories can only be cloned over SSH:\n  git clone "+sshURL)
		return
	}
	// When the deadline passes, writes fail and the CGI handler kills git.
	// The deadlines are cleared afterwards: the connection may be reused.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(cloneTimeout)
	rc.SetReadDeadline(deadline)
	rc.SetWriteDeadline(deadline)
	defer rc.SetReadDeadline(time.Time{})
	defer rc.SetWriteDeadline(time.Time{})
	select {
	case cloneSlots <- struct{}{}:
		defer func() { <-cloneSlots }()
	case <-r.Context().Done():
		return
	case <-time.After(gitTimeout):
		plain(http.StatusServiceUnavailable, "The server is busy; try again in a moment.")
		return
	}
	r2 := r.Clone(r.Context())
	r2.Header.Del("Authorization")
	r2.Header.Del("Cookie")
	r2.Header.Del("Git-Protocol") // no protocol v2; see cmdSSHServe
	r2.URL.Path = "/" + owner + "/" + name + ".git/" + endpoint
	r2.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	h := &cgi.Handler{
		Path: s.gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.reposDir,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=false",
			"GIT_CONFIG_KEY_1=http.getanyfile", "GIT_CONFIG_VALUE_1=false",
		},
	}
	h.ServeHTTP(w, r2)
}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		if !s.cfg.Insecure {
			h.Set("Strict-Transport-Security", "max-age=63072000")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		// The query string is deliberately not logged.
		// %q: the decoded path may contain newlines that would forge log lines.
		log.Printf("%s %s %q %d %s", s.clientIP(r), r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy && s.cfg.RealIPHeader != "" {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get(s.cfg.RealIPHeader))); ip != nil {
			return ip.String()
		}
	}
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipKey is the rate-limit key for a client. IPv6 clients usually control a
// whole /64, so they are limited per /64 rather than per address.
func ipKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "ip:" + ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return "ip:" + v4.String()
	}
	return "ip:" + parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

// withSession attaches the logged-in user (if any) to the request context.
// A session ends when the user is deleted or their password or TOTP secret
// changes (also when changed from the command line).
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(s.cookieName()); err == nil {
			if sess, ok := s.sessions.get(c.Value); ok {
				u, err := s.store.Get(sess.user)
				if err == nil && constantTimeEqual(sess.credFP, credentialFingerprint(u)) {
					ctx := context.WithValue(r.Context(), ctxUser, u)
					ctx = context.WithValue(ctx, ctxSession, &sess)
					r = r.WithContext(ctx)
				} else {
					s.sessions.delete(c.Value)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireSSHKey sends logged-in users without an SSH key to the key page:
// every account must have at least one.
func (s *Server) requireSSHKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := currentUser(r); u != nil && len(u.SSHKeys) == 0 {
			switch r.URL.Path {
			case "/settings/keys", "/logout", "/static/style.css", "/static/highlight.css":
			default:
				http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUser).(*User)
	return u
}

func currentSession(r *http.Request) *session {
	sess, _ := r.Context().Value(ctxSession).(*session)
	return sess
}

func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				s.notFound(w, r)
			}
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if r.Method == http.MethodPost && !s.validCSRF(r) {
			s.error(w, r, http.StatusForbidden, "Invalid or missing CSRF token.")
			return
		}
		h(w, r)
	}
}

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).Admin {
			s.notFound(w, r)
			return
		}
		h(w, r)
	})
}

// startSession issues a fresh session ID (preventing session fixation).
// A non-empty notice is shown on the password page during this session.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *User, notice string) {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    s.sessions.create(u.Name, credentialFingerprint(u), notice),
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		Secure:   !s.cfg.Insecure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) validCSRF(r *http.Request) bool {
	sess := currentSession(r)
	if sess == nil {
		return false
	}
	got := r.PostFormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}
