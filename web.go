package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	logPageSize   = 50
	maxReadmeSize = 512 << 10
)

var templateFuncs = template.FuncMap{
	"date":     func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	"datefull": func(t time.Time) string { return t.Format("Mon, 02 Jan 2006 15:04:05 -0700") },
	"ago":      timeAgo,
	"short":    func(h string) string { return h[:min(len(h), 8)] },
	"inc":      func(i int) int { return i + 1 },
	"size":     humanSize,
	"ev":       func(repo string, c Commit) eventData { return eventData{repo, c} },
}

type eventData struct {
	Repo string
	C    Commit
}

// page is the data passed to every template.
type page struct {
	Site     string
	AssetVer string
	Title    string
	Path     string
	User     *User
	CSRF     string
	Tab      string
	Repo     *Repo
	Clone    string // SSH clone URL
	CloneWeb string // HTTPS clone URL (public repositories only)
	IsOwner  bool
	Data     any
}

func (s *Server) newPage(r *http.Request, title string, data any) *page {
	p := &page{Site: s.cfg.SiteName, AssetVer: s.assetVer, Title: title, Path: r.URL.RequestURI(), User: currentUser(r), Data: data}
	if sess := currentSession(r); sess != nil {
		p.CSRF = sess.csrf
	}
	return p
}

func (s *Server) repoPage(r *http.Request, repo *Repo, tab, title string, data any) *page {
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
	p.IsOwner = repo.isOwner(p.User)
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
	w.Write(highlightCSS())
}

type landingData struct {
	Intro template.HTML
	Repos []*Repo
}

const defaultIntro = "Self-hosted git repositories. Browse the public projects below, or log in to see everything you have access to."

// intro renders <data>/intro.md for the landing page.
func (s *Server) intro() template.HTML {
	src := readSmallFileN(filepath.Join(s.cfg.DataDir, "intro.md"), 64<<10)
	if src == "" {
		return template.HTML("<p>" + template.HTMLEscapeString(defaultIntro) + "</p>")
	}
	html, err := renderMarkdown([]byte(src), "", "")
	if err != nil {
		return ""
	}
	return html
}

// visibleRepos lists the repositories user may see (all owners, or one).
func (s *Server) visibleRepos(r *http.Request, owner string) ([]*Repo, error) {
	user := currentUser(r)
	repos, err := listRepos(s.reposDir, owner)
	if err != nil {
		return nil, err
	}
	visible := repos[:0]
	for _, repo := range repos {
		if repo.canRead(user) {
			repo.Updated = lastChange(r.Context(), repo.Dir)
			visible = append(visible, repo)
		}
	}
	slices.SortStableFunc(visible, func(a, b *Repo) int { return b.Updated.Compare(a.Updated) })
	return visible, nil
}

type indexData struct {
	Owner string // set on a user page
	Repos []*Repo
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	repos, err := s.visibleRepos(r, "")
	if err != nil {
		log.Printf("list repos: %v", err)
		s.error(w, r, http.StatusInternalServerError, "Could not list repositories.")
		return
	}
	if currentUser(r) == nil {
		p := s.newPage(r, "", landingData{Intro: s.intro(), Repos: repos})
		p.Tab = "landing"
		s.render(w, http.StatusOK, "landing", p)
		return
	}
	p := s.newPage(r, "", indexData{Repos: repos})
	p.Tab = "repos"
	s.render(w, http.StatusOK, "index", p)
}

// handleUserPage lists the repositories of ~owner that the viewer may see.
func (s *Server) handleUserPage(w http.ResponseWriter, r *http.Request) {
	owner, ok := strings.CutPrefix(r.PathValue("owner"), "~")
	if !ok || !userNameRe.MatchString(owner) {
		s.notFound(w, r)
		return
	}
	if _, err := s.store.Get(owner); err != nil {
		s.notFound(w, r)
		return
	}
	repos, err := s.visibleRepos(r, owner)
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not list repositories.")
		return
	}
	p := s.newPage(r, "~"+owner, indexData{Owner: owner, Repos: repos})
	p.Tab = "repos"
	s.render(w, http.StatusOK, "index", p)
}

