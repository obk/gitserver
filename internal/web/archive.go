package web

import (
	"context"
	"fmt"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"go-git-server/internal/gitrepo"
)

// Archive downloads: a branch, tag or commit as .tar.gz or .zip, made by
// git archive and streamed as it is made. They count against the same
// limits as HTTPS clones (per address, overall, and in time), since they
// cost about as much.

var archiveFormats = map[string]struct{ format, contentType string }{
	".tar.gz": {"tar.gz", "application/gzip"},
	".zip":    {"zip", "application/zip"},
}

// archiveName is the file name (without extension) and top folder of an
// archive of repo at ref: "hello-v1.0", "hello-feature-x".
func archiveName(repo *gitrepo.Repo, ref string) string {
	ref = strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	return repo.Name + "-" + strings.ReplaceAll(ref, "/", "-")
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request, repo *gitrepo.Repo, file string) {
	var ref, ext string
	for e := range archiveFormats {
		if name, ok := strings.CutSuffix(file, e); ok && name != "" {
			ref, ext = name, e
		}
	}
	if ref == "" {
		s.notFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cloneTimeout)
	defer cancel()
	commit, err := gitrepo.ResolveCommit(ctx, repo.Dir, ref)
	if err != nil {
		s.notFound(w, r)
		return
	}
	client := ipKey(s.clientIP(r))
	if !s.clones.acquire(client, clonesPerIP) {
		s.error(w, r, http.StatusServiceUnavailable, fmt.Sprintf("Too many downloads from your address at once (at most %d); try again when one has finished.", clonesPerIP))
		return
	}
	defer s.clones.release(client)
	select {
	case cloneSlots <- struct{}{}:
		defer func() { <-cloneSlots }()
	case <-ctx.Done():
		return
	case <-time.After(gitrepo.Timeout):
		s.error(w, r, http.StatusServiceUnavailable, "The server is busy; try again in a moment.")
		return
	}

	// A slow reader can't hold a slot for longer than a clone could; the
	// deadline is cleared afterwards, the connection may be reused.
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(cloneTimeout))
	defer rc.SetWriteDeadline(time.Time{})

	f := archiveFormats[ext]
	name := archiveName(repo, ref)
	h := w.Header()
	h.Set("Content-Type", f.contentType)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ext}))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if !repo.Public {
		h.Set("Cache-Control", "no-store")
	}
	// --format is fixed here; attributes in the repository (export-ignore,
	// export-subst) apply as git defines them.
	cmd := gitrepo.Command(ctx, repo.Dir, "archive", "--format="+f.format, "--prefix="+name+"/", commit, "--")
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		// The headers are sent; the client sees a cut-off download.
		log.Printf("archive %s %s: %v", repo.FullName(), ref, err)
	}
}
