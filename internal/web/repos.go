package web

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go-git-server/internal/gitrepo"
	"go-git-server/internal/render"
	"go-git-server/internal/sshgit"
)

const (
	logPageSize   = 50
	maxReadmeSize = 512 << 10
)

type createData struct {
	Name, Description, Error string
	Mirror                   string
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
		Mirror:      strings.TrimSpace(r.PostFormValue("mirror")),
	}
	if data.Mirror != "" {
		if _, err := gitrepo.ParseMirrorURL(data.Mirror); err != nil {
			data.Error = capitalize(err.Error()) + "."
			s.render(w, http.StatusBadRequest, "create", s.newPage(r, "New repository", data))
			return
		}
	}
	if repos, err := gitrepo.List(s.reposDir, u.Name); err != nil || len(repos) >= maxReposPerUser {
		data.Error = fmt.Sprintf("You have reached the limit of %d repositories. Delete one first, or ask the administrator.", maxReposPerUser)
		s.render(w, http.StatusBadRequest, "create", s.newPage(r, "New repository", data))
		return
	}
	if err := gitrepo.Create(s.reposDir, u.Name, data.Name, data.Description, data.Public); err != nil {
		data.Error = capitalize(err.Error()) + "."
		s.render(w, http.StatusBadRequest, "create", s.newPage(r, "New repository", data))
		return
	}
	if data.Mirror != "" {
		repo, err := gitrepo.Load(s.reposDir, u.Name, data.Name)
		if err == nil {
			err = gitrepo.SetMirror(repo, data.Mirror)
		}
		if err == nil {
			err = gitrepo.RequestMirrorSync(s.cfg.DataDir, repo)
		}
		if err != nil {
			log.Printf("mirror setup ~%s/%s: %v", u.Name, data.Name, err)
		}
	}
	log.Printf("repo created user=%q repo=~%s/%s public=%v mirror=%q", u.Name, u.Name, data.Name, data.Public, data.Mirror)
	http.Redirect(w, r, "/~"+u.Name+"/"+data.Name+"/", http.StatusSeeOther)
}

// ownedRepo loads the repository in the URL if the current user owns it.
func (s *Server) ownedRepo(w http.ResponseWriter, r *http.Request) *gitrepo.Repo {
	owner, _ := strings.CutPrefix(r.PathValue("owner"), "~")
	repo, err := gitrepo.Load(s.reposDir, owner, r.PathValue("repo"))
	if err != nil || !repo.IsOwner(currentUser(r)) {
		s.notFound(w, r)
		return nil
	}
	return repo
}

type repoSettingsData struct {
	Error, Notice string
	RenameError   string
	MirrorStatus  *gitrepo.MirrorStatus
}

// repoSettingsPage renders the settings of repo with its mirror status.
func (s *Server) repoSettingsPage(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo, status int, data repoSettingsData) {
	if repo.Mirror != "" {
		data.MirrorStatus = gitrepo.ReadMirrorStatus(repo.Dir)
	}
	s.render(w, status, "repo-settings", s.repoPage(r, repo, "settings", "Settings", data))
}

func (s *Server) handleRepoSettingsForm(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	if !repo.IsOwner(currentUser(r)) {
		s.notFound(w, r)
		return
	}
	data := repoSettingsData{}
	if r.URL.Query().Get("sync") == "1" {
		data.Notice = "Sync started. Reload this page in a minute to see the result."
	}
	if old := r.URL.Query().Get("renamed"); old != "" {
		data.Notice = "Renamed. Links to ~" + repo.Owner + "/" + old + " lead here now; update your clones: git remote set-url origin " + s.cloneURL(r, repo)
	}
	s.repoSettingsPage(w, r, repo, http.StatusOK, data)
}

