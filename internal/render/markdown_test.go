package render

import (
	"strings"
	"testing"
)

func TestMarkdownRelativeLinks(t *testing.T) {
	src := "[a](auth.go#L48) [b](https://example.com/x) [c](#top) [d](../etc/passwd) [e](/abs) " +
		"[f](deploy/install.sh) [g](javascript:alert(1)) ![i](img/logo.png)"
	html, err := Markdown([]byte(src), "/~o/r/tree/", "/~o/r/raw/")
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
	html, _ = Markdown([]byte("[a](auth.go)"), "", "")
	if !strings.Contains(string(html), `href="auth.go"`) {
		t.Errorf("intro links rewritten: %s", html)
	}
	for name, want := range map[string]string{"a.PNG": "image/png", "x.svg": "image/svg+xml", "main.go": "text/plain; charset=utf-8", "x.html": "text/plain; charset=utf-8"} {
		if got := RawContentType(name); got != want {
			t.Errorf("rawContentType(%s) = %s, want %s", name, got, want)
		}
	}
}
