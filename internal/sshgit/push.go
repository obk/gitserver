package sshgit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"go-git-server/internal/gitrepo"
)

// A push that deletes a branch or tag, or force-pushes over commits, leaves
// those commits on disk: git keeps unreachable objects until gc prunes them,
// normally two weeks later. gitserver never serves them (see
// gitrepo.ResolveCommit), but a leaked secret should not stay on the
// server's disk either. So after such a push, ssh-serve runs gc and prunes
// unreachable objects at once.
//
// Pruning "now" is safe because every push to a repository holds its push
// lock until its gc is done: nothing else writes objects meanwhile (the web
// UI and clones only read). A grace period would protect against concurrent
// writers instead, but it would also keep exactly the objects of a quick
// "oops, force-push" alive. git's own background gc after a push is turned
// off, so it can't run beside this one; ssh-serve runs "gc --auto" itself.

// receivePack runs git receive-pack for repo (a child process, unlike the
// other services, so that gitserver can clean up afterwards).
func receivePack(gitPath string, argv, env []string, repo *gitrepo.Repo, audit func(string, ...any)) error {
	// The client may disconnect once git has reported the result; the
	// cleanup must still finish.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE, syscall.SIGINT)

	lock, err := LockPush(repo)
	if err != nil {
		return err
	}
	defer lock.Close()

	before, err := listRefs(gitPath, env, repo.Dir)
	if err != nil {
		return err
	}
	hooks, err := hookDir()
	if err != nil {
		return err
	}
	// #nosec G204 G702 -- no shell; argv was checked by parseSSHCommand
	cmd := exec.Command(gitPath, append([]string{"-c", "core.hooksPath=" + hooks}, argv[1:]...)...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	os.RemoveAll(hooks)
	if err != nil {
		// git has told the client what went wrong; just pass on its status.
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		return err
	}
	after, err := listRefs(gitPath, env, repo.Dir)
	if err != nil {
		return err
	}

	if !historyRemoved(gitPath, env, repo.Dir, before, after) {
		// Routine housekeeping, normally a no-op.
		run(gitPath, env, repo.Dir, "gc", "--auto", "--quiet")
		return nil
	}
	fmt.Fprintln(os.Stderr, "gitserver: this push removed commits; deleting them from the server's disk...")
	err = run(gitPath, env, repo.Dir, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all")
	if err == nil {
		err = run(gitPath, env, repo.Dir, "-c", "gc.reflogExpire=now", "-c", "gc.reflogExpireUnreachable=now",
			"gc", "--quiet", "--prune=now")
	}
	if err != nil {
		audit("cleanup failed repo=%s: %v", repo.FullName(), err)
		fmt.Fprintln(os.Stderr, "gitserver: the push worked, but deleting the removed commits failed; please tell the administrator")
		return nil
	}
	audit("removed unreachable objects repo=%s", repo.FullName())
	fmt.Fprintln(os.Stderr, "gitserver: done; the removed commits are gone from the server.")
	return nil
}

// LockPush waits for other pushes to repo to finish (including their
// cleanup) and returns the held lock; closing it releases the lock.
func LockPush(repo *gitrepo.Repo) (*os.File, error) {
	// #nosec G703 -- repo.Dir is made from a checked owner and repository name
	f, err := os.OpenFile(filepath.Join(repo.Dir, "gitserver-push.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		fmt.Fprintf(os.Stderr, "gitserver: waiting for another push to %s to finish...\n", repo.FullName())
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// listRefs returns every ref of the repository in dir and its object.
func listRefs(gitPath string, env []string, dir string) (map[string]string, error) {
	// #nosec G204 G702 -- no shell; fixed arguments
	cmd := exec.Command(gitPath, "--git-dir="+dir, "for-each-ref", "--format=%(objectname) %(refname)")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing refs: %w", err)
	}
	refs := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if obj, name, ok := strings.Cut(sc.Text(), " "); ok {
			refs[name] = obj
		}
	}
	return refs, sc.Err()
}

// historyRemoved reports whether going from the refs before to the refs
// after left objects unreachable: a ref was deleted, or moved to something
// that does not contain what it pointed to (a force push, a moved tag).
// When in doubt it says yes; an extra gc does no harm.
func historyRemoved(gitPath string, env []string, dir string, before, after map[string]string) bool {
	for ref, old := range before {
		now, ok := after[ref]
		if !ok {
			return true
		}
		if now == old {
			continue
		}
		// #nosec G204 G702 -- no shell; both hashes come from git itself
		cmd := exec.Command(gitPath, "--git-dir="+dir, "merge-base", "--is-ancestor", old, now)
		cmd.Env = env
		if cmd.Run() != nil { // not an ancestor, or not commits at all
			return true
		}
	}
	return false
}

// run runs a git command in the repository at dir with its output going to
// this process's stderr (shown to the client, which is still connected).
func run(gitPath string, env []string, dir string, args ...string) error {
	// #nosec G204 G702 -- no shell; fixed git commands
	cmd := exec.Command(gitPath, append([]string{"--git-dir=" + dir}, args...)...)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
