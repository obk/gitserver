package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go-git-server/internal/gitrepo"
	"go-git-server/internal/sshgit"
)

// cmdFsck: gitserver fsck. Checks every repository, one at a time and each
// under its push lock (a push's cleanup deletes objects, which would look
// like damage halfway through). Prints a line per problem for the journal
// and fails if anything is broken.
func cmdFsck(args []string) error {
	fs, data := newFlagSet("fsck")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errUsage
	}
	repos, err := gitrepo.List(filepath.Join(*data, "repos"), "")
	if err != nil {
		return err
	}
	st := gitrepo.FsckStatus{At: time.Now()}
	for _, repo := range repos {
		lock, err := sshgit.LockPush(repo)
		if err != nil {
			return err
		}
		problems, err := gitrepo.Fsck(context.Background(), repo.Dir)
		lock.Close()
		st.Checked++
		switch {
		case err != nil:
			st.Broken++
			fmt.Printf("%s: could not check: %v\n", repo.FullName(), err)
		case problems != "":
			st.Broken++
			fmt.Printf("%s: BROKEN\n  %s\n", repo.FullName(), strings.ReplaceAll(problems, "\n", "\n  "))
		}
	}
	if err := gitrepo.WriteFsckStatus(*data, st); err != nil {
		return err
	}
	fmt.Printf("checked %d repositories, %d broken\n", st.Checked, st.Broken)
	if st.Broken > 0 {
		return fmt.Errorf("%d broken repositories; restore them from a backup or snapshot", st.Broken)
	}
	return nil
}

// cmdMirrorSync: gitserver mirror-sync. Syncs every pull mirror that is
// due (asked for, or not synced for an hour), one at a time and each under
// its push lock. Run by gitserver-mirror.timer and gitserver-mirror.path.
// A failed sync is recorded for the repository's owner to see; it doesn't
// fail the command.
func cmdMirrorSync(args []string) error {
	fs, data := newFlagSet("mirror-sync")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errUsage
	}
	repos, err := gitrepo.List(filepath.Join(*data, "repos"), "")
	if err != nil {
		return err
	}
	synced, failed := 0, 0
	for _, repo := range repos {
		if !gitrepo.MirrorDue(repo, time.Now()) {
			continue
		}
		if free, _, err := gitrepo.DiskSpace(repo.Dir); err == nil && free < 1<<30 {
			fmt.Println("less than 1 GiB of disk free; not syncing mirrors")
			break
		}
		lock, err := sshgit.LockPush(repo)
		if err != nil {
			return err
		}
		err = gitrepo.SyncMirror(context.Background(), repo)
		lock.Close()
		if err != nil {
			failed++
			fmt.Printf("%s: %v\n", repo.FullName(), err)
			continue
		}
		synced++
	}
	if synced+failed > 0 {
		fmt.Printf("synced %d mirrors, %d failed\n", synced, failed)
	}
	return nil
}
