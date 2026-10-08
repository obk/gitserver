package gitrepo

import (
	"strings"
	"testing"
)

// Fuzz tests for functions that check attacker-controlled input. Each one
// states what an accepted value must never contain. `go test` runs the seed
// corpus; `go test -fuzz FuzzName` searches for more (see README).

func FuzzValidRev(f *testing.F) {
	for _, s := range []string{"main", "refs/heads/main", "v1.0", "--output=x", "HEAD:secret", "a..b", "HEAD^{tree}", "x y", "abcd"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rev string) {
		if !validRev(rev) {
			return
		}
		// Not an option, not rev:path or a range, no revision operators, no whitespace.
		if strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, ":\\?*[~^ \t\n\x00") || strings.Contains(rev, "..") {
			t.Fatalf("validRev accepted %q", rev)
		}
	})
}
