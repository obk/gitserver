package web

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go-git-server/internal/gitrepo"
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

func TestProtectedBranches(t *testing.T) {
	e := newTestEnv(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // where pushes put their hook folder
	e.pushHistory("alice", "pub")
	dir := filepath.Join(e.work, "alice-pub-hist") // on branch feature
	repoDir := gitrepo.Dir(filepath.Join(e.data, "repos"), "alice", "pub")
	remote := "git@git.test:~alice/pub"
	git := func(args ...string) (string, error) { return e.git("alice", dir, args...) }
	must := func(args ...string) string {
		t.Helper()
		out, err := git(args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	serverRef := func(ref string) string {
		out, _ := exec.Command("git", "--git-dir="+repoDir, "rev-parse", "--verify", "-q", ref).Output()
		return strings.TrimSpace(string(out))
	}
	// A hook in the repository folder must never run.
	os.WriteFile(filepath.Join(repoDir, "hooks", "pre-receive"), []byte("#!/bin/sh\necho REPO HOOK RAN >&2\nexit 1\n"), 0o755)

	// Protect main and release/* in the settings.
	alice := e.login("alice")
	_, body := e.get(alice, "/~alice/pub/settings")
	csrf := csrfToken(t, body)
	settings := func(protected string) (int, string) {
		return e.post(alice, "/~alice/pub/settings", url.Values{"csrf": {csrf}, "description": {"d"}, "visibility": {"public"}, "protected": {protected}}, "")
	}
	if code, body := settings("main ../x"); code != http.StatusBadRequest || !strings.Contains(body, "not a branch name") {
		t.Fatalf("bad pattern accepted: %d", code)
	}
	if code, body := settings("main, refs/heads/release/* main"); code != 200 || !strings.Contains(body, `value="main release/*"`) {
		t.Fatalf("protecting: %d\n%s", code, body)
	}

	mainBefore := serverRef("refs/heads/main")
	// Force-pushing main is refused, and nothing changes.
	out, err := git("push", "-f", remote, "feature~2:main") // main is feature~1; this drops "second"
	if err == nil || !strings.Contains(out, "main is protected: this push would remove commits") || strings.Contains(out, "REPO HOOK") {
		t.Fatalf("force push to a protected branch: %v\n%s", err, out)
	}
	// Deleting it too, even together with an allowed change.
	out, err = git("push", remote, ":main", "feature:refs/heads/other")
	if err == nil || !strings.Contains(out, "main is protected and can't be deleted") {
		t.Fatalf("delete of a protected branch: %v\n%s", err, out)
	}
	if serverRef("refs/heads/main") != mainBefore || serverRef("refs/heads/other") != "" {
		t.Fatal("a refused push changed refs")
	}
	// Moving it forward works, and so does creating and deleting others.
	must("push", remote, "feature:main")
	if serverRef("refs/heads/main") != serverRef("refs/heads/feature") {
		t.Fatal("fast-forward of a protected branch refused")
	}
	must("push", remote, "feature~1:refs/heads/release/1.0", "feature~1:refs/heads/scratch")
	must("push", "-f", remote, "feature~2:scratch")
	must("push", remote, ":scratch")
	if out, err := git("push", "-f", remote, "feature~2:release/1.0"); err == nil || !strings.Contains(out, "release/1.0 is protected") {
		t.Fatalf("pattern release/* not enforced: %v\n%s", err, out)
	}
	// Lifting the protection allows it again.
	settings("")
	if _, err := os.Stat(filepath.Join(repoDir, "gitserver-protected-branches")); err == nil {
		t.Fatal("protection file left after clearing")
	}
	must("push", "-f", remote, "feature~2:main")
	if left, _ := filepath.Glob(filepath.Join(tmp, "gitserver-hooks-*")); len(left) != 0 {
		t.Fatalf("hook folders left behind: %v", left)
	}
}

func TestArchive(t *testing.T) {
	e := newTestEnv(t)
	e.pushHistory("alice", "pub")
	e.pushHistory("alice", "secret")
	anon := &http.Client{}
	fetch := func(c *http.Client, path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	resp, b := fetch(anon, "/~alice/pub/archive/feature.tar.gz")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/gzip" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename=pub-feature.tar.gz` {
		t.Fatalf("tar.gz: %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if !slices.Contains(names, "pub-feature/feature.txt") || !slices.Contains(names, "pub-feature/src/app.go") {
		t.Fatalf("tar.gz contents: %v", names)
	}

	resp, b = fetch(anon, "/~alice/pub/archive/refs/tags/v1.zip")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "pub-v1.zip") {
		t.Fatalf("zip: %d %v", resp.StatusCode, resp.Header)
	}
	zipr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, f := range zipr.File {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "pub-v1/notes.txt") || slices.Contains(names, "pub-v1/feature.txt") {
		t.Fatalf("zip of v1 (before feature.txt): %v", names)
	}

	for _, p := range []string{"/~alice/pub/archive/nope.zip", "/~alice/pub/archive/main.rar", "/~alice/pub/archive/.zip",
		"/~alice/pub/archive/--output=x.zip", "/~alice/secret/archive/main.zip"} {
		if code, _ := e.get(anon, p); code != http.StatusNotFound {
			t.Errorf("%s: %d", p, code)
		}
	}
	if resp, _ := fetch(e.login("alice"), "/~alice/secret/archive/main.zip"); resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("owner's private archive: %d %v", resp.StatusCode, resp.Header)
	}
	if _, body := e.get(anon, "/~alice/pub/refs"); !strings.Contains(body, `href="/~alice/pub/archive/main.tar.gz"`) ||
		!strings.Contains(body, `href="/~alice/pub/archive/refs/tags/v1.zip"`) {
		t.Fatal("refs page has no archive links")
	}
	if _, body := e.get(anon, "/~alice/pub/"); !strings.Contains(body, `href="/~alice/pub/archive/main.zip"`) {
		t.Fatal("summary has no download link")
	}
}

func TestMirrorSettings(t *testing.T) {
	e := newTestEnv(t)
	alice := e.login("alice")
	_, body := e.get(alice, "/create")
	csrf := csrfToken(t, body)
	create := func(name, mirror string) (int, string) {
		return e.post(alice, "/create", url.Values{"csrf": {csrf}, "name": {name}, "visibility": {"public"}, "mirror": {mirror}}, "")
	}
	for _, bad := range []string{"http://github.com/o/r", "https://169.254.169.254/x", "https://u:p@github.com/o/r", "file:///etc"} {
		if code, _ := create("m", bad); code != http.StatusBadRequest {
			t.Errorf("mirror source %q accepted: %d", bad, code)
		}
	}
	if code, body := create("m", "https://github.com/obk/gitserver.git"); code != 200 || !strings.Contains(body, "Waiting for the first sync") ||
		!strings.Contains(body, ">mirror</span>") {
		t.Fatalf("creating a mirror: %d\n%s", code, body)
	}
	repoDir := gitrepo.Dir(filepath.Join(e.data, "repos"), "alice", "m")
	trigger := filepath.Join(e.data, gitrepo.MirrorTriggerFile)
	r, _ := gitrepo.Load(filepath.Join(e.data, "repos"), "alice", "m")
	if r.Mirror != "https://github.com/obk/gitserver.git" || !gitrepo.MirrorDue(r, time.Now()) {
		t.Fatalf("mirror not set up: %+v", r)
	}
	if _, err := os.Stat(trigger); err != nil {
		t.Fatal("first sync not triggered")
	}

	// Nobody pushes to a mirror, not even its owner.
	dir := filepath.Join(e.work, "m-src")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "f"), []byte("x\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-qm", "x"}} {
		e.git("alice", dir, args...)
	}
	if out, err := e.git("alice", dir, "push", "git@git.test:~alice/m", "main"); err == nil || !strings.Contains(out, "is a mirror of") {
		t.Fatalf("push to a mirror: %v\n%s", err, out)
	}

	// Sync now, then a failed sync's error shows in the settings.
	os.Remove(trigger)
	_, body = e.get(alice, "/~alice/m/settings")
	csrf = csrfToken(t, body)
	if !strings.Contains(body, "Not synced yet") || !strings.Contains(body, "Sync now") {
		t.Fatalf("mirror settings:\n%s", body)
	}
	if code, body := e.post(alice, "/~alice/m/mirror/sync", url.Values{"csrf": {csrf}}, ""); code != 200 || !strings.Contains(body, "Sync started") {
		t.Fatalf("sync now: %d", code)
	}
	if _, err := os.Stat(trigger); err != nil {
		t.Fatal("sync now didn't trigger a sync")
	}
	os.WriteFile(filepath.Join(repoDir, "gitserver-mirror-status.json"), []byte(`{"at":"2026-01-02T03:04:05Z","error":"git fetch: repository not found"}`), 0o644)
	if _, body := e.get(alice, "/~alice/m/settings"); !strings.Contains(body, "failed: git fetch: repository not found") {
		t.Fatal("sync error not shown")
	}
	// Other users can't trigger syncs.
	bob := e.login("bob")
	_, bobBody := e.get(bob, "/settings/keys")
	if code, _ := e.post(bob, "/~alice/m/mirror/sync", url.Values{"csrf": {csrfToken(t, bobBody)}}, ""); code != http.StatusNotFound {
		t.Fatalf("another user triggered a sync: %d", code)
	}

	// Clearing the source makes it an ordinary repository again.
	e.post(alice, "/~alice/m/settings", url.Values{"csrf": {csrf}, "description": {""}, "visibility": {"public"}, "mirror": {""}}, "")
	if out, err := e.git("alice", dir, "push", "git@git.test:~alice/m", "main"); err != nil {
		t.Fatalf("push after mirroring stopped: %v\n%s", err, out)
	}
}

