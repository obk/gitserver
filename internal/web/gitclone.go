package web

import (
	"fmt"
	"net/http"
	"net/http/cgi"
	"regexp"
	"runtime"
	"time"

	"go-git-server/internal/gitrepo"
)

// Read-only git over HTTPS: the repository's web URL is also its clone URL,
// e.g. git clone https://git.example.com/~obk/example (".git" is optional).
// Only public repositories are served; pushing is SSH-only.
var gitHTTPRe = regexp.MustCompile(`^/~([a-z0-9][a-z0-9_-]{0,31})/([A-Za-z0-9][A-Za-z0-9._-]{0,99}?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

// cloneSlots limits concurrent HTTPS clones/fetches (separate from the web UI).
var cloneSlots = make(chan struct{}, max(2, runtime.NumCPU()))

// clonesPerIP caps concurrent HTTPS clones from one client (IPv6: one /64),
// so a single client cannot take all of cloneSlots.
const clonesPerIP = 2

// cloneTimeout ends an HTTPS clone or fetch that takes longer, so clients
// that stop reading cannot hold the clone slots forever.
const cloneTimeout = 30 * time.Minute

func (s *Server) serveGitHTTP(w http.ResponseWriter, r *http.Request, owner, name, endpoint string) {
	plain := func(status int, msg string) {
		// git shows text/plain error bodies to the user as "remote: ...".
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		fmt.Fprintln(w, msg)
	}
	sshURL := "git@" + s.sshHost(r) + ":~" + owner + "/" + name
	switch {
	case endpoint == "git-receive-pack" ||
		(endpoint == "info/refs" && r.URL.Query().Get("service") == "git-receive-pack"):
		plain(http.StatusForbidden, "Pushing over HTTPS is not supported; push over SSH instead:\n  git remote set-url --push origin "+sshURL)
		return
	case endpoint == "info/refs" && r.Method == http.MethodGet && r.URL.Query().Get("service") == "git-upload-pack":
	case endpoint == "git-upload-pack" && r.Method == http.MethodPost:
	default:
		plain(http.StatusNotFound, "Only git clone/fetch over the smart HTTP protocol is supported here.")
		return
	}
	repo, err := gitrepo.Load(s.reposDir, owner, name)
	if err != nil || !repo.Public {
		// Same answer for private and missing repositories.
		plain(http.StatusNotFound, "Repository not found. Private repositories can only be cloned over SSH:\n  git clone "+sshURL)
		return
	}
	client := ipKey(s.clientIP(r))
	if !s.clones.acquire(client, clonesPerIP) {
		plain(http.StatusServiceUnavailable, fmt.Sprintf("Too many clones from your address at once (at most %d); try again when one has finished.", clonesPerIP))
		return
	}
	defer s.clones.release(client)
	// When the deadline passes, writes fail and the CGI handler kills git.
	// The deadlines are cleared afterwards: the connection may be reused.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(cloneTimeout)
	rc.SetReadDeadline(deadline)
	rc.SetWriteDeadline(deadline)
	defer rc.SetReadDeadline(time.Time{})
	defer rc.SetWriteDeadline(time.Time{})
	select {
	case cloneSlots <- struct{}{}:
		defer func() { <-cloneSlots }()
	case <-r.Context().Done():
		return
	case <-time.After(gitrepo.Timeout):
		plain(http.StatusServiceUnavailable, "The server is busy; try again in a moment.")
		return
	}
	r2 := r.Clone(r.Context())
	r2.Header.Del("Authorization")
	r2.Header.Del("Cookie")
	r2.Header.Del("Git-Protocol") // no protocol v2; see sshgit.Serve
	r2.URL.Path = "/" + owner + "/" + name + ".git/" + endpoint
	r2.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	h := &cgi.Handler{
		Path: s.gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.reposDir,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=false",
			"GIT_CONFIG_KEY_1=http.getanyfile", "GIT_CONFIG_VALUE_1=false",
		},
	}
	h.ServeHTTP(w, r2)
}
