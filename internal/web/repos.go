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
)

const (
	logPageSize   = 50
	maxReadmeSize = 512 << 10
)

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
	log.Printf("repo created user=%q repo=~%s/%s public=%v", u.Name, u.Name, data.Name, data.Public)
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
}

func (s *Server) handleRepoSettingsForm(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	if !repo.IsOwner(currentUser(r)) {
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
	err := gitrepo.SetDescription(repo, desc)
	if err == nil {
		err = gitrepo.SetPublic(s.reposDir, repo.Owner, repo.Name, public)
	}
	if err != nil {
		s.render(w, http.StatusBadRequest, "repo-settings", s.repoPage(r, repo, "settings", "Settings", repoSettingsData{Error: capitalize(err.Error()) + "."}))
		return
	}
	log.Printf("repo settings user=%q repo=%s public=%v", repo.Owner, repo.FullName(), public)
	repo, _ = gitrepo.Load(s.reposDir, repo.Owner, repo.Name)
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
	if readme, _ := gitrepo.SpecialFilesAt(ctx, repo.Dir, commit); readme != "" {
		data.ReadmeName = readme
		if size, err := gitrepo.BlobSize(ctx, repo.Dir, commit, readme); err == nil && size <= maxReadmeSize {
			if content, err := gitrepo.BlobContent(ctx, repo.Dir, commit, readme); err == nil {
				ext := strings.ToLower(path.Ext(readme))
				var html template.HTML
				var err error
				if ext == ".md" || ext == ".markdown" {
					html, err = render.Markdown(content, repo.Path()+"/tree/", repo.Path()+"/raw/")
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
			te := treeEntry{Mode: fileMode(e.Mode), Name: e.Name, Size: e.Size, IsDir: e.Type == "tree", IsFile: e.Type == "blob"}
			if e.Type != "commit" { // submodules have nothing to show
				te.Href = base + render.EscapePath(full) + q
			}
			data.Entries = append(data.Entries, te)
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
