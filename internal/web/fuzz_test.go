package web

import (
	"net/url"
	"path"
	"strings"
	"testing"
)

// Fuzz tests for functions that check attacker-controlled input. Each one
// states what an accepted value must never contain. `go test` runs the seed
// corpus; `go test -fuzz FuzzName` searches for more (see README).

func FuzzCleanTreePath(f *testing.F) {
	for _, s := range []string{"", "a/b.go", "/a/", "../etc/passwd", "a/./b", "a//b", "a/..", "a\x00b", "..."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		clean, ok := cleanTreePath(p)
		if !ok {
			return
		}
		if clean == "" {
			return
		}
		if strings.HasPrefix(clean, "/") || strings.HasSuffix(clean, "/") || strings.ContainsRune(clean, 0) {
			t.Fatalf("cleanTreePath(%q) = %q", p, clean)
		}
		for _, seg := range strings.Split(clean, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("cleanTreePath(%q) = %q has segment %q", p, clean, seg)
			}
		}
		if path.Clean(clean) != clean {
			t.Fatalf("cleanTreePath(%q) = %q is not clean", p, clean)
		}
	})
}

// The login redirect must never leave the site, however a browser parses it.
func FuzzSafeNext(f *testing.F) {
	for _, s := range []string{"/", "/~a/b?h=main", "//evil.com", "/\\evil.com", "/\t/evil.com", "https://evil.com", "/.//evil.com",
		"/%2F%2Fevil.com", "javascript:alert(1)", "/a/../..//evil.com", "/@evil.com"} {
		f.Add(s)
	}
	base, _ := url.Parse("https://git.example.com/login")
	f.Fuzz(func(t *testing.T, next string) {
		got := safeNext(next)
		// Browsers ignore tabs and newlines and treat \ like /.
		browser := strings.NewReplacer("\t", "", "\n", "", "\r", "", "\\", "/").Replace(got)
		// http.Redirect cleans the path before sending it.
		p, q, found := strings.Cut(browser, "?")
		redirected := path.Clean(p)
		if strings.HasSuffix(p, "/") && redirected != "/" {
			redirected += "/"
		}
		if found {
			redirected += "?" + q
		}
		for _, loc := range []string{got, browser, redirected} {
			u, err := url.Parse(loc)
			if err != nil {
				continue
			}
			if r := base.ResolveReference(u); r.Host != base.Host || r.Scheme != base.Scheme {
				t.Fatalf("safeNext(%q) = %q leaves the site as %q", next, got, r)
			}
		}
	})
}
