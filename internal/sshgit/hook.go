package sshgit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go-git-server/internal/gitrepo"
)

// Pushes run git's hooks from a temporary folder that only holds
// gitserver's own pre-receive hook (core.hooksPath), never hooks from the
// repository folder. The hook enforces protected branches before
// receive-pack updates any ref; a refused push changes nothing.

// ErrProtected is returned by PreReceive when it refused the push.
var ErrProtected = errors.New("push refused: protected branch")

// HookName is the name gitserver runs under as the pre-receive hook: a
// symlink to the program itself (main checks os.Args[0]). A symlink rather
// than a script, because a script in a noexec /tmp would not run, and git
// skips a hook it can't run with only a warning: the protection would be
// gone. The link's target is the installed binary, which can run.
const HookName = "pre-receive"

// hookDir makes a new temporary folder with the pre-receive hook. The
// caller removes it.
func hookDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	hooks, err := os.MkdirTemp("", "gitserver-hooks-")
	if err != nil {
		return "", err
	}
	if err := os.Symlink(exe, filepath.Join(hooks, HookName)); err != nil {
		os.RemoveAll(hooks)
		return "", err
	}
	return hooks, nil
}

// HookRepoDir is the repository a hook runs for: git starts hooks in the
// repository folder with GIT_DIR set (often to ".").
func HookRepoDir() (string, error) {
	dir := os.Getenv("GIT_DIR")
	if dir == "" {
		dir = "."
	}
	return filepath.Abs(dir)
}

// PreReceive is the pre-receive hook for a push to the repository in dir.
// It reads git's "old new ref" lines from in and refuses the whole push,
// explaining why on out, if it deletes or force-pushes a protected branch.
// Creating one, or moving it forward, is fine.
func PreReceive(gitPath, dir string, in io.Reader, out io.Writer) error {
	patterns := gitrepo.Protected(dir)
	var refused []string
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 || len(patterns) == 0 {
			continue
		}
		oldHash, newHash, ref := f[0], f[1], f[2]
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || !gitrepo.IsProtected(patterns, branch) {
			continue
		}
		switch {
		case isZeroHash(newHash):
			refused = append(refused, fmt.Sprintf("%s is protected and can't be deleted", branch))
		case isZeroHash(oldHash):
			// a new branch
		default:
			// The pushed objects are only in the quarantine area git
			// names in the environment, which the command inherits.
			cmd := exec.Command(gitPath, "--git-dir="+dir, "merge-base", "--is-ancestor", oldHash, newHash)
			if cmd.Run() != nil {
				refused = append(refused, fmt.Sprintf("%s is protected: this push would remove commits from it (a force push)", branch))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if refused == nil {
		return nil
	}
	for _, msg := range refused {
		fmt.Fprintln(out, "gitserver:", msg)
	}
	fmt.Fprintln(out, "gitserver: nothing was changed. Merge or rebase onto it instead, or the owner can lift the protection in the repository's settings.")
	return ErrProtected
}

func isZeroHash(h string) bool { return h != "" && strings.Trim(h, "0") == "" }
