package web

import (
	"html"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"

	"go-git-server/internal/gitrepo"
	"go-git-server/internal/render"
)

// Code search: the files of a branch, tag or commit searched with
// git grep for a plain string (no regular expressions, so no expensive
// patterns), with links to the matching lines.

const (
	maxSearchQuery   = 200
	maxSearchResults = 200
	maxShownLine     = 300 // bytes of a matching line shown
)

type searchData struct {
	Query      string
	IgnoreCase bool
	Ref        string
	Searched   bool
	Error      string
	Files      []searchFile
	Matches    int
	Truncated  bool
}

type searchFile struct {
	Path, Href string
	Lines      []searchLine
}

type searchLine struct {
	N    int
	Href string
	HTML template.HTML
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	ctx := r.Context()
	q := r.URL.Query()
	data := searchData{Query: q.Get("q"), IgnoreCase: q.Get("i") == "1", Ref: q.Get("h")}
	page := func(status int) {
		s.render(w, status, "search", s.repoPage(r, repo, "search", "Search", data))
	}
	if data.Query == "" {
		page(http.StatusOK)
		return
	}
	data.Searched = true
	if len(data.Query) > maxSearchQuery || !utf8.ValidString(data.Query) {
		data.Error = "The search text is too long."
		page(http.StatusBadRequest)
		return
	}
	commit, err := gitrepo.ResolveCommit(ctx, repo.Dir, data.Ref)
	if err != nil {
		if data.Ref != "" {
			data.Error = "No branch, tag or commit named " + data.Ref + "."
			page(http.StatusNotFound)
			return
		}
		page(http.StatusOK) // empty repository: nothing found
		return
	}
	matches, truncated, err := gitrepo.Grep(ctx, repo.Dir, commit, data.Query, data.IgnoreCase, maxSearchResults)
	if err != nil {
		log.Printf("search %s: %v", repo.FullName(), err)
		data.Error = "The search failed or took too long. Try a longer, more specific search."
		page(http.StatusServiceUnavailable)
		return
	}
	data.Matches, data.Truncated = len(matches), truncated
	mark := regexp.MustCompile(regexp.QuoteMeta(data.Query))
	if data.IgnoreCase {
		mark = regexp.MustCompile("(?i)" + regexp.QuoteMeta(data.Query))
	}
	ref := refQuery(data.Ref)
	for _, m := range matches {
		if len(data.Files) == 0 || data.Files[len(data.Files)-1].Path != m.Path {
			data.Files = append(data.Files, searchFile{Path: m.Path, Href: repo.Path() + "/tree/" + render.EscapePath(m.Path) + ref})
		}
		f := &data.Files[len(data.Files)-1]
		sep := "?"
		if ref != "" {
			sep = "&"
		}
		f.Lines = append(f.Lines, searchLine{N: m.Line, Href: f.Href + sep + "lines=" + strconv.Itoa(m.Line) + "#L" + strconv.Itoa(m.Line),
			HTML: markMatch(m.Text, mark)})
	}
	page(http.StatusOK)
}

// markMatch escapes line, shortened to maxShownLine bytes, and wraps the
// first match of re in <mark>.
func markMatch(line string, re *regexp.Regexp) template.HTML {
	if len(line) > maxShownLine {
		cut := maxShownLine
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	loc := re.FindStringIndex(line)
	if loc == nil {
		return template.HTML(html.EscapeString(line))
	}
	return template.HTML(html.EscapeString(line[:loc[0]]) + "<mark>" + html.EscapeString(line[loc[0]:loc[1]]) + "</mark>" +
		html.EscapeString(line[loc[1]:]))
}
