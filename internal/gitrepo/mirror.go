package gitrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-git-server/internal/outbound"
)

// A pull mirror is a repository that copies another one, e.g. on GitHub:
// its branches and tags are fetched again and again ("gitserver
// mirror-sync", run by a systemd timer) and nobody pushes to it. The source
// URL is in gitserver-mirror in the repository folder, the result of the
// last sync in gitserver-mirror-status.json. Only public https sources:
// see outbound for why the server must not fetch from just anywhere.

const (
	mirrorFile        = "gitserver-mirror"
	mirrorStatusFile  = "gitserver-mirror-status.json"
	mirrorRequestFile = "gitserver-mirror-request"
	// MirrorTriggerFile, in the data folder, starts a sync at once when
	// touched (gitserver-mirror.path watches it).
	MirrorTriggerFile = "mirror-sync-now"

	// MirrorInterval is how often a mirror is synced.
	MirrorInterval = time.Hour
	mirrorTimeout  = 10 * time.Minute
	maxMirrorURL   = 500
)

// ParseMirrorURL checks a mirror source: https, a host name or public
// address, no user name or password (only public repositories), no query.
func ParseMirrorURL(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || len(s) > maxMirrorURL || u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return nil, errors.New("the mirror source must be an https:// URL, like https://github.com/owner/repo.git")
	}
	if u.User != nil {
		return nil, errors.New("the mirror source can't contain a user name or password; only public repositories can be mirrored")
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, " \t\r\n\\") {
		return nil, errors.New("the mirror source can't have a query (?) or fragment (#)")
	}
	// Names are checked when syncing (they can change); obvious inside
	// addresses are refused right away.
	host := strings.ToLower(u.Hostname())
	if a, err := netip.ParseAddr(host); (err == nil && !outbound.IsPublic(a)) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, errors.New("the mirror source must be on the public internet")
	}
	return u, nil
}

// SetMirror makes r a mirror of source, or stops mirroring if source is "".
func SetMirror(r *Repo, source string) error {
	file := filepath.Join(r.Dir, mirrorFile)
	if source == "" {
		for _, f := range []string{file, filepath.Join(r.Dir, mirrorStatusFile), filepath.Join(r.Dir, mirrorRequestFile)} {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	u, err := ParseMirrorURL(source)
	if err != nil {
		return err
	}
	return os.WriteFile(file, []byte(u.String()+"\n"), 0o644)
}

// MirrorStatus is the result of a mirror's last sync.
type MirrorStatus struct {
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// ReadMirrorStatus returns the last sync's result, or nil if there was none.
func ReadMirrorStatus(dir string) *MirrorStatus {
	b, err := os.ReadFile(filepath.Join(dir, mirrorStatusFile))
	if err != nil {
		return nil
	}
	var st MirrorStatus
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

func writeMirrorStatus(dir string, st MirrorStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, mirrorStatusFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, mirrorStatusFile))
}

// RequestMirrorSync asks for r to be synced at the next run of the sync
// job, and starts that run now where systemd watches the trigger file.
func RequestMirrorSync(dataDir string, r *Repo) error {
	if err := os.WriteFile(filepath.Join(r.Dir, mirrorRequestFile), nil, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, MirrorTriggerFile), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
}

// MirrorDue reports whether r should be synced now: it was asked for, or
// the last sync is MirrorInterval old.
func MirrorDue(r *Repo, now time.Time) bool {
	if r.Mirror == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(r.Dir, mirrorRequestFile)); err == nil {
		return true
	}
	st := ReadMirrorStatus(r.Dir)
	return st == nil || now.Sub(st.At) >= MirrorInterval-time.Minute
}

// mirrorEnv is added to the environment of mirror git commands (tests use
// it to trust their own certificate); mirrorAddrs resolves the source.
var (
	mirrorEnv   []string
	mirrorAddrs = outbound.PublicAddrs
)

// SyncMirror fetches every branch and tag of r's source into r, removing
// the ones the source no longer has, and records the result. The caller
// holds the push lock.
func SyncMirror(ctx context.Context, r *Repo) error {
	err := syncMirror(ctx, r)
	st := MirrorStatus{At: time.Now()}
	if err != nil {
		st.Error = err.Error()
	}
	os.Remove(filepath.Join(r.Dir, mirrorRequestFile))
	if werr := writeMirrorStatus(r.Dir, st); werr != nil && err == nil {
		err = werr
	}
	return err
}

func syncMirror(ctx context.Context, r *Repo) error {
	u, err := ParseMirrorURL(r.Mirror)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	addrs, err := mirrorAddrs(ctx, u.Hostname())
	if err != nil {
		return err
	}
	// git connects to the address checked here, not to whatever the name
	// resolves to by then, and not through a proxy (which would resolve
	// the name itself); it doesn't follow redirects, speaks only https,
	// never asks for or sends credentials.
	ip := addrs[0].String()
	if addrs[0].Is6() {
		ip = "[" + ip + "]"
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	guard := []string{
		"-c", "http.curloptResolve=" + u.Hostname() + ":" + port + ":" + ip,
		"-c", "http.followRedirects=false", "-c", "http.proxy=",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "credential.helper=", "-c", "core.askPass=",
	}
	run := func(args ...string) ([]byte, error) {
		cmd := Command(ctx, r.Dir, append(guard, args...)...)
		cmd.Env = append(withoutProxy(cmd.Env), mirrorEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if len(msg) > 500 {
				msg = msg[len(msg)-500:]
			}
			if ctx.Err() != nil {
				msg = "timed out after " + mirrorTimeout.String()
			}
			return out, fmt.Errorf("git %s: %s", args[0], msg)
		}
		return out, nil
	}
	source := u.String()
	if _, err := run("fetch", "--quiet", "--prune", "--no-write-fetch-head", "--", source,
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return err
	}
	// Point HEAD at the source's default branch, so the summary shows it.
	out, err := run("ls-remote", "--symref", "--", source, "HEAD")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: "); ok {
			ref, _, _ = strings.Cut(ref, "\t")
			if strings.HasPrefix(ref, "refs/heads/") && validRev(strings.TrimPrefix(ref, "refs/heads/")) {
				if _, err := run("symbolic-ref", "HEAD", ref); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// withoutProxy drops proxy settings from env (curl reads them).
func withoutProxy(env []string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasSuffix(strings.ToLower(name), "_proxy") {
			out = append(out, kv)
		}
	}
	return out
}
