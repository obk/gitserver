package gitrepo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Renaming moves a repository's folder. The old name keeps working as a
// pointer to the new one: the owner's folder has a list of renames,
// gitserver-renamed ("old new" per line), which the web UI follows with a
// redirect, HTTPS clones too (git follows it), and SSH explains. A new
// repository under the old name takes precedence over the pointer.

const (
	renamedFile = "gitserver-renamed"
	maxRenamed  = 200
)

// Rename gives r the name newName. The caller holds r's push lock, so no
// push is halfway through.
func Rename(reposDir string, r *Repo, newName string) error {
	if !validRepoName(newName) {
		return errors.New("repository names are 1-100 characters: letters, digits, . _ and -, not ending in .git")
	}
	if newName == r.Name {
		return errors.New("that is already its name")
	}
	if r.Dir != Dir(reposDir, r.Owner, r.Name) {
		return errors.New("refusing to rename unexpected path")
	}
	newDir := Dir(reposDir, r.Owner, newName)
	if _, err := os.Lstat(newDir); err == nil {
		return fmt.Errorf("a repository named %s already exists", newName)
	}
	if err := os.Rename(r.Dir, newDir); err != nil {
		return err
	}
	return recordRename(reposDir, r.Owner, r.Name, newName)
}

// recordRename notes that owner's repository old is now called new. Older
// pointers to old now point to new, so lookups take one step, and a pointer
// from new itself (it was renamed away earlier) is dropped.
func recordRename(reposDir, owner, old, new string) error {
	list := readRenames(reposDir, owner)
	var out []string
	for _, r := range list {
		switch {
		case r[0] == new || r[0] == old:
			continue
		case r[1] == old:
			r[1] = new
		}
		out = append(out, r[0]+" "+r[1])
	}
	out = append(out, old+" "+new)
	if len(out) > maxRenamed {
		out = out[len(out)-maxRenamed:]
	}
	file := filepath.Join(reposDir, owner, renamedFile)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func readRenames(reposDir, owner string) [][2]string {
	var list [][2]string
	for _, line := range strings.Split(ReadSmallFileN(filepath.Join(reposDir, owner, renamedFile), 64<<10), "\n") {
		if old, new, ok := strings.Cut(strings.TrimSpace(line), " "); ok && validRepoName(old) && validRepoName(new) {
			list = append(list, [2]string{old, new})
		}
	}
	return list
}

// Renamed returns the repository that owner's repository name was renamed
// to, if name is not a repository itself and the new one exists.
func Renamed(reposDir, owner, name string) (*Repo, bool) {
	if _, err := Load(reposDir, owner, name); err == nil {
		return nil, false
	}
	for _, r := range readRenames(reposDir, owner) {
		if r[0] == name {
			repo, err := Load(reposDir, owner, r[1])
			return repo, err == nil
		}
	}
	return nil, false
}
