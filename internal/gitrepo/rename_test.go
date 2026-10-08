package gitrepo

import (
	"os/exec"
	"testing"
)

func TestRename(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	Create(dir, "alice", "a", "", false)
	Create(dir, "alice", "taken", "", false)
	r, _ := Load(dir, "alice", "a")
	for _, bad := range []string{"a", "taken", "../x", "x.git", ""} {
		if err := Rename(dir, r, bad); err == nil {
			t.Errorf("renamed to %q", bad)
		}
	}
	if err := Rename(dir, r, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "alice", "a"); err == nil {
		t.Fatal("old name still a repository")
	}
	if to, ok := Renamed(dir, "alice", "a"); !ok || to.Name != "b" {
		t.Fatalf("a -> %v %v", to, ok)
	}
	// a -> b -> c: the old name leads straight to the newest.
	b, _ := Load(dir, "alice", "b")
	Rename(dir, b, "c")
	for _, old := range []string{"a", "b"} {
		if to, ok := Renamed(dir, "alice", old); !ok || to.Name != "c" {
			t.Fatalf("%s -> %v %v", old, to, ok)
		}
	}
	// Renaming back drops the pointer from its own name; a new repository
	// under an old name wins over the pointer.
	c, _ := Load(dir, "alice", "c")
	Rename(dir, c, "a")
	if _, ok := Renamed(dir, "alice", "a"); ok {
		t.Fatal("a points away although it is a repository again")
	}
	if to, ok := Renamed(dir, "alice", "b"); !ok || to.Name != "a" {
		t.Fatalf("b -> %v %v", to, ok)
	}
	Create(dir, "alice", "b", "", false)
	if _, ok := Renamed(dir, "alice", "b"); ok {
		t.Fatal("pointer beats a real repository")
	}
	// Deleted targets lead nowhere; other owners are separate.
	a, _ := Load(dir, "alice", "a")
	Delete(dir, a)
	if _, ok := Renamed(dir, "alice", "c"); ok {
		t.Fatal("pointer to a deleted repository")
	}
	if _, ok := Renamed(dir, "bob", "a"); ok {
		t.Fatal("pointer crossed owners")
	}
}