func (s *Server) handleRepoSettings(w http.ResponseWriter, r *http.Request) {
	repo := s.ownedRepo(w, r)
	if repo == nil {
		return
	}
	desc := strings.TrimSpace(r.PostFormValue("description"))
	public := r.PostFormValue("visibility") == "public"
	protected, err := gitrepo.ParseProtected(r.PostFormValue("protected"))
	if err == nil {
		err = gitrepo.SetDescription(repo, desc)
	}
	if err == nil {
		err = gitrepo.SetPublic(s.reposDir, repo.Owner, repo.Name, public)
	}
	if err == nil {
		err = gitrepo.SetProtected(repo, protected)
	}
	mirror := strings.TrimSpace(r.PostFormValue("mirror"))
	if err == nil && mirror != repo.Mirror {
		if err = gitrepo.SetMirror(repo, mirror); err == nil && mirror != "" {
			repo.Mirror = mirror
			err = gitrepo.RequestMirrorSync(s.cfg.DataDir, repo)
		}
	}
	if err != nil {
		s.repoSettingsPage(w, r, repo, http.StatusBadRequest, repoSettingsData{Error: capitalize(err.Error()) + "."})
		return
	}
	log.Printf("repo settings user=%q repo=%s public=%v protected=%q", repo.Owner, repo.FullName(), public, protected)
	repo, _ = gitrepo.Load(s.reposDir, repo.Owner, repo.Name)
	s.repoSettingsPage(w, r, repo, http.StatusOK, repoSettingsData{Notice: "Settings saved."})
}

func (s *Server) handleRepoRename(w http.ResponseWriter, r *http.Request) {
	repo := s.ownedRepo(w, r)
	if repo == nil {
		return
	}
	newName := strings.TrimSpace(r.PostFormValue("name"))
	lock, err := sshgit.LockPush(repo)
	if err == nil {
		err = gitrepo.Rename(s.reposDir, repo, newName)
		lock.Close()
	}
	if err != nil {
		s.repoSettingsPage(w, r, repo, http.StatusBadRequest, repoSettingsData{RenameError: capitalize(err.Error()) + "."})
		return
	}
	log.Printf("repo renamed user=%q repo=%s to=%s", repo.Owner, repo.FullName(), newName)
	http.Redirect(w, r, "/~"+repo.Owner+"/"+newName+"/settings?renamed="+url.QueryEscape(repo.Name), http.StatusSeeOther)
}

