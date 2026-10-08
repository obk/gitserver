package gitrepo

import (
	"strings"
	"testing"
)

func TestValidation(t *testing.T) {
	for _, n := range []string{"", ".hidden", "a/b", "..", "x.git", "a b", strings.Repeat("a", 101)} {
		if validRepoName(n) {
			t.Errorf("validRepoName(%q) = true", n)
		}
	}
	for _, r := range []string{"-x", "a..b", "HEAD:secret", "a b", "HEAD~1", "--upload-pack=x"} {
		if validRev(r) {
			t.Errorf("validRev(%q) = true", r)
		}
	}
}