// safeNext only allows redirects to local paths.
func safeNext(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") {
		return "/"
	}
	// Browsers drop tabs and newlines and treat \ like /, so "/\t/evil.com"
	// would become "//evil.com". Reject all control characters and backslashes.
	if strings.ContainsFunc(next, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' }) {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return next
}

type loginData struct {
	Next  string
	Error string
	Name  string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if currentUser(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", s.newPage(r, "Log in", loginData{Next: next}))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	name := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	code := r.PostFormValue("code")
	next := safeNext(r.PostFormValue("next"))
	ip := s.clientIP(r)
	ipKey, userKey := ipKey(ip), "user:"+name

	fail := func(status int, msg string) {
		s.render(w, status, "login", s.newPage(r, "Log in", loginData{Next: next, Error: msg, Name: name}))
	}
	// The attempt counts against the IP before the password is hashed, so
	// parallel requests cannot all slip past the limit; success refunds it.
	if s.limiter.blocked(userKey) || !s.limiter.take(ipKey) {
		fail(http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	u, passwordOK, err := s.authenticate(name, password, code)
	if errors.Is(err, errHashBusy) {
		s.limiter.refund(ipKey)
		fail(http.StatusServiceUnavailable, capitalize(err.Error())+".")
		return
	}
	if u == nil {
		// Only wrong codes after a correct password count against the
		// account. Wrong passwords count per IP only, so a stranger who
		// types random passwords cannot lock the real user out.
		if passwordOK {
			s.limiter.fail(userKey)
			if !errors.Is(err, errCodeReused) {
				s.alerts.add(name)
			}
		}
		log.Printf("login failed user=%q ip=%s password_ok=%v", name, ip, passwordOK)
		fail(http.StatusUnauthorized, "Invalid username, password or code.")
		return
	}
	s.limiter.refund(ipKey)
	s.limiter.reset(userKey)
	var notice string
	if a := s.alerts.take(u.Name); a.n > 0 {
		notice = fmt.Sprintf("Since %s, %d wrong 2FA code(s) were entered together with your correct password. "+
			"If that was not you, someone knows your password: change it now.", a.since.UTC().Format("2006-01-02 15:04 MST"), a.n)
		next = "/settings/password"
		log.Printf("login ok user=%q ip=%s after %d wrong code(s) with the correct password", name, ip, a.n)
	} else {
		log.Printf("login ok user=%q ip=%s", name, ip)
	}
	s.startSession(w, r, u, notice)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// authenticate checks password and TOTP code and returns the user on
// success. passwordOK reports whether the password alone was right. A TOTP
// code is only consumed once the password has been verified. The error is
// errHashBusy when the server is too busy to check the password, or
// errCodeReused when the code was right but already used.
func (s *Server) authenticate(name, password, code string) (u *User, passwordOK bool, err error) {
	if len(password) > 1024 {
		return nil, false, nil
	}
	u, err = s.store.Get(name)
	if err != nil {
		_, err := checkPassword(dummyHash(), password)
		return nil, false, err
	}
	if ok, err := checkPassword(u.PasswordHash, password); !ok {
		return nil, false, err
	}
	err = s.store.Update(name, func(stored *User) error {
		secret, err := s.box.openTOTP(stored.Name, stored.TOTPSecret)
		if err != nil {
			log.Printf("login: user=%q: %v", stored.Name, err)
			return err
		}
		now := time.Now()
		step, ok := checkTOTP(secret, code, stored.TOTPLast, now)
		if !ok {
			if _, valid := checkTOTP(secret, code, 0, now); valid {
				return errCodeReused
			}
			return errors.New("bad code")
		}
		stored.TOTPLast = step
		u = stored
		return nil
	})
	if errors.Is(err, errCodeReused) {
		return nil, true, err
	}
	if err != nil {
		return nil, true, nil
	}
	return u, true, nil
}

// errCodeReused: the code is valid but was already used, e.g. a form sent
// twice. It fails like any wrong code but is no sign of an attack.
var errCodeReused = errors.New("2FA code already used")

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if currentUser(r) != nil && !s.validCSRF(r) {
		s.error(w, r, http.StatusForbidden, "Invalid or missing CSRF token.")
		return
	}
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		Secure: !s.cfg.Insecure, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type keysData struct {
	Keys   []SSHKey
	Error  string
	Notice string
	Input  string
	Host   string
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request, status int, data keysData) {
	u, err := s.store.Get(currentUser(r).Name)
	if err != nil {
		s.notFound(w, r)
		return
	}
	data.Keys = u.SSHKeys
	data.Host = s.sshHost(r)
	p := s.newPage(r, "SSH keys", data)
	p.Tab = "keys"
	s.render(w, status, "keys", p)
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	s.keysPage(w, r, http.StatusOK, keysData{})
}

func (s *Server) handleKeyAdd(w http.ResponseWriter, r *http.Request) {
	input := r.PostFormValue("key")
	key, err := parseSSHKey(input)
	if err == nil {
		err = s.store.AddSSHKey(currentUser(r).Name, key)
	}
	if err != nil {
		s.keysPage(w, r, http.StatusBadRequest, keysData{Error: capitalize(err.Error()) + ".", Input: input})
		return
	}
	log.Printf("ssh key added user=%q fingerprint=%s", currentUser(r).Name, key.Fingerprint)
	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSSHKey(currentUser(r).Name, r.PostFormValue("id")); err != nil {
		s.keysPage(w, r, http.StatusBadRequest, keysData{Error: capitalize(err.Error()) + "."})
		return
	}
	log.Printf("ssh key deleted user=%q", currentUser(r).Name)
	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

type createData struct {
	Name, Description, Error string
	Public                   bool
}

func (s *Server) handleCreateForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "create", s.newPage(r, "New repository", createData{}))
}

// maxReposPerUser caps repositories created in the web UI (the CLI is not limited).
const maxReposPerUser = 100

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	data := createData{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Public:      r.PostFormValue("visibility") == "public",
	}
	if repos, err := listRepos(s.reposDir, u.Name); err != nil || len(repos) >= maxReposPerUser {
		data.Error = fmt.Sprintf("You have reached the limit of %d repositories. Delete one first, or ask the administrator.", maxReposPerUser)
		s.render(w, http.StatusBadRequest, "create", s.newPage(r, "New repository", data))
		return
	}
	if err := createRepo(s.reposDir, u.Name, data.Name, data.Description, data.Public); err != nil {
		data.Error = capitalize(err.Error()) + "."
		s.render(w, http.StatusBadRequest, "create", s.newPage(r, "New repository", data))
		return
	}
	log.Printf("repo created user=%q repo=~%s/%s public=%v", u.Name, u.Name, data.Name, data.Public)
	http.Redirect(w, r, "/~"+u.Name+"/"+data.Name+"/", http.StatusSeeOther)
}

// ownedRepo loads the repository in the URL if the current user owns it.
func (s *Server) ownedRepo(w http.ResponseWriter, r *http.Request) *Repo {
	owner, _ := strings.CutPrefix(r.PathValue("owner"), "~")
	repo, err := loadRepo(s.reposDir, owner, r.PathValue("repo"))
	if err != nil || !repo.isOwner(currentUser(r)) {
		s.notFound(w, r)
		return nil
	}
	return repo
}

type repoSettingsData struct {
	Error, Notice string
}

func (s *Server) handleRepoSettingsForm(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if !repo.isOwner(currentUser(r)) {
		s.notFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "repo-settings", s.repoPage(r, repo, "settings", "Settings", repoSettingsData{}))
}

func (s *Server) handleRepoSettings(w http.ResponseWriter, r *http.Request) {
	repo := s.ownedRepo(w, r)
	if repo == nil {
		return
	}
	desc := strings.TrimSpace(r.PostFormValue("description"))
	public := r.PostFormValue("visibility") == "public"
	err := setRepoDescription(repo, desc)
	if err == nil {
		err = setRepoPublic(s.reposDir, repo.Owner, repo.Name, public)
	}
	if err != nil {
		s.render(w, http.StatusBadRequest, "repo-settings", s.repoPage(r, repo, "settings", "Settings", repoSettingsData{Error: capitalize(err.Error()) + "."}))
		return
	}
	log.Printf("repo settings user=%q repo=%s public=%v", repo.Owner, repo.FullName(), public)
	repo, _ = loadRepo(s.reposDir, repo.Owner, repo.Name)
	s.render(w, http.StatusOK, "repo-settings", s.repoPage(r, repo, "settings", "Settings", repoSettingsData{Notice: "Settings saved."}))
}

func (s *Server) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	repo := s.ownedRepo(w, r)
	if repo == nil {
		return
	}
	if r.PostFormValue("confirm") != repo.Name {
		s.render(w, http.StatusBadRequest, "repo-settings", s.repoPage(r, repo, "settings", "Settings",
			repoSettingsData{Error: "Type the repository name exactly to confirm deletion."}))
		return
	}
	if err := deleteRepo(s.reposDir, repo); err != nil {
		log.Printf("delete %s: %v", repo.FullName(), err)
		s.error(w, r, http.StatusInternalServerError, "Could not delete the repository.")
		return
	}
	log.Printf("repo deleted user=%q repo=%s", repo.Owner, repo.FullName())
	http.Redirect(w, r, "/~"+repo.Owner, http.StatusSeeOther)
}

// escapePath escapes each segment of a slash-separated path for use in URLs.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// cleanTreePath rejects anything that is not a plain relative path.
func cleanTreePath(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", true
	}
	if strings.ContainsRune(p, 0) {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}

func refQuery(ref string) string {
	if ref == "" {
		return ""
	}
	return "?" + url.Values{"h": {ref}}.Encode()
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	owner, ok := strings.CutPrefix(r.PathValue("owner"), "~")
	if !ok {
		s.notFound(w, r)
		return
	}
	repo, err := loadRepo(s.reposDir, owner, r.PathValue("repo"))
	if err != nil || !repo.canRead(currentUser(r)) {
		s.notFound(w, r) // private repos are indistinguishable from missing ones
		return
	}
	rest := r.PathValue("rest")
	switch {
	case rest == "":
		s.handleSummary(w, r, repo)
	case rest == "log":
		s.handleLog(w, r, repo)
	case rest == "refs":
		s.handleRefs(w, r, repo)
	case rest == "settings":
		s.handleRepoSettingsForm(w, r, repo)
	case rest == "tree" || strings.HasPrefix(rest, "tree/"):
		s.handleTree(w, r, repo, strings.TrimPrefix(strings.TrimPrefix(rest, "tree"), "/"))
	case strings.HasPrefix(rest, "raw/"):
		s.handleRaw(w, r, repo, strings.TrimPrefix(rest, "raw/"))
	case strings.HasPrefix(rest, "commit/"):
		s.handleCommit(w, r, repo, strings.TrimPrefix(rest, "commit/"))
	default:
		s.notFound(w, r)
	}
}

type summaryData struct {
	Empty      bool
	Commits    []Commit
	Branches   int
	Tags       int
	ReadmeName string
	ReadmeHTML template.HTML
	ReadmeText string
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request, repo *Repo) {
	ctx := r.Context()
	data := summaryData{}
	commit, err := resolveCommit(ctx, repo.Dir, "")
	if err != nil {
		data.Empty = true
		s.render(w, http.StatusOK, "summary", s.repoPage(r, repo, "summary", "", data))
		return
	}
	if data.Commits, err = gitLog(ctx, repo.Dir, commit, "", 0, 3); err != nil {
		log.Printf("log %s: %v", repo.FullName(), err)
	}
	if branches, tags, err := gitRefs(ctx, repo.Dir); err == nil {
		data.Branches, data.Tags = len(branches), len(tags)
	}
	if readme, _ := specialFilesAt(ctx, repo.Dir, commit); readme != "" {
		data.ReadmeName = readme
		if size, err := blobSize(ctx, repo.Dir, commit, readme); err == nil && size <= maxReadmeSize {
			if content, err := blobContent(ctx, repo.Dir, commit, readme); err == nil {
				ext := strings.ToLower(path.Ext(readme))
				var html template.HTML
				var err error
				if ext == ".md" || ext == ".markdown" {
					html, err = renderMarkdown(content, repo.Path()+"/tree/", repo.Path()+"/raw/")
				}
				if html != "" && err == nil {
					data.ReadmeHTML = html
				} else {
					data.ReadmeText = string(content)
				}
			}
		}
	}
	s.render(w, http.StatusOK, "summary", s.repoPage(r, repo, "summary", "", data))
}

type logData struct {
	Ref     string
	Commits []Commit
	Prev    string
	Next    string
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, repo *Repo) {
	ctx := r.Context()
	ref := r.URL.Query().Get("h")
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageNum = max(1, min(pageNum, 100000))

	commit, err := resolveCommit(ctx, repo.Dir, ref)
	if err != nil {
		if ref != "" {
			s.notFound(w, r)
			return
		}
		http.Redirect(w, r, repo.Path()+"/", http.StatusSeeOther)
		return
	}
	commits, err := gitLog(ctx, repo.Dir, commit, "", (pageNum-1)*logPageSize, logPageSize+1)
	if err != nil {
		log.Printf("log %s: %v", repo.FullName(), err)
		s.error(w, r, http.StatusInternalServerError, "Could not read the log.")
		return
	}
	data := logData{Ref: ref, Commits: commits}
	link := func(p int) string {
		q := url.Values{}
		if ref != "" {
			q.Set("h", ref)
		}
		if p > 1 {
			q.Set("page", strconv.Itoa(p))
		}
		if len(q) == 0 {
			return repo.Path() + "/log"
		}
		return repo.Path() + "/log?" + q.Encode()
	}
	if len(commits) > logPageSize {
		data.Commits = commits[:logPageSize]
		data.Next = link(pageNum + 1)
	}
	if pageNum > 1 {
		data.Prev = link(pageNum - 1)
	}
	s.render(w, http.StatusOK, "log", s.repoPage(r, repo, "log", "Log", data))
}

type crumb struct{ Name, Href string }

type treeData struct {
	Ref     string
	Path    string
	Crumbs  []crumb
	Last    *Commit
	Entries []treeEntry
	// Blob view
	IsBlob   bool
	Name     string
	Lines    []template.HTML
	Binary   bool
	TooLarge bool
	Size     int64
	Raw      string
}

type treeEntry struct {
	Mode, Name, Href string
	Size             int64
	IsDir, IsFile    bool
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request, repo *Repo, rawPath string) {
	ctx := r.Context()
	ref := r.URL.Query().Get("h")
	p, ok := cleanTreePath(rawPath)
	if !ok {
		s.notFound(w, r)
		return
	}
	commit, err := resolveCommit(ctx, repo.Dir, ref)
	if err != nil {
		if ref == "" && p == "" {
			http.Redirect(w, r, repo.Path()+"/", http.StatusSeeOther)
			return
		}
		s.notFound(w, r)
		return
	}
	typ, err := objectType(ctx, repo.Dir, commit, p)
	if err != nil {
		s.notFound(w, r)
		return
	}
	data := treeData{Ref: ref, Path: p}
	base := repo.Path() + "/tree/"
	q := refQuery(ref)
	data.Crumbs = []crumb{{repo.Name, base + q}}
	if p != "" {
		segs := strings.Split(p, "/")
		for i := range segs {
			data.Crumbs = append(data.Crumbs, crumb{segs[i], base + escapePath(strings.Join(segs[:i+1], "/")) + q})
		}
	}
	if last, err := gitLog(ctx, repo.Dir, commit, p, 0, 1); err == nil && len(last) == 1 {
		data.Last = &last[0]
	}

	switch typ {
	case "tree":
		entries, err := gitTree(ctx, repo.Dir, commit, p)
		if err != nil {
			s.notFound(w, r)
			return
		}
		for _, e := range entries {
			full := e.Name
			if p != "" {
				full = p + "/" + e.Name
			}
			te := treeEntry{Mode: fileMode(e.Mode), Name: e.Name, Size: e.Size, IsDir: e.Type == "tree", IsFile: e.Type == "blob"}
			if e.Type != "commit" { // submodules have nothing to show
				te.Href = base + escapePath(full) + q
			}
			data.Entries = append(data.Entries, te)
		}
	case "blob":
		data.IsBlob = true
		data.Name = path.Base(p)
		data.Raw = repo.Path() + "/raw/" + escapePath(p) + q
		data.Size, err = blobSize(ctx, repo.Dir, commit, p)
		if err != nil {
			s.notFound(w, r)
			return
		}
		if data.Size > maxBlobView {
			data.TooLarge = true
			break
		}
		content, err := blobContent(ctx, repo.Dir, commit, p)
		if err != nil {
			s.error(w, r, http.StatusInternalServerError, "Could not read the file.")
			return
		}
		if bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
			data.Binary = true
			break
		}
		text := strings.TrimSuffix(string(content), "\n")
		if text != "" {
			data.Lines = highlightLines(lexerFor(p, text), text, time.Now().Add(highlightBudget))
			if data.Lines == nil {
				data.Lines = plainLines(text)
			}
		}
	default:
		s.notFound(w, r)
		return
	}
	title := "Tree"
	if p != "" {
		title = p
	}
	s.render(w, http.StatusOK, "tree", s.repoPage(r, repo, "tree", title, data))
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request, repo *Repo, rawPath string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	p, ok := cleanTreePath(rawPath)
	if !ok || p == "" {
		s.notFound(w, r)
		return
	}
	commit, err := resolveCommit(ctx, repo.Dir, r.URL.Query().Get("h"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if typ, err := objectType(ctx, repo.Dir, commit, p); err != nil || typ != "blob" {
		s.notFound(w, r)
		return
	}
	size, err := blobSize(ctx, repo.Dir, commit, p)
	if err != nil {
		s.notFound(w, r)
		return
	}
	release, err := acquireGit(ctx)
	if err != nil {
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}
	defer release()
	// Never let a repository file render as active content on this origin.
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Content-Type", rawContentType(p))
	// An SVG opened directly (not as an <img>) is downloaded rather than
	// shown, so a repository can't put a look-alike page on this domain.
	if dest := r.Header.Get("Sec-Fetch-Dest"); path.Ext(strings.ToLower(p)) == ".svg" && dest != "" && dest != "image" {
		h.Set("Content-Disposition", "attachment")
	}
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	if !repo.Public {
		h.Set("Cache-Control", "no-store")
	}
	cmd := gitCmd(ctx, repo.Dir, "cat-file", "blob", commit+":"+p)
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		log.Printf("raw %s/%s: %v", repo.FullName(), p, err)
	}
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, repo *Repo, hash string) {
	ctx := r.Context()
	if !hashRe.MatchString(hash) {
		s.notFound(w, r)
		return
	}
	full, err := resolveCommit(ctx, repo.Dir, hash)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if full != hash { // short hash: the page lives at the full one
		http.Redirect(w, r, repo.Path()+"/commit/"+full, http.StatusMovedPermanently)
		return
	}
	info, err := gitCommit(ctx, repo.Dir, full)
	if err != nil {
		s.notFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "commit", s.repoPage(r, repo, "log", info.Hash[:8], info))
}

type refsData struct {
	Branches, Tags []Ref
}

func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	branches, tags, err := gitRefs(r.Context(), repo.Dir)
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not read refs.")
		return
	}
	s.render(w, http.StatusOK, "refs", s.repoPage(r, repo, "refs", "Refs", refsData{branches, tags}))
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
