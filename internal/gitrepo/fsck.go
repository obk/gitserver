package gitrepo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A weekly "gitserver fsck" (systemd timer) checks every repository with
// git fsck, so damage from a failing disk or an interrupted write shows up
// while backups still have a good copy. Results go to the journal, and a
// summary without repository names to fsck-status.json for the admin page.

const fsckTimeout = 30 * time.Minute

// Fsck checks the repository in dir. It returns git's complaints if it
// found any.
func Fsck(ctx context.Context, dir string) (problems string, err error) {
	ctx, cancel := context.WithTimeout(ctx, fsckTimeout)
	defer cancel()
	out, err := Command(ctx, dir, "fsck", "--no-dangling", "--no-progress").CombinedOutput()
	if err != nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p, nil
		}
		return "", err
	}
	return "", nil
}

// FsckStatus is the summary of the last check.
type FsckStatus struct {
	At      time.Time `json:"at"`
	Checked int       `json:"checked"`
	Broken  int       `json:"broken"`
}

const fsckStatusFile = "fsck-status.json"

// WriteFsckStatus saves st in the data folder.
func WriteFsckStatus(dataDir string, st FsckStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dataDir, fsckStatusFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dataDir, fsckStatusFile))
}

// ReadFsckStatus returns the last check's summary, or nil if there was none.
func ReadFsckStatus(dataDir string) *FsckStatus {
	b, err := os.ReadFile(filepath.Join(dataDir, fsckStatusFile))
	if err != nil {
		return nil
	}
	var st FsckStatus
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}
