package render

import (
	"net/url"
	"path"
	"strings"
	"testing"
)

// Fuzz tests for functions that check attacker-controlled input. Each one
// states what an accepted value must never contain. `go test` runs the seed
// corpus; `go test -fuzz FuzzName` searches for more (see README).

// README links rewritten into the repository must stay inside it.
func FuzzRewriteRelative(f *testing.F) {
	for _, s := range []string{"docs/a.md", "../x", "a/../../b", "https://evil.com", "//evil.com", "#top", "a.png?raw=1#x", "./a", "a%2F..%2F..%2Fb"} {
		f.Add(s)
	}
	const linkBase = "/~a/r/tree/"
	repoBase, _ := url.Parse("https://git.example.com/~a/r/")
	f.Fuzz(func(t *testing.T, dest string) {
		out := string(rewriteRelative([]byte(dest), linkBase))
		if out == dest {
			return // left alone: absolute, fragment or outside the repository
		}
		if !strings.HasPrefix(out, linkBase) {
			t.Fatalf("rewriteRelative(%q) = %q", dest, out)
		}
		u, err := url.Parse(out)
		if err != nil {
			t.Fatalf("rewriteRelative(%q) = %q does not parse: %v", dest, out, err)
		}
		r := repoBase.ResolveReference(u)
		if r.Host != repoBase.Host || !strings.HasPrefix(path.Clean(r.Path)+"/", linkBase) {
			t.Fatalf("rewriteRelative(%q) = %q resolves to %q", dest, out, r)
		}
	})
}