func TestFolderReadme(t *testing.T) {
	e := newTestEnv(t)
	dir := filepath.Join(e.work, "readme-src")
	os.MkdirAll(filepath.Join(dir, "docs", "deep"), 0o755)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "README.md"), []byte("# Docs\n\nSee [setup](setup.md), [main](../main.go), [deep](deep/) "+
		"and [far](../../etc/passwd).\n\n![logo](logo.png)\n\n<script>alert(1)</script>\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "deep", "readme.txt"), []byte("plain <b>text</b>\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-qm", "x"}, {"tag", "v1"},
		{"push", "-q", "git@git.test:~alice/pub", "main", "v1"}} {
		if out, err := e.git("alice", dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	anon := &http.Client{}
	_, body := e.get(anon, "/~alice/pub/tree/docs")
	for _, want := range []string{
		`<h1 id="docs">Docs</h1>`,
		`href="/~alice/pub/tree/docs/setup.md"`,
		`href="/~alice/pub/tree/main.go"`, // ../ from docs/
		`href="/~alice/pub/tree/docs/deep"`,
		`href="../../etc/passwd"`, // leaves the repository: untouched
		`src="/~alice/pub/raw/docs/logo.png"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs/ README lacks %s", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("raw HTML in a README was rendered")
	}
	// On a tag, links stay on it.
	if _, body := e.get(anon, "/~alice/pub/tree/docs?h=v1"); !strings.Contains(body, `href="/~alice/pub/tree/docs/setup.md?h=v1"`) {
		t.Errorf("README links leave the tag:\n%s", body)
	}
	// A README that isn't Markdown is shown as text.
	if _, body := e.get(anon, "/~alice/pub/tree/docs/deep"); !strings.Contains(body, "<pre>plain &lt;b&gt;text&lt;/b&gt;\n</pre>") {
		t.Errorf("text README:\n%s", body)
	}
	// No README, nothing shown.
	if _, body := e.get(anon, "/~alice/pub/tree"); strings.Contains(body, `class="readme`) {
		t.Error("README shown for a folder without one")
	}
}

func TestRepoRename(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	e.pushInitial("alice", "secret")
	alice := e.login("alice")
	noFollow := func(c *http.Client) *http.Client {
		return &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	_, body := e.get(alice, "/~alice/pub/settings")
	csrf := csrfToken(t, body)
	rename := func(repo, name string) (int, string) {
		return e.post(alice, "/~alice/"+repo+"/rename", url.Values{"csrf": {csrf}, "name": {name}}, "")
	}
	for _, bad := range []string{"secret", "pub", "../x", "x.git", ""} {
		if code, _ := rename("pub", bad); code == 200 && bad != "" {
			t.Errorf("renamed to %q", bad)
		}
	}
	if code, body := rename("pub", "project"); code != 200 || !strings.Contains(body, "Renamed.") || !strings.Contains(body, "~alice/project") {
		t.Fatalf("rename: %d\n%s", code, body)
	}
	// The old address leads to the new one, keeping the path and query.
	resp, err := noFollow(&http.Client{}).Get(e.srv.URL + "/~alice/pub/tree/README?h=main")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/~alice/project/tree/README?h=main" {
		t.Fatalf("old address: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// HTTPS clones of the old address work (git follows the redirect).
	if out, err := e.git("", e.work, "clone", "-q", e.srv.URL+"/~alice/pub", "old-addr"); err != nil {
		t.Fatalf("clone of the old address: %v\n%s", err, out)
	}
	// SSH says where it went.
	if out, err := e.git("alice", e.work, "ls-remote", "git@git.test:~alice/pub"); err == nil || !strings.Contains(out, "was renamed to ~alice/project") {
		t.Fatalf("SSH to the old name: %v\n%s", err, out)
	}

	// A private repository's old name stays a 404 for others.
	_, body = e.get(alice, "/~alice/secret/settings")
	rename("secret", "hidden")
	for _, c := range []*http.Client{{}, e.login("bob")} {
		if code, _ := e.get(c, "/~alice/secret/"); code != http.StatusNotFound {
			t.Errorf("private repo's old name: %d", code)
		}
	}
	if out, _ := e.git("bob", e.work, "ls-remote", "git@git.test:~alice/secret"); strings.Contains(out, "hidden") {
		t.Fatalf("SSH leaked a private repo's new name to bob:\n%s", out)
	}
	if resp, _ := noFollow(alice).Get(e.srv.URL + "/~alice/secret/"); resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("owner not redirected: %d", resp.StatusCode)
	}
	// Only the owner renames.
	bob := e.login("bob")
	_, bobBody := e.get(bob, "/settings/keys")
	if code, _ := e.post(bob, "/~alice/project/rename", url.Values{"csrf": {csrfToken(t, bobBody)}, "name": {"x"}}, ""); code != http.StatusNotFound {
		t.Fatalf("bob renamed alice's repo: %d", code)
	}
	// The command line too.
	if out, err := exec.Command(testBinary, "repo", "rename", "-data", e.data, "~alice/project", "proj").CombinedOutput(); err != nil {
		t.Fatalf("repo rename: %v\n%s", err, out)
	}
	if resp, _ := noFollow(&http.Client{}).Get(e.srv.URL + "/~alice/pub/"); resp.Header.Get("Location") != "/~alice/proj/" {
		t.Fatalf("chain not followed: %s", resp.Header.Get("Location"))
	}
}
