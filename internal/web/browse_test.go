package web

import (
	"encoding/xml"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pushHistory pushes a small history to ~owner/repo:
//
//	main:    "first" (notes.txt with lines one..five, src/app.go),
//	         "second" (notes.txt line three changed), tag v1 (annotated)
//	feature: main + "add feature" (feature.txt)
func (e *testEnv) pushHistory(owner, repo string) {
	dir := filepath.Join(e.work, owner+"-"+repo+"-hist")
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	write := func(name, content string) { os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644) }
	steps := []func(){
		func() {
			write("notes.txt", "one\ntwo\nthree\nfour\nfive\n")
			write("src/app.go", "package app\n\n// Needle is what search looks for.\nconst Needle = 1\n")
		},
		func() { write("notes.txt", "one\ntwo\nTHREE <b>loud</b>\nfour\nfive\n") },
		func() { write("feature.txt", "a feature needle\n") },
	}
	git := func(args ...string) {
		e.t.Helper()
		if out, err := e.git(owner, dir, args...); err != nil {
			e.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	steps[0]()
	git("add", ".")
	git("commit", "-q", "-m", "first\n\nThe body of the first commit.")
	steps[1]()
	git("commit", "-q", "-am", "second")
	git("tag", "-a", "v1", "-m", "Release <one>")
	git("checkout", "-q", "-b", "feature")
	steps[2]()
	git("add", ".")
	git("commit", "-q", "-m", "add feature")
	git("push", "-q", "git@git.test:~"+owner+"/"+repo, "main", "feature", "v1")
}

func TestParseLines(t *testing.T) {
	for _, tc := range []struct {
		in       string
		from, to int
		ok       bool
	}{
		{"3", 3, 3, true}, {"2-4", 2, 4, true}, {"4-2", 2, 4, true}, {"1-5", 1, 5, true},
		{"0", 0, 0, false}, {"6", 0, 0, false}, {"2-9", 0, 0, false}, {"x", 0, 0, false},
		{"2-", 0, 0, false}, {"-2", 0, 0, false}, {"1-2-3", 0, 0, false}, {"", 0, 0, false},
	} {
		from, to, ok := parseLines(tc.in, 5)
		if from != tc.from || to != tc.to || ok != tc.ok {
			t.Errorf("parseLines(%q) = %d, %d, %v", tc.in, from, to, ok)
		}
	}
}

func TestLineLinks(t *testing.T) {
	lines := make([]template.HTML, 5)
	hrefs := func(page string, from, to int) []string {
		var out []string
		for _, l := range lineLinks(lines, page, from, to) {
			out = append(out, l.Href)
		}
		return out
	}
	// Nothing selected: each number selects its line.
	if h := hrefs("/f", 0, 0); h[1] != "/f?lines=2#L2" {
		t.Errorf("no selection: %v", h)
	}
	// One line selected: others make a range, its own number clears.
	if h := hrefs("/f?h=v1", 3, 3); h[0] != "/f?h=v1&lines=1-3#L1" || h[2] != "/f?h=v1#L3" || h[4] != "/f?h=v1&lines=3-5#L3" {
		t.Errorf("one line selected: %v", h)
	}
	// A range selected: numbers start over.
	if h := hrefs("/f", 2, 4); h[4] != "/f?lines=5#L5" {
		t.Errorf("range selected: %v", h)
	}
	sel := lineLinks(lines, "/f", 2, 4)
	if sel[0].Selected || !sel[1].Selected || !sel[3].Selected || sel[4].Selected {
		t.Errorf("selection marks: %+v", sel)
	}
}

func TestLineSelection(t *testing.T) {
	e := newTestEnv(t)
	e.pushHistory("alice", "pub")
	anon := &http.Client{}
	_, body := e.get(anon, "/~alice/pub/tree/notes.txt?lines=4-2")
	if !strings.Contains(body, "Lines 2&ndash;4 selected") || strings.Count(body, `class="sel"`) != 6 {
		t.Fatalf("range not shown as selected:\n%s", body)
	}
	if !strings.Contains(body, `<span id="L3" class="sel">THREE &lt;b&gt;loud&lt;/b&gt;</span>`) {
		t.Fatalf("line not escaped or not marked:\n%s", body)
	}
	_, body = e.get(anon, "/~alice/pub/tree/notes.txt?h=v1&lines=2")
	if !strings.Contains(body, `href="/~alice/pub/tree/notes.txt?h=v1&amp;lines=2-5#L2"`) {
		t.Fatalf("no range link from a selected line:\n%s", body)
	}
	for _, bad := range []string{"0", "9", "a-b"} {
		if code, body := e.get(anon, "/~alice/pub/tree/notes.txt?lines="+bad); code != 200 || strings.Contains(body, `class="sel"`) {
			t.Errorf("lines=%s: %d, selection shown", bad, code)
		}
	}
}

func TestFeeds(t *testing.T) {
	e := newTestEnv(t)
	e.pushHistory("alice", "pub")
	e.pushHistory("alice", "secret")
	anon := &http.Client{}
	get := func(c *http.Client, path string) atomFeed {
		t.Helper()
		resp, err := c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/atom+xml; charset=utf-8" {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		var f atomFeed
		if err := xml.NewDecoder(resp.Body).Decode(&f); err != nil {
			t.Fatalf("%s: not Atom: %v", path, err)
		}
		return f
	}

	f := get(anon, "/~alice/pub/log.atom")
	if len(f.Entries) != 2 || f.Entries[0].Title != "second" || f.Entries[1].Content == nil ||
		f.Entries[1].Content.Text != "The body of the first commit." || f.Entries[0].Author.Name != "Alice" {
		t.Fatalf("main feed: %+v", f.Entries)
	}
	if !strings.HasPrefix(f.Entries[0].Link.Href, e.srv.URL+"/~alice/pub/commit/") || f.Updated != f.Entries[0].Updated {
		t.Fatalf("links or updated: %+v", f)
	}
	if f := get(anon, "/~alice/pub/log.atom?h=feature"); len(f.Entries) != 3 || f.Entries[0].Title != "add feature" {
		t.Fatalf("branch feed: %+v", f.Entries)
	}
	tags := get(anon, "/~alice/pub/tags.atom")
	if len(tags.Entries) != 1 || tags.Entries[0].Title != "v1" || tags.Entries[0].Content.Text != "Release <one>" {
		t.Fatalf("tags feed: %+v", tags.Entries)
	}
	if code, _ := e.get(anon, "/~alice/pub/log.atom?h=nope"); code != http.StatusNotFound {
		t.Fatalf("feed of a missing branch: %d", code)
	}
	for _, p := range []string{"/~alice/secret/log.atom", "/~alice/secret/tags.atom"} {
		if code, _ := e.get(anon, p); code != http.StatusNotFound {
			t.Fatalf("private feed %s: %d", p, code)
		}
	}
	get(e.login("alice"), "/~alice/secret/log.atom") // the owner can read it
	_, page := e.get(anon, "/~alice/pub/log")
	if !strings.Contains(page, `type="application/atom+xml"`) || !strings.Contains(page, `href="/~alice/pub/log.atom"`) {
		t.Fatal("log page doesn't link its feed")
	}
}

func TestSearch(t *testing.T) {
	e := newTestEnv(t)
	e.pushHistory("alice", "pub")
	e.pushHistory("alice", "secret")
	anon := &http.Client{}

	_, body := e.get(anon, "/~alice/pub/search?q=Needle")
	if !strings.Contains(body, "2 matching lines in 1 file") ||
		!strings.Contains(body, `href="/~alice/pub/tree/src/app.go?lines=4#L4"`) ||
		!strings.Contains(body, "const <mark>Needle</mark> = 1") {
		t.Fatalf("case-sensitive search:\n%s", body)
	}
	_, body = e.get(anon, "/~alice/pub/search?q=needle&i=1&h=feature")
	if !strings.Contains(body, "3 matching lines in 2 files") || !strings.Contains(body, "a feature <mark>needle</mark>") ||
		!strings.Contains(body, `href="/~alice/pub/tree/feature.txt?h=feature&amp;lines=1#L1"`) {
		t.Fatalf("search on a branch, ignoring case:\n%s", body)
	}
	// The text is searched for literally and shown escaped.
	_, body = e.get(anon, "/~alice/pub/search?q="+url.QueryEscape("<b>loud"))
	if !strings.Contains(body, "THREE <mark>&lt;b&gt;loud</mark>&lt;/b&gt;") {
		t.Fatalf("literal search:\n%s", body)
	}
	for q, want := range map[string]string{
		"t.*o":                   "Nothing found",
		"-e":                     "Nothing found", // not taken as an option
		"--no-index":             "Nothing found",
		strings.Repeat("x", 201): "too long",
	} {
		if _, body := e.get(anon, "/~alice/pub/search?q="+url.QueryEscape(q)); !strings.Contains(body, want) {
			t.Errorf("search %q: no %q", q, want)
		}
	}
	if code, _ := e.get(anon, "/~alice/pub/search?q=x&h=nope"); code != http.StatusNotFound {
		t.Errorf("search on a missing branch: %d", code)
	}
	if code, _ := e.get(anon, "/~alice/secret/search?q=Needle"); code != http.StatusNotFound {
		t.Fatalf("searched a private repository: %d", code)
	}
	if _, body := e.get(e.login("alice"), "/~alice/secret/search?q=Needle"); !strings.Contains(body, "<mark>Needle</mark>") {
		t.Fatal("owner can't search their private repository")
	}
}

func TestCompare(t *testing.T) {
	e := newTestEnv(t)
	e.pushHistory("alice", "pub")
	e.pushHistory("alice", "secret")
	anon := &http.Client{}

	_, body := e.get(anon, "/~alice/pub/compare?from=main&to=feature")
	if !strings.Contains(body, "1 commit on <b>feature</b> that <b>main</b> doesn't have") ||
		!strings.Contains(body, "add feature") || strings.Contains(body, ">second<") ||
		!strings.Contains(body, "feature.txt") || !strings.Contains(body, "a feature needle") || strings.Contains(body, "notes.txt") {
		t.Fatalf("main to feature:\n%s", body)
	}
	// From a tag to a branch: v1 is main, so the same as above.
	if _, body := e.get(anon, "/~alice/pub/compare?from=v1&to=feature"); !strings.Contains(body, "add feature") {
		t.Fatal("compare from a tag")
	}
	// Backwards: feature has everything main has.
	_, body = e.get(anon, "/~alice/pub/compare?from=feature&to=main")
	if !strings.Contains(body, "has no commits that") || !strings.Contains(body, "No changes to files") {
		t.Fatalf("feature to main:\n%s", body)
	}
	if _, body := e.get(anon, "/~alice/pub/compare?from=main&to=v1"); !strings.Contains(body, "the same commit") {
		t.Fatal("same commit not noticed")
	}
	// Without from, the default branch is preselected.
	if _, body := e.get(anon, "/~alice/pub/compare"); !strings.Contains(body, `name="from" value="main"`) || !strings.Contains(body, `<option value="v1">`) {
		t.Fatalf("empty compare form:\n%s", body)
	}
	for _, q := range []string{"from=main&to=nope", "from=main..feature&to=main", "from=--all&to=main"} {
		if code, _ := e.get(anon, "/~alice/pub/compare?"+q); code != http.StatusNotFound {
			t.Errorf("compare %s: %d", q, code)
		}
	}
	if _, body := e.get(anon, "/~alice/pub/refs"); !strings.Contains(body, `href="/~alice/pub/compare?from=main&amp;to=feature"`) {
		t.Fatal("refs page has no compare link")
	}
	if code, _ := e.get(anon, "/~alice/secret/compare?from=main&to=feature"); code != http.StatusNotFound {
		t.Fatalf("compared in a private repository: %d", code)
	}
}
