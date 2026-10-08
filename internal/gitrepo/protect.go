package gitrepo

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Protected branches can't be deleted or force-pushed: a force push
// deletes the commits it drops from the server for good (see
// sshgit/push.go), so a slip on main would lose history. The list is in
// the repository folder, one name per line; "*" matches within one path
// segment (release/* matches release/1.0, not release/1.0/x).
const protectedFile = "gitserver-protected-branches"

const maxProtected = 20

var protectedRe = regexp.MustCompile(`^[A-Za-z0-9._*][A-Za-z0-9._*/-]{0,99}$`)

// Protected returns the protected branch patterns of the repository in dir.
func Protected(dir string) []string {
	var list []string
	for _, line := range strings.Split(ReadSmallFileN(filepath.Join(dir, protectedFile), 16<<10), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			list = append(list, line)
		}
	}
	return list
}

// ParseProtected splits text (names separated by spaces, commas or new
// lines) into protected branch patterns and checks them.
func ParseProtected(text string) ([]string, error) {
	var list []string
	seen := map[string]bool{}
	for _, p := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		p = strings.TrimPrefix(p, "refs/heads/")
		if !protectedRe.MatchString(p) || strings.Contains(p, "..") || strings.Contains(p, "//") || strings.HasSuffix(p, "/") {
			return nil, fmt.Errorf("%q is not a branch name", p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%q is not a valid pattern", p)
		}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	if len(list) > maxProtected {
		return nil, fmt.Errorf("at most %d protected branches", maxProtected)
	}
	return list, nil
}

// SetProtected saves the protected branch patterns of r.
func SetProtected(r *Repo, list []string) error {
	file := filepath.Join(r.Dir, protectedFile)
	if len(list) == 0 {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(file, []byte(strings.Join(list, "\n")+"\n"), 0o644)
}

// IsProtected reports whether branch (without refs/heads/) matches one of
// the patterns.
func IsProtected(patterns []string, branch string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, branch); ok {
			return true
		}
	}
	return false
}
