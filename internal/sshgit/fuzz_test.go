package sshgit

import (
	"strings"
	"testing"

	"go-git-server/internal/account"
)

// Fuzz tests for functions that check attacker-controlled input. Each one
// states what an accepted value must never contain. `go test` runs the seed
// corpus; `go test -fuzz FuzzName` searches for more (see WIKI.md).

func FuzzParseSSHCommand(f *testing.F) {
	for _, s := range []string{"git-upload-pack '~obk/x.git'", "git-receive-pack '/~a/b/'", "git-upload-archive '~a/b'",
		"git-upload-pack '~a/../b'", "git-upload-pack '~a/b'; id", "sh -c id", "git-upload-pack '~A/b'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		service, owner, repo, err := parseSSHCommand(cmd)
		if err != nil {
			return
		}
		if service != "upload-pack" && service != "receive-pack" && service != "upload-archive" {
			t.Fatalf("unknown service %q from %q", service, cmd)
		}
		if !account.UserNameRe.MatchString(owner) {
			t.Fatalf("bad owner %q from %q", owner, cmd)
		}
		if repo == "" || strings.ContainsAny(repo, "/\\'\x00 \n") || strings.HasPrefix(repo, "-") || strings.HasPrefix(repo, ".") {
			t.Fatalf("bad repo %q from %q", repo, cmd)
		}
	})
}
