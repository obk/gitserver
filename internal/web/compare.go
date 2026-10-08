package web

import (
	"log"
	"net/http"

	"go-git-server/internal/gitrepo"
)

// The compare view: the commits on one branch, tag or commit that another
// doesn't have, and the diff between them from where they forked.

type compareData struct {
	From, To string
	Refs     []string // branch and tag names, for the form
	Error    string
	Same     bool
	*gitrepo.Comparison
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	ctx := r.Context()
	q := r.URL.Query()
	data := compareData{From: q.Get("from"), To: q.Get("to")}
	branches, tags, err := gitrepo.Refs(ctx, repo.Dir)
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not read refs.")
		return
	}
	for _, b := range branches {
		data.Refs = append(data.Refs, b.Name)
	}
	for _, t := range tags {
		data.Refs = append(data.Refs, t.Name)
	}
	if data.From == "" {
		data.From = gitrepo.DefaultBranch(ctx, repo.Dir)
	}
	page := func(status int) {
		s.render(w, status, "compare", s.repoPage(r, repo, "refs", "Compare", data))
	}
	if data.From == "" || data.To == "" {
		page(http.StatusOK)
		return
	}
	from, err := gitrepo.ResolveCommit(ctx, repo.Dir, data.From)
	if err != nil {
		data.Error = "No branch, tag or commit named " + data.From + "."
		page(http.StatusNotFound)
		return
	}
	to, err := gitrepo.ResolveCommit(ctx, repo.Dir, data.To)
	if err != nil {
		data.Error = "No branch, tag or commit named " + data.To + "."
		page(http.StatusNotFound)
		return
	}
	if from == to {
		data.Same = true
		page(http.StatusOK)
		return
	}
	if data.Comparison, err = gitrepo.Compare(ctx, repo.Dir, from, to); err != nil {
		log.Printf("compare %s %s..%s: %v", repo.FullName(), from, to, err)
		data.Error = "Could not compare them."
		page(http.StatusInternalServerError)
		return
	}
	page(http.StatusOK)
}
