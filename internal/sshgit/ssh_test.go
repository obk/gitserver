package sshgit

import (
	"crypto/ed25519"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestParseSSHKey(t *testing.T) {
	good := newTestKey(t)
	k, err := ParseKey("  " + good + " me@laptop \n")
	if err != nil || k.Comment != "me@laptop" || !strings.HasPrefix(k.Fingerprint, "SHA256:") || k.Key != good {
		t.Fatalf("valid key: %+v, %v", k, err)
	}
	for _, bad := range []string{
		"", "hello", good + "\n" + good, `command="sh" ` + good,
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"ssh-dss AAAAB3NzaC1kc3MAAACBAP==",
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseSSHCommand(t *testing.T) {
	ok := map[string][3]string{
		"git-upload-pack '~alice/pub'":         {"upload-pack", "alice", "pub"},
		"git-upload-pack '/~alice/pub.git'":    {"upload-pack", "alice", "pub"},
		"git-receive-pack '~bob/my.repo'":      {"receive-pack", "bob", "my.repo"},
		"git-upload-archive '~alice/pub.git/'": {"upload-archive", "alice", "pub"},
	}
	for cmd, want := range ok {
		svc, owner, repo, err := parseSSHCommand(cmd)
		if err != nil || [3]string{svc, owner, repo} != want {
			t.Errorf("%q -> %s %s %s %v", cmd, svc, owner, repo, err)
		}
	}
	for _, cmd := range []string{
		"", "sh", "bash -i", "git-upload-pack", "git-upload-pack ~alice/pub",
		"git-upload-pack '~alice/../bob/notes'", "git-upload-pack '~alice/pub'; rm -rf /",
		"git-upload-pack '~alice/pub' extra", "git-upload-pack '/etc/passwd'",
		"git-upload-pack '~Alice/pub'", "git upload-pack '~alice/pub'", "git-upload-pack '--help'",
	} {
		if _, _, _, err := parseSSHCommand(cmd); err == nil {
			t.Errorf("accepted %q", cmd)
		}
	}
}

// Each user may run at most MaxGitPerUser git operations over SSH at once;
// the locks are per user and freed when the holder goes away.
func TestUserSlots(t *testing.T) {
	data := t.TempDir()
	var fds []int
	for i := range MaxGitPerUser {
		fd, ok, err := AcquireUserSlot(data, "alice")
		if !ok || err != nil {
			t.Fatalf("slot %d: %v %v", i, ok, err)
		}
		fds = append(fds, fd)
	}
	if _, ok, _ := AcquireUserSlot(data, "alice"); ok {
		t.Fatal("more than MaxGitPerUser slots")
	}
	if _, ok, _ := AcquireUserSlot(data, "bob"); !ok {
		t.Fatal("alice's slots blocked bob")
	}
	syscall.Close(fds[1]) // git exited
	if _, ok, _ := AcquireUserSlot(data, "alice"); !ok {
		t.Fatal("slot not freed when its holder closed it")
	}
}

func newTestKey(t *testing.T) string {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
}
