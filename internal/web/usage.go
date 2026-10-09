package web

import (
	"log"
	"net/http"

	"go-git-server/internal/gitrepo"
)

// Settings -> disk (admins): how much space each user's repositories take
// and how much is left, and the last repository check. Repository names
// are not shown, so private ones stay private.

type usageData struct {
	Owners      []gitrepo.OwnerUsage
	Repos       int
	Bytes       int64
	Free, Total int64
	UsedPercent int
	Fsck        *gitrepo.FsckStatus
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	owners, err := gitrepo.Usage(s.reposDir)
	if err != nil {
		log.Printf("usage: %v", err)
		s.error(w, r, http.StatusInternalServerError, "Could not read the disk usage.")
		return
	}
	data := usageData{Owners: owners}
	for _, o := range owners {
		data.Repos += o.Repos
		data.Bytes += o.Bytes
	}
	if free, total, err := gitrepo.DiskSpace(s.reposDir); err == nil && total > 0 {
		// #nosec G115 -- disk sizes are far below 2^63 bytes
		data.Free, data.Total = int64(free), int64(total)
		data.UsedPercent = int(100 - free*100/total)
	}
	data.Fsck = gitrepo.ReadFsckStatus(s.cfg.DataDir)
	p := s.newPage(r, "Disk usage", data)
	p.Tab = "usage"
	s.render(w, http.StatusOK, "usage", p)
}
