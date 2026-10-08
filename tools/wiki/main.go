// Command wiki turns WIKI.md into GitHub wiki pages: Home (the intro and
// a table of contents), one page per "## " section, and a sidebar. Links
// between sections are pointed at the right page, and links to files in the
// repository at their GitHub URL. The wiki workflow runs it on every change
// to it, so WIKI.md stays the one place to edit.
//
//	go run ./tools/wiki -repo owner/name -branch main -out DIR
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

func main() {
	readme := flag.String("src", "WIKI.md", "the Markdown file to split")
	out := flag.String("out", "wiki", "directory to write the pages to")
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "GitHub repository (owner/name), for links to files")
	branch := flag.String("branch", "main", "branch that links to files point at")
	flag.Parse()
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "wiki: -repo is required")
		os.Exit(2)
	}
	src, err := os.ReadFile(*readme)
	if err == nil {
		err = write(*out, string(src), "https://github.com/"+*repo+"/blob/"+*branch+"/")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wiki:", err)
		os.Exit(1)
	}
}

// write splits src into pages and writes them to dir.
func write(dir, src, fileURL string) error {
	pages, err := split(src, fileURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, p := range pages {
		if err := os.WriteFile(filepath.Join(dir, p.file+".md"), []byte(p.body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type page struct {
	title, file string
	lines       []string
	body        string
}

// A target is where a README anchor ends up: a page, and an anchor on it
// ("" for the page itself).
type target struct{ file, anchor string }

const footer = "_This page is generated from [WIKI.md](%sWIKI.md); edit it there._\n"

// split returns the wiki pages for the README src: Home, one per section,
// _Sidebar and _Footer.
func split(src, fileURL string) ([]page, error) {
	home := &page{title: "Home", file: "Home"}
	var sections []*page
	cur := home
	skip := false // inside "## Contents", replaced by a generated one
	inFence := false
	for _, line := range strings.Split(strings.TrimRight(src, "\n"), "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence {
			if title, ok := strings.CutPrefix(line, "## "); ok {
				skip = title == "Contents"
				if skip {
					continue
				}
				cur = &page{title: title, file: pageFile(title)}
				sections = append(sections, cur)
				continue
			}
			if cur == home && strings.HasPrefix(line, "# ") {
				continue // the project name; the wiki shows the page title
			}
		}
		if !skip {
			cur.lines = append(cur.lines, line)
		}
	}
	if inFence {
		return nil, errors.New("unclosed code block")
	}
	if len(sections) == 0 {
		return nil, errors.New(`no "## " sections`)
	}

	// Where each README anchor lands. Anchors are numbered (-1, -2) per
	// document, so a heading's anchor in the README and on its page differ
	// when an earlier section has a heading of the same name.
	targets := map[string]target{}
	global := map[string]int{}
	for _, p := range append([]*page{home}, sections...) {
		if p != home {
			targets[uniqueSlug(global, p.title)] = target{p.file, ""}
		}
		local := map[string]int{}
		for _, h := range headings(p.lines) {
			targets[uniqueSlug(global, h)] = target{p.file, uniqueSlug(local, h)}
		}
	}

	var toc, side strings.Builder
	for i, p := range sections {
		fmt.Fprintf(&toc, "%d. [%s](%s)\n", i+1, plain(p.title), p.file)
		fmt.Fprintf(&side, "- [%s](%s)\n", plain(p.title), p.file)
	}
	var pages []page
	for _, p := range append([]*page{home}, sections...) {
		body, err := rewriteLinks(strings.Join(trimSeparators(p.lines), "\n"), p.file, fileURL, targets)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.title, err)
		}
		if p == home { // its links are wiki pages already
			body += "\n\n## Contents\n\n" + toc.String()
		}
		pages = append(pages, page{title: p.title, file: p.file, body: strings.TrimSpace(body) + "\n"})
	}
	pages = append(pages,
		page{title: "Sidebar", file: "_Sidebar", body: "**[Home](Home)**\n\n" + side.String()},
		page{title: "Footer", file: "_Footer", body: fmt.Sprintf(footer, fileURL)})
	return pages, nil
}

// pageFile is the wiki file name (without .md) for a section title; the
// wiki shows it with the dashes as spaces.
func pageFile(title string) string {
	var b strings.Builder
	for _, r := range plain(title) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// plain removes Markdown code marks from a heading.
func plain(s string) string { return strings.ReplaceAll(s, "`", "") }

// headings returns the text of the headings (### and deeper) in lines,
// skipping code blocks.
func headings(lines []string) []string {
	var hs []string
	inFence := false
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(line, "##") {
			if t := strings.TrimLeft(line, "#"); strings.HasPrefix(t, " ") {
				hs = append(hs, strings.TrimSpace(t))
			}
		}
	}
	return hs
}

// slug is GitHub's anchor for a heading: lower case, punctuation removed,
// spaces as dashes.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// uniqueSlug numbers repeated anchors the way GitHub does: name, name-1, ...
func uniqueSlug(seen map[string]int, heading string) string {
	s := slug(heading)
	n := seen[s]
	seen[s] = n + 1
	if n > 0 {
		return fmt.Sprintf("%s-%d", s, n)
	}
	return s
}

// trimSeparators drops blank lines and "---" rules at the start and end.
func trimSeparators(lines []string) []string {
	empty := func(l string) bool { l = strings.TrimSpace(l); return l == "" || l == "---" }
	for len(lines) > 0 && empty(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && empty(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return lines
}

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// rewriteLinks points the Markdown links in body (on page file) at their
// place in the wiki: README anchors at the page they moved to, files at
// GitHub. Code blocks are left alone. An anchor that matches no heading is
// an error, so a broken README link fails the build.
func rewriteLinks(body, file, fileURL string, targets map[string]target) (string, error) {
	lines := strings.Split(body, "\n")
	inFence := false
	var bad []string
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		lines[i] = linkRe.ReplaceAllStringFunc(line, func(m string) string {
			dest := m[2 : len(m)-1]
			switch {
			case strings.Contains(dest, "://"), strings.HasPrefix(dest, "mailto:"):
				return m
			case strings.HasPrefix(dest, "#"):
				t, ok := targets[dest[1:]]
				if !ok {
					bad = append(bad, dest)
					return m
				}
				switch {
				case t.anchor == "":
					return "](" + t.file + ")"
				case t.file == file:
					return "](#" + t.anchor + ")"
				}
				return "](" + t.file + "#" + t.anchor + ")"
			}
			return "](" + fileURL + strings.TrimPrefix(dest, "./") + ")"
		})
	}
	if bad != nil {
		return "", fmt.Errorf("links to missing headings: %s", strings.Join(bad, ", "))
	}
	return strings.Join(lines, "\n"), nil
}
