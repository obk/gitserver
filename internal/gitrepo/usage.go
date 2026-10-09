package gitrepo

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// OwnerUsage is how much disk one user's repositories take.
type OwnerUsage struct {
	Owner string
	Repos int
	Bytes int64
}

// Usage adds up the size of every owner's repositories, biggest first.
func Usage(reposDir string) ([]OwnerUsage, error) {
	owners, err := os.ReadDir(reposDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []OwnerUsage
	for _, o := range owners {
		if !o.IsDir() {
			continue
		}
		repos, _ := List(reposDir, o.Name())
		u := OwnerUsage{Owner: o.Name(), Repos: len(repos)}
		filepath.WalkDir(filepath.Join(reposDir, o.Name()), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, err := d.Info(); err == nil {
					u.Bytes += fi.Size()
				}
			}
			return nil
		})
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Bytes > list[j].Bytes })
	return list, nil
}

// DiskSpace returns the free (for unprivileged users) and total bytes of
// the file system holding dir.
func DiskSpace(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	// #nosec G115 -- block counts times block size fit easily for any real disk
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
