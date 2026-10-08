package gitrepo

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParseProtected(t *testing.T) {
	got, err := ParseProtected(" main,release/*\nrefs/heads/dev main ")
	if err != nil || !slices.Equal(got, []string{"main", "release/*", "dev"}) {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := ParseProtected(""); err != nil || got != nil {
		t.Fatalf("empty: %q, %v", got, err)
	}
	var many []string
	for i := range maxProtected + 1 {
		many = append(many, fmt.Sprint("b", i))
	}
	for _, bad := range []string{"../x", "a..b", "-x", "a//b", "a/", "a b[", "x;rm", strings.Repeat("a", 101), strings.Join(many, " ")} {
		if _, err := ParseProtected(bad); err == nil {
			t.Errorf("ParseProtected(%q) accepted", bad)
		}
	}
}

func TestIsProtected(t *testing.T) {
	p := []string{"main", "release/*"}
	for branch, want := range map[string]bool{"main": true, "release/1.0": true, "release/1.0/x": false, "mainline": false, "dev": false} {
		if IsProtected(p, branch) != want {
			t.Errorf("IsProtected(%q) = %v", branch, !want)
		}
	}
}
