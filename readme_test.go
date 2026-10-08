package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestReadmeCodeLinks keeps README.md's code links honest: every link of the
// form [`name`](file#L12) must point at a line that contains `name`. When code
// moves, this fails and says which link to update.
func TestReadmeCodeLinks(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	links := regexp.MustCompile("\\[`([^`]+)`\\]\\(([^)#\\s]+)#L([0-9]+)\\)").FindAllStringSubmatch(string(readme), -1)
	if len(links) < 50 {
		t.Fatalf("only %d code links found in README.md", len(links))
	}
	files := map[string][]string{}
	for _, l := range links {
		name, file := l[1], l[2]
		n, _ := strconv.Atoi(l[3])
		lines, ok := files[file]
		if !ok {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Errorf("README links to missing file %s", file)
				continue
			}
			lines = strings.Split(string(b), "\n")
			files[file] = lines
		}
		if n < 1 || n > len(lines) || !strings.Contains(lines[n-1], name) {
			want := -1
			for i, line := range lines {
				if strings.Contains(line, name) {
					want = i + 1
					break
				}
			}
			t.Errorf("README.md: [`%s`](%s#L%d) is out of date (first match is now line %d)", name, file, n, want)
		}
	}
}

func TestMarkdownRelativeLinks(t *testing.T) {
	src := "[a](auth.go#L48) [b](https://example.com/x) [c](#top) [d](../etc/passwd) [e](/abs) " +
		"[f](deploy/install.sh) [g](javascript:alert(1)) ![i](img/logo.png)"
	html, err := renderMarkdown([]byte(src), "/~o/r/tree/", "/~o/r/raw/")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{
		`href="/~o/r/tree/auth.go#L48"`, `href="https://example.com/x"`, `href="#top"`,
		`href="../etc/passwd"`, `href="/abs"`, `href="/~o/r/tree/deploy/install.sh"`, `src="/~o/r/raw/img/logo.png"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in %s", want, h)
		}
	}
	if strings.Contains(h, "javascript:") {
		t.Errorf("javascript: link rendered: %s", h)
	}
	// Without bases (intro.md), links are left alone.
	html, _ = renderMarkdown([]byte("[a](auth.go)"), "", "")
	if !strings.Contains(string(html), `href="auth.go"`) {
		t.Errorf("intro links rewritten: %s", html)
	}
	for name, want := range map[string]string{"a.PNG": "image/png", "x.svg": "image/svg+xml", "main.go": "text/plain; charset=utf-8", "x.html": "text/plain; charset=utf-8"} {
		if got := rawContentType(name); got != want {
			t.Errorf("rawContentType(%s) = %s, want %s", name, got, want)
		}
	}
}
