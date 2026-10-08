package account

import (
	"regexp"
	"strings"
	"testing"
)

func TestRecoveryCodes(t *testing.T) {
	codes := NewRecoveryCodes()
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("%d codes", len(codes))
	}
	format := regexp.MustCompile(`^[a-z2-7]{4}(-[a-z2-7]{4}){3}$`)
	seen := map[string]bool{}
	for _, c := range codes {
		if !format.MatchString(c) || seen[c] {
			t.Fatalf("bad or repeated code %q", c)
		}
		seen[c] = true
	}
	c := codes[0]
	// Typed in another case, without dashes or with spaces: the same code.
	for _, typed := range []string{strings.ToUpper(c), strings.ReplaceAll(c, "-", ""), strings.ReplaceAll(c, "-", " ")} {
		if NormalizeRecoveryCode(typed) != NormalizeRecoveryCode(c) || HashRecoveryCode("alice", typed) != HashRecoveryCode("alice", c) {
			t.Errorf("%q is not treated as %q", typed, c)
		}
	}
	for _, bad := range []string{"", "123456", "abcd-efgh-2345-mno", "abcd-efgh-2345-mnop1", "abcd-efgh-2345-mn0p", "abcd-efgh-2345-mn!p"} {
		if NormalizeRecoveryCode(bad) != "" {
			t.Errorf("%q accepted as a recovery code", bad)
		}
	}
	if HashRecoveryCode("alice", c) == HashRecoveryCode("bob", c) {
		t.Error("hash does not depend on the user")
	}
}