func (s *Server) handleMirrorSync(w http.ResponseWriter, r *http.Request) {
	repo := s.ownedRepo(w, r)
	if repo == nil {
		return
	}
	if repo.Mirror == "" {
		s.notFound(w, r)
		return
	}
	if err := gitrepo.RequestMirrorSync(s.cfg.DataDir, repo); err != nil {
		log.Printf("mirror sync request %s: %v", repo.FullName(), err)
		s.repoSettingsPage(w, r, repo, http.StatusInternalServerError, repoSettingsData{Error: "Could not start the sync."})
		return
	}
	http.Redirect(w, r, repo.Path()+"/settings?sync=1", http.StatusSeeOther)
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
	if err := gitrepo.Delete(s.reposDir, repo); err != nil {
		log.Printf("delete %s: %v", repo.FullName(), err)
		s.error(w, r, http.StatusInternalServerError, "Could not delete the repository.")
		return
	}
	log.Printf("repo deleted user=%q repo=%s", repo.Owner, repo.FullName())
	http.Redirect(w, r, "/~"+repo.Owner, http.StatusSeeOther)
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
	repo, err := gitrepo.Load(s.reposDir, owner, r.PathValue("repo"))
	if err != nil {
		// A renamed repository: send those who may see it to the new name.
		if to, ok := gitrepo.Renamed(s.reposDir, owner, r.PathValue("repo")); ok && to.CanRead(currentUser(r)) && r.Method == http.MethodGet {
			target := to.Path() + "/" + r.PathValue("rest")
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			// #nosec G710 -- target starts with this server's /~owner/name path
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
	}
	if err != nil || !repo.CanRead(currentUser(r)) {
		s.notFound(w, r) // private repos are indistinguishable from missing ones
		return
	}
	rest := r.PathValue("rest")
	switch {
	case rest == "":
		s.handleSummary(w, r, repo)
	case rest == "log":
		s.handleLog(w, r, repo)
	case rest == "log.atom":
		s.handleLogFeed(w, r, repo)
	case rest == "tags.atom":
		s.handleTagsFeed(w, r, repo)
	case rest == "refs":
		s.handleRefs(w, r, repo)
	case rest == "compare":
		s.handleCompare(w, r, repo)
	case rest == "search":
		s.handleSearch(w, r, repo)
	case rest == "settings":
		s.handleRepoSettingsForm(w, r, repo)
	case rest == "tree" || strings.HasPrefix(rest, "tree/"):
		s.handleTree(w, r, repo, strings.TrimPrefix(strings.TrimPrefix(rest, "tree"), "/"))
	case strings.HasPrefix(rest, "archive/"):
		s.handleArchive(w, r, repo, strings.TrimPrefix(rest, "archive/"))
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
	Default    string // the default branch, for the download links
	Commits    []gitrepo.Commit
	Branches   int
	Tags       int
	ReadmeName string
	ReadmeHTML template.HTML
	ReadmeText string
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	ctx := r.Context()
	data := summaryData{}
	commit, err := gitrepo.ResolveCommit(ctx, repo.Dir, "")
	if err != nil {
		data.Empty = true
		s.render(w, http.StatusOK, "summary", s.repoPage(r, repo, "summary", "", data))
		return
	}
	if data.Commits, err = gitrepo.Log(ctx, repo.Dir, commit, "", 0, 3); err != nil {
		log.Printf("log %s: %v", repo.FullName(), err)
	}
	if branches, tags, err := gitrepo.Refs(ctx, repo.Dir); err == nil {
		data.Branches, data.Tags = len(branches), len(tags)
	}
	data.Default = gitrepo.DefaultBranch(ctx, repo.Dir)
	if readme, _ := gitrepo.SpecialFilesAt(ctx, repo.Dir, commit); readme != "" {
		data.ReadmeName = readme
		data.ReadmeHTML, data.ReadmeText = s.readme(ctx, repo, commit, readme, "", "")
	}
	s.render(w, http.StatusOK, "summary", s.repoPage(r, repo, "summary", "", data))
}

// readme renders the README at file in commit, in the folder dir: as HTML
// if it is Markdown, otherwise as text. Links in it stay on the branch or
// tag in query. Both are empty if it is too large or unreadable.
func (s *Server) readme(ctx context.Context, repo *gitrepo.Repo, commit, file, dir, query string) (template.HTML, string) {
	size, err := gitrepo.BlobSize(ctx, repo.Dir, commit, file)
	if err != nil || size > maxReadmeSize {
		return "", ""
	}
	content, err := gitrepo.BlobContent(ctx, repo.Dir, commit, file)
	if err != nil {
		return "", ""
	}
	if ext := strings.ToLower(path.Ext(file)); ext == ".md" || ext == ".markdown" {
		if html, err := render.MarkdownAt(content, repo.Path()+"/tree/", repo.Path()+"/raw/", dir, query); err == nil && html != "" {
			return html, ""
		}
	}
	return "", string(content)
}

type logData struct {
	Ref     string
	Feed    string
	Commits []gitrepo.Commit
	Prev    string
	Next    string
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	ctx := r.Context()
	ref := r.URL.Query().Get("h")
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageNum = max(1, min(pageNum, 100000))

	commit, err := gitrepo.ResolveCommit(ctx, repo.Dir, ref)
	if err != nil {
		if ref != "" {
			s.notFound(w, r)
			return
		}
		http.Redirect(w, r, repo.Path()+"/", http.StatusSeeOther)
		return
	}
	commits, err := gitrepo.Log(ctx, repo.Dir, commit, "", (pageNum-1)*logPageSize, logPageSize+1)
	if err != nil {
		log.Printf("log %s: %v", repo.FullName(), err)
		s.error(w, r, http.StatusInternalServerError, "Could not read the log.")
		return
	}
	data := logData{Ref: ref, Commits: commits, Feed: repo.Path() + "/log.atom" + refQuery(ref)}
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
	Last    *gitrepo.Commit
	Entries []treeEntry
	// The folder's README, shown below its files.
	ReadmeName string
	ReadmeHTML template.HTML
	ReadmeText string
	// Blob view
	IsBlob   bool
	Name     string
	Lines    []codeLine
	Binary   bool
	TooLarge bool
	Size     int64
	Raw      string
	// Selected lines (?lines=A or A-B); 0 if none.
	SelFrom, SelTo int
	Unselect       string // the page without the selection
}

// codeLine is one line of a file: its number, a link from the number (see
// lineLinks), and whether it is selected.
type codeLine struct {
	N        int
	HTML     template.HTML
	Href     string
	Selected bool
}

// parseLines reads a "?lines=" selection: "12" or "12-20" (either order),
// within 1..n. ok is false for anything else.
func parseLines(s string, n int) (from, to int, ok bool) {
	a, b, isRange := strings.Cut(s, "-")
	from, err1 := strconv.Atoi(a)
	to, err2 := from, error(nil)
	if isRange {
		to, err2 = strconv.Atoi(b)
	}
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	from, to = min(from, to), max(from, to)
	if from < 1 || to > n {
		return 0, 0, false
	}
	return from, to, true
}

// lineLinks makes the line numbers select lines without JavaScript: with
// nothing or a range selected, a number selects its line; with one line
// selected, another number selects the range between them, and the
// selected line's own number clears the selection. The fragment scrolls
// to the selection.
func lineLinks(lines []template.HTML, page string, from, to int) []codeLine {
	sep := "?"
	if strings.Contains(page, "?") {
		sep = "&"
	}
	out := make([]codeLine, len(lines))
	for i, html := range lines {
		n := i + 1
		href := fmt.Sprintf("%s%slines=%d#L%d", page, sep, n, n)
		if from != 0 && from == to {
			switch {
			case n == from:
				href = fmt.Sprintf("%s#L%d", page, n)
			case n < from:
				href = fmt.Sprintf("%s%slines=%d-%d#L%d", page, sep, n, from, n)
			default:
				href = fmt.Sprintf("%s%slines=%d-%d#L%d", page, sep, from, n, from)
			}
		}
		out[i] = codeLine{N: n, HTML: html, Href: href, Selected: n >= from && n <= to}
	}
	return out
}

type treeEntry struct {
	Mode, Name, Href string
	Size             int64
	IsDir, IsFile    bool
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo, rawPath string) {
	ctx := r.Context()
	ref := r.URL.Query().Get("h")
	p, ok := cleanTreePath(rawPath)
	if !ok {
		s.notFound(w, r)
		return
	}
	commit, err := gitrepo.ResolveCommit(ctx, repo.Dir, ref)
	if err != nil {
		if ref == "" && p == "" {
			http.Redirect(w, r, repo.Path()+"/", http.StatusSeeOther)
			return
		}
		s.notFound(w, r)
		return
	}
	typ, err := gitrepo.ObjectType(ctx, repo.Dir, commit, p)
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
			data.Crumbs = append(data.Crumbs, crumb{segs[i], base + render.EscapePath(strings.Join(segs[:i+1], "/")) + q})
		}
	}
	if last, err := gitrepo.Log(ctx, repo.Dir, commit, p, 0, 1); err == nil && len(last) == 1 {
		data.Last = &last[0]
	}

	switch typ {
	case "tree":
		entries, err := gitrepo.Tree(ctx, repo.Dir, commit, p)
		if err != nil {
			s.notFound(w, r)
			return
		}
		for _, e := range entries {
			full := e.Name
			if p != "" {
				full = p + "/" + e.Name
			}
			if e.Type == "blob" && data.ReadmeName == "" && gitrepo.IsReadme(e.Name) {
				data.ReadmeName = full
			}
			te := treeEntry{Mode: fileMode(e.Mode), Name: e.Name, Size: e.Size, IsDir: e.Type == "tree", IsFile: e.Type == "blob"}
			if e.Type != "commit" { // submodules have nothing to show
				te.Href = base + render.EscapePath(full) + q
			}
			data.Entries = append(data.Entries, te)
		}
		if data.ReadmeName != "" {
			query := ""
			if ref != "" {
				query = url.Values{"h": {ref}}.Encode()
			}
			data.ReadmeHTML, data.ReadmeText = s.readme(ctx, repo, commit, data.ReadmeName, p, query)
		}
	case "blob":
		data.IsBlob = true
		data.Name = path.Base(p)
		data.Raw = repo.Path() + "/raw/" + render.EscapePath(p) + q
		data.Size, err = gitrepo.BlobSize(ctx, repo.Dir, commit, p)
		if err != nil {
			s.notFound(w, r)
			return
		}
		if data.Size > gitrepo.MaxBlobView {
			data.TooLarge = true
			break
		}
		content, err := gitrepo.BlobContent(ctx, repo.Dir, commit, p)
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
			lines := render.HighlightLines(render.LexerFor(p, text), text, time.Now().Add(render.HighlightBudget))
			if lines == nil {
				lines = render.PlainLines(text)
			}
			if sel := r.URL.Query().Get("lines"); sel != "" {
				data.SelFrom, data.SelTo, _ = parseLines(sel, len(lines))
			}
			page := base + render.EscapePath(p) + q
			data.Unselect = page
			data.Lines = lineLinks(lines, page, data.SelFrom, data.SelTo)
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

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo, rawPath string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	p, ok := cleanTreePath(rawPath)
	if !ok || p == "" {
		s.notFound(w, r)
		return
	}
	commit, err := gitrepo.ResolveCommit(ctx, repo.Dir, r.URL.Query().Get("h"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if typ, err := gitrepo.ObjectType(ctx, repo.Dir, commit, p); err != nil || typ != "blob" {
		s.notFound(w, r)
		return
	}
	size, err := gitrepo.BlobSize(ctx, repo.Dir, commit, p)
	if err != nil {
		s.notFound(w, r)
		return
	}
	release, err := gitrepo.AcquireGit(ctx)
	if err != nil {
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}
	defer release()
	// Never let a repository file render as active content on this origin.
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Content-Type", render.RawContentType(p))
	// An SVG opened directly (not as an <img>) is downloaded rather than
	// shown, so a repository can't put a look-alike page on this domain.
	if dest := r.Header.Get("Sec-Fetch-Dest"); path.Ext(strings.ToLower(p)) == ".svg" && dest != "" && dest != "image" {
		h.Set("Content-Disposition", "attachment")
	}
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	if !repo.Public {
		h.Set("Cache-Control", "no-store")
	}
	cmd := gitrepo.Command(ctx, repo.Dir, "cat-file", "blob", commit+":"+p)
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		log.Printf("raw %s/%s: %v", repo.FullName(), p, err)
	}
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo, hash string) {
	ctx := r.Context()
	if !gitrepo.HashRe.MatchString(hash) {
		s.notFound(w, r)
		return
	}
	full, err := gitrepo.ResolveCommit(ctx, repo.Dir, hash)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if full != hash { // short hash: the page lives at the full one
		http.Redirect(w, r, repo.Path()+"/commit/"+full, http.StatusMovedPermanently)
		return
	}
	info, err := gitrepo.ReadCommit(ctx, repo.Dir, full)
	if err != nil {
		s.notFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "commit", s.repoPage(r, repo, "log", info.Hash[:8], info))
}

type refsData struct {
	Branches, Tags []gitrepo.Ref
	Default        string
}

func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	branches, tags, err := gitrepo.Refs(r.Context(), repo.Dir)
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not read refs.")
		return
	}
	data := refsData{branches, tags, gitrepo.DefaultBranch(r.Context(), repo.Dir)}
	s.render(w, http.StatusOK, "refs", s.repoPage(r, repo, "refs", "Refs", data))
}
