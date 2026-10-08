package web

import (
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"go-git-server/internal/account"
	"go-git-server/internal/gitrepo"
	"go-git-server/internal/render"
)

type landingData struct {
	Intro template.HTML
	Repos []*gitrepo.Repo
}

const defaultIntro = "Self-hosted git repositories. Browse the public projects below, or log in to see everything you have access to."

// intro renders <data>/intro.md for the landing page.
func (s *Server) intro() template.HTML {
	src := gitrepo.ReadSmallFileN(filepath.Join(s.cfg.DataDir, "intro.md"), 64<<10)
	if src == "" {
		return template.HTML("<p>" + template.HTMLEscapeString(defaultIntro) + "</p>")
	}
	html, err := render.Markdown([]byte(src), "", "")
	if err != nil {
		return ""
	}
	return html
}

// visibleRepos lists the repositories user may see (all owners, or one).
func (s *Server) visibleRepos(r *http.Request, owner string) ([]*gitrepo.Repo, error) {
	user := currentUser(r)
	repos, err := gitrepo.List(s.reposDir, owner)
	if err != nil {
		return nil, err
	}
	visible := repos[:0]
	for _, repo := range repos {
		if repo.CanRead(user) {
			repo.Updated = gitrepo.LastChange(r.Context(), repo.Dir)
			visible = append(visible, repo)
		}
	}
	slices.SortStableFunc(visible, func(a, b *gitrepo.Repo) int { return b.Updated.Compare(a.Updated) })
	return visible, nil
}

type indexData struct {
	Owner string // set on a user page
	Repos []*gitrepo.Repo
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
	if !ok || !account.UserNameRe.MatchString(owner) {
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
