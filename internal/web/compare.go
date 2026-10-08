package web

import (
	"log"
	"net/http"

	"go-git-server/internal/gitrepo"
)

// The compare view: the commits on one branch, tag or commit that another
// doesn't have, and the diff between them from where they forked.

type compareData struct {
	From, To         string
	FromOpts, ToOpts []refGroup // the form's drop-down lists
	Error            string
	Same             bool
	*gitrepo.Comparison
}

// refGroup is a group of a drop-down list of refs: branches, tags, or the
// commit given in the address.
type refGroup struct {
	Label   string
	Options []refOption
}

type refOption struct {
	Name     string
	Selected bool
}

// refOptions lists branches and tags with selected marked. A selected
// value that is neither (a commit hash from a link) gets its own group, so
// the form keeps it.
func refOptions(branches, tags []string, selected string) []refGroup {
	found := false
	group := func(label string, names []string) refGroup {
		g := refGroup{Label: label}
		for _, n := range names {
			g.Options = append(g.Options, refOption{n, n == selected})
			found = found || n == selected
		}
		return g
	}
	groups := []refGroup{group("Branches", branches), group("Tags", tags)}
	if !found && selected != "" {
		groups = append(groups, refGroup{"Commit", []refOption{{selected, true}}})
	}
	return groups
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
	var branchNames, tagNames []string
	for _, b := range branches {
		branchNames = append(branchNames, b.Name)
	}
	for _, t := range tags {
		tagNames = append(tagNames, t.Name)
	}
	if data.From == "" {
		data.From = gitrepo.DefaultBranch(ctx, repo.Dir)
	}
	// Before anything is picked, "to" starts at another branch (or tag),
	// so Compare shows something right away.
	toPick := data.To
	if toPick == "" {
		for _, n := range append(append([]string{}, branchNames...), tagNames...) {
			if n != data.From {
				toPick = n
				break
			}
		}
	}
	data.FromOpts = refOptions(branchNames, tagNames, data.From)
	data.ToOpts = refOptions(branchNames, tagNames, toPick)
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
