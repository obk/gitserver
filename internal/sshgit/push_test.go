package sshgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryRemoved(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	env := append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
	git := func(args ...string) string {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	commit := func(msg string) string {
		os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644)
		git("add", "f")
		git("commit", "-q", "-m", msg)
		return git("rev-parse", "HEAD")
	}
	a := commit("a")
	b := commit("b")
	git("checkout", "-q", "-b", "side", a)
	c := commit("c") // not a descendant of b
	gitDir := filepath.Join(dir, ".git")

	for _, tc := range []struct {
		name          string
		before, after map[string]string
		want          bool
	}{
		{"fast-forward", map[string]string{"refs/heads/main": a}, map[string]string{"refs/heads/main": b}, false},
		{"new branch", map[string]string{"refs/heads/main": b}, map[string]string{"refs/heads/main": b, "refs/heads/side": c}, false},
		{"unchanged", map[string]string{"refs/heads/main": b}, map[string]string{"refs/heads/main": b}, false},
		{"force push", map[string]string{"refs/heads/main": b}, map[string]string{"refs/heads/main": a}, true},
		{"force push to another line", map[string]string{"refs/heads/main": b}, map[string]string{"refs/heads/main": c}, true},
		{"deleted branch", map[string]string{"refs/heads/main": b, "refs/heads/side": c}, map[string]string{"refs/heads/main": b}, true},
		{"moved tag", map[string]string{"refs/tags/v1": b}, map[string]string{"refs/tags/v1": c}, true},
		{"deleted tag", map[string]string{"refs/tags/v1": a}, map[string]string{}, true},
	} {
		if got := historyRemoved(gitPath, env, gitDir, tc.before, tc.after); got != tc.want {
			t.Errorf("%s: historyRemoved = %v, want %v", tc.name, got, tc.want)
		}
	}
}
