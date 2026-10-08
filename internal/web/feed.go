package web

import (
	"encoding/xml"
	"log"
	"net/http"
	"net/url"
	"time"

	"go-git-server/internal/gitrepo"
)

// Atom feeds of a repository's commits (log.atom, of one branch with ?h=)
// and tags (tags.atom), for feed readers. Feed readers can't log in, so
// feeds of private repositories only work in the owner's browser; for
// anyone else they don't exist, like the repository.

const feedEntries = 30

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
}

type atomEntry struct {
	ID      string     `xml:"id"`
	Title   string     `xml:"title"`
	Updated string     `xml:"updated"`
	Author  atomAuthor `xml:"author"`
	Link    atomLink   `xml:"link"`
	Content *atomText  `xml:"content,omitempty"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomText struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

func atomTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// writeFeed fills in the feed's updated time (its newest entry) and sends it.
func writeFeed(w http.ResponseWriter, f *atomFeed) {
	updated := time.Unix(0, 0)
	for _, e := range f.Entries {
		if t, err := time.Parse(time.RFC3339, e.Updated); err == nil && t.After(updated) {
			updated = t
		}
	}
	f.Updated = atomTime(updated)
	out, err := xml.MarshalIndent(f, "", "  ")
	if err != nil {
		http.Error(w, "could not make the feed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	w.Write(out)
}

func (s *Server) handleLogFeed(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	ctx := r.Context()
	ref := r.URL.Query().Get("h")
	base := s.baseURL(r) + repo.Path()
	title := repo.FullName() + " commits"
	self, page := base+"/log.atom", base+"/log"
	if ref != "" {
		title += " on " + ref
		self += refQuery(ref)
		page += refQuery(ref)
	}
	f := &atomFeed{ID: self, Title: title, Links: []atomLink{{Href: self, Rel: "self"}, {Href: page, Rel: "alternate"}}}
	commit, err := gitrepo.ResolveCommit(ctx, repo.Dir, ref)
	if err != nil {
		if ref != "" {
			s.notFound(w, r)
			return
		}
		writeFeed(w, f) // an empty repository
		return
	}
	commits, err := gitrepo.Messages(ctx, repo.Dir, commit, feedEntries)
	if err != nil {
		log.Printf("feed %s: %v", repo.FullName(), err)
		http.Error(w, "could not read the log", http.StatusInternalServerError)
		return
	}
	for _, c := range commits {
		link := base + "/commit/" + c.Hash
		e := atomEntry{ID: link, Title: c.Subject, Updated: atomTime(c.Date), Author: atomAuthor{c.Author}, Link: atomLink{Href: link}}
		if c.Body != "" {
			e.Content = &atomText{Type: "text", Text: c.Body}
		}
		f.Entries = append(f.Entries, e)
	}
	writeFeed(w, f)
}

func (s *Server) handleTagsFeed(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo) {
	_, tags, err := gitrepo.Refs(r.Context(), repo.Dir)
	if err != nil {
		http.Error(w, "could not read the tags", http.StatusInternalServerError)
		return
	}
	base := s.baseURL(r) + repo.Path()
	self := base + "/tags.atom"
	f := &atomFeed{ID: self, Title: repo.FullName() + " tags",
		Links: []atomLink{{Href: self, Rel: "self"}, {Href: base + "/refs", Rel: "alternate"}}}
	for i, t := range tags {
		if i == feedEntries {
			break
		}
		link := base + "/log?" + url.Values{"h": {t.Full}}.Encode()
		e := atomEntry{ID: link, Title: t.Name, Updated: atomTime(t.Date), Author: atomAuthor{t.Author}, Link: atomLink{Href: link}}
		if e.Author.Name == "" {
			e.Author.Name = repo.Owner
		}
		if t.Subject != "" {
			e.Content = &atomText{Type: "text", Text: t.Subject}
		}
		f.Entries = append(f.Entries, e)
	}
	writeFeed(w, f)
}
