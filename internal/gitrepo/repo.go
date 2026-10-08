// Package gitrepo manages the bare repositories on disk and runs the git
// commands that read them.
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go-git-server/internal/account"
	"go-git-server/internal/store"
)

// A repository is a bare git directory <data>/repos/<owner>/<name>.git and
// belongs to exactly one user. It is private unless it contains the file
// git-daemon-export-ok. Only the owner can see a private repository, and
// only the owner can push.
type Repo struct {
	Owner       string
	Name        string
	Dir         string
	Description string
	Public      bool
	Updated     time.Time
}

const publicMarker = "git-daemon-export-ok"

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func validRepoName(name string) bool {
	return repoNameRe.MatchString(name) && !strings.HasSuffix(name, ".git") && !strings.Contains(name, "..")
}

// FullName is how a repository is written in URLs and git remotes: ~owner/name.
func (r *Repo) FullName() string { return "~" + r.Owner + "/" + r.Name }

// Path is the web UI path of the repository.
func (r *Repo) Path() string { return "/" + r.FullName() }

func Dir(reposDir, owner, name string) string {
	return filepath.Join(reposDir, owner, name+".git")
}

func Load(reposDir, owner, name string) (*Repo, error) {
	if !account.UserNameRe.MatchString(owner) || !validRepoName(name) {
		return nil, errNotFound
	}
	dir := Dir(reposDir, owner, name)
	if fi, err := os.Lstat(filepath.Join(dir, "HEAD")); err != nil || !fi.Mode().IsRegular() {
		return nil, errNotFound
	}
	r := &Repo{Owner: owner, Name: name, Dir: dir}
	r.Description = readSmallFile(filepath.Join(dir, "description"))
	if strings.HasPrefix(r.Description, "Unnamed repository;") {
		r.Description = ""
	}
	_, err := os.Stat(filepath.Join(dir, publicMarker))
	r.Public = err == nil
	return r, nil
}

// List returns all repositories, or only those of owner if it is set.
func List(reposDir, owner string) ([]*Repo, error) {
	owners := []string{owner}
	if owner == "" {
		entries, err := os.ReadDir(reposDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		owners = owners[:0]
		for _, e := range entries {
			if e.IsDir() {
				owners = append(owners, e.Name())
			}
		}
	}
	var repos []*Repo
	for _, o := range owners {
		entries, err := os.ReadDir(filepath.Join(reposDir, o))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), ".git")
			if !ok || !e.IsDir() {
				continue
			}
			if r, err := Load(reposDir, o, name); err == nil {
				repos = append(repos, r)
			}
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName() < repos[j].FullName() })
	return repos, nil
}

// OwnerDirExists reports whether <reposDir>/<owner> exists. Repositories
// stay on disk when an account is deleted, and ownership goes by name, so
// such a name must not be given to a new account.
func OwnerDirExists(reposDir, owner string) bool {
	_, err := os.Lstat(filepath.Join(reposDir, owner))
	return err == nil
}

func (r *Repo) CanRead(u *store.User) bool { return r.Public || r.IsOwner(u) }

func (r *Repo) CanWrite(u *store.User) bool { return r.IsOwner(u) }

func (r *Repo) IsOwner(u *store.User) bool { return u != nil && u.Name == r.Owner }

func readSmallFile(path string) string { return ReadSmallFileN(path, 4096) }

func ReadSmallFileN(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, max))
	return strings.TrimSpace(string(b))
}

func validDescription(desc string) error {
	if len([]rune(desc)) > 200 || strings.ContainsFunc(desc, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return errors.New("descriptions are at most 200 characters on one line")
	}
	return nil
}

func Create(reposDir, owner, name, desc string, public bool) error {
	if !account.UserNameRe.MatchString(owner) {
		return fmt.Errorf("invalid owner %q", owner)
	}
	if !validRepoName(name) {
		return errors.New("repository names are 1-100 characters: letters, digits, . _ and -, not ending in .git")
	}
	if err := validDescription(desc); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(reposDir, owner), 0o750); err != nil {
		return err
	}
	dir := Dir(reposDir, owner, name)
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("a repository named %s already exists", name)
	}
	cmd := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", "--initial-branch=main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "description"), []byte(desc+"\n"), 0o644); err != nil {
		return err
	}
	return SetPublic(reposDir, owner, name, public)
}

func SetPublic(reposDir, owner, name string, public bool) error {
	if _, err := Load(reposDir, owner, name); err != nil {
		return fmt.Errorf("no repository ~%s/%s", owner, name)
	}
	marker := filepath.Join(Dir(reposDir, owner, name), publicMarker)
	if public {
		return os.WriteFile(marker, nil, 0o644)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func SetDescription(r *Repo, desc string) error {
	if err := validDescription(desc); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Dir, "description"), []byte(desc+"\n"), 0o644)
}

// Delete removes the repository directory. r.Dir is built from
// validated names, so it is always <reposDir>/<owner>/<name>.git.
func Delete(reposDir string, r *Repo) error {
	if r.Dir != Dir(reposDir, r.Owner, r.Name) {
		return errors.New("refusing to delete unexpected path")
	}
	return os.RemoveAll(r.Dir)
}

// DeleteOwner deletes every repository of owner, and their folder.
func DeleteOwner(reposDir, owner string) error {
	if owner == "" || owner != filepath.Base(owner) || owner == "." || owner == ".." {
		return errors.New("refusing to delete unexpected path")
	}
	return os.RemoveAll(filepath.Join(reposDir, owner))
}

// ParseRef accepts "~owner/name", "owner/name" and an optional ".git".
func ParseRef(s string) (owner, name string, err error) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "~"), ".git")
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !account.UserNameRe.MatchString(owner) || !validRepoName(name) {
		return "", "", fmt.Errorf("expected ~owner/name, got %q", s)
	}
	return owner, name, nil
}
