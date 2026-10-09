// Package web is the HTTP side of gitserver: the web UI, accounts and
// sessions, invites, and read-only git clone over HTTPS.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go-git-server/internal/account"
	"go-git-server/internal/gitrepo"
	"go-git-server/internal/render"
	"go-git-server/internal/store"
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
	cfg        Config
	store      *store.Store
	sessions   *sessionStore
	limiter    *limiter
	clones     *counter // HTTPS clones running per client (ipKey)
	pending    *pendingSignups
	totpSetups *totpSetups        // authenticators being set up (Settings -> two-factor)
	box        *account.SecretBox // encrypts TOTP secrets
	reposDir   string
	gitPath    string
	tmpl       map[string]*template.Template
	assetVer   string // changes whenever the stylesheets change (cache busting)
}

func NewServer(cfg Config) (*Server, error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	box, err := store.LoadSecretBox(cfg.DataDir, st)
	if err != nil {
		return nil, err
	}
	if err := store.EncryptTOTPSecrets(st, box); err != nil {
		return nil, err
	}
	s := &Server{
		cfg:        cfg,
		box:        box,
		store:      st,
		sessions:   newSessionStore(),
		limiter:    newLimiter(10, 15*time.Minute),
		clones:     &counter{m: make(map[string]int)},
		pending:    newPendingSignups(),
		totpSetups: &totpSetups{m: make(map[string]totpSetup)},
		reposDir:   filepath.Join(cfg.DataDir, "repos"),
		gitPath:    gitPath,
		tmpl:       make(map[string]*template.Template),
	}
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		return nil, err
	}
	s.assetVer = account.HashToken(string(css) + string(render.HighlightCSS()))[:12]
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
func (s *Server) cloneURL(r *http.Request, repo *gitrepo.Repo) string {
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
		http.Redirect(w, r, "/settings/profile", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /settings/profile", s.requireUser(s.handleProfile))
	mux.HandleFunc("POST /settings/profile", s.requireUser(s.handleWebsite))
	mux.HandleFunc("POST /settings/profile/avatar", s.requireUserLimit(render.MaxAvatarUpload+16<<10, s.handleAvatarUpload))
	mux.HandleFunc("POST /settings/profile/avatar/delete", s.requireUser(s.handleAvatarDelete))
	mux.HandleFunc("GET /settings/keys", s.requireUser(s.handleKeys))
	mux.HandleFunc("POST /settings/keys", s.requireUser(s.handleKeyAdd))
	mux.HandleFunc("POST /settings/keys/delete", s.requireUser(s.handleKeyDelete))
	mux.HandleFunc("GET /settings/password", s.requireUser(s.handlePassword))
	mux.HandleFunc("POST /settings/password", s.requireUser(s.handlePasswordChange))
	mux.HandleFunc("GET /settings/security", s.requireUser(s.handleSecurity))
	mux.HandleFunc("POST /settings/security/logout", s.requireUser(s.handleSessionLogout))
	mux.HandleFunc("POST /settings/security/logout-others", s.requireUser(s.handleLogoutOthers))
	mux.HandleFunc("POST /settings/account/delete", s.requireUser(s.handleAccountDelete))
	mux.HandleFunc("GET /settings/2fa", s.requireUser(s.handleTwoFactor))
	mux.HandleFunc("POST /settings/2fa/recovery", s.requireUser(s.handleRecoveryCodes))
	mux.HandleFunc("POST /settings/2fa/totp", s.requireUser(s.handleTOTPSetup))
	mux.HandleFunc("POST /settings/2fa/totp/confirm", s.requireUser(s.handleTOTPConfirm))
	mux.HandleFunc("GET /settings/invites", s.requireAdmin(s.handleInvites))
	mux.HandleFunc("POST /settings/invites", s.requireAdmin(s.handleInviteCreate))
	mux.HandleFunc("POST /settings/invites/revoke", s.requireAdmin(s.handleInviteRevoke))
	mux.HandleFunc("GET /settings/audit", s.requireUser(s.handleAudit))
	mux.HandleFunc("GET /settings/usage", s.requireAdmin(s.handleUsage))
	mux.HandleFunc("GET /avatars/{user}", s.handleAvatar)
	mux.HandleFunc("GET /{owner}", s.handleUserPage)
	mux.HandleFunc("GET /{owner}/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+url.PathEscape(r.PathValue("owner")), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /{owner}/{repo}/{rest...}", s.handleRepo)
	mux.HandleFunc("POST /{owner}/{repo}/settings", s.requireUser(s.handleRepoSettings))
	mux.HandleFunc("POST /{owner}/{repo}/delete", s.requireUser(s.handleRepoDelete))
	mux.HandleFunc("POST /{owner}/{repo}/rename", s.requireUser(s.handleRepoRename))
	mux.HandleFunc("POST /{owner}/{repo}/mirror/sync", s.requireUser(s.handleMirrorSync))
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
