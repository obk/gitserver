package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go-git-server/internal/gitrepo"
)

// newRepoWithCommit makes ~owner/name under data with one commit and
// returns the blob's object file.
func newRepoWithCommit(t *testing.T, data, owner, name string) string {
	t.Helper()
	reposDir := filepath.Join(data, "repos")
	if err := gitrepo.Create(reposDir, owner, name, "", false); err != nil {
		t.Fatal(err)
	}
	dir := gitrepo.Dir(reposDir, owner, name)
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := git("hello "+name+"\n", "hash-object", "-w", "--stdin")
	tree := git("100644 blob "+blob+"\tf.txt\n", "mktree")
	commit := git("", "commit-tree", tree, "-m", "x")
	git("", "update-ref", "refs/heads/main", commit)
	return filepath.Join(dir, "objects", blob[:2], blob[2:])
}

func TestFsck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	data := t.TempDir()
	newRepoWithCommit(t, data, "alice", "good")
	if err := cmdFsck([]string{"-data", data}); err != nil {
		t.Fatalf("healthy repository reported: %v", err)
	}
	if st := gitrepo.ReadFsckStatus(data); st == nil || st.Checked != 1 || st.Broken != 0 {
		t.Fatalf("status: %+v", st)
	}
	obj := newRepoWithCommit(t, data, "bob", "bad")
	os.Chmod(obj, 0o644)
	if err := os.Remove(obj); err != nil {
		t.Fatal(err)
	}
	if err := cmdFsck([]string{"-data", data}); err == nil || !strings.Contains(err.Error(), "1 broken") {
		t.Fatalf("missing object not found: %v", err)
	}
	if st := gitrepo.ReadFsckStatus(data); st.Checked != 2 || st.Broken != 1 {
		t.Fatalf("status: %+v", st)
	}
}
