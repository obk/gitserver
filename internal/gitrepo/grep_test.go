package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepLimits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Repeat("hit\n", 25)), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("miss\nhit\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "c.bin"), []byte("hit\x00\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ctx := context.Background()
	gitDir := filepath.Join(dir, ".git")
	commit, err := ResolveCommit(ctx, gitDir, "")
	if err != nil {
		t.Fatal(err)
	}
	m, truncated, err := Grep(ctx, gitDir, commit, "hit", false, 100)
	if err != nil || !truncated || len(m) != maxGrepPerFile+1 || m[maxGrepPerFile] != (GrepMatch{"b.txt", 2, "hit"}) {
		t.Fatalf("per-file cap or binary skip: %v %v %+v", err, truncated, m)
	}
	if m, truncated, _ := Grep(ctx, gitDir, commit, "hit", false, 5); len(m) != 5 || !truncated {
		t.Fatalf("total cap: %d %v", len(m), truncated)
	}
	if m, truncated, err := Grep(ctx, gitDir, commit, "nothing", false, 5); err != nil || len(m) != 0 || truncated {
		t.Fatalf("no match: %v %v %v", m, truncated, err)
	}
	if _, _, err := Grep(ctx, gitDir, "HEAD", "hit", false, 5); err == nil {
		t.Fatal("accepted a commit that is not a hash")
	}
}
