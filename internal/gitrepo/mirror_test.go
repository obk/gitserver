package gitrepo

import (
	"context"
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-git-server/internal/outbound"
)

func TestParseMirrorURL(t *testing.T) {
	for _, ok := range []string{"https://github.com/o/r.git", "https://git.example.com:8443/x", " https://h/r "} {
		if _, err := ParseMirrorURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://h/r", "git://h/r", "ssh://h/r", "file:///etc", "https://u:p@h/r", "https://u@h/r",
		"https:///r", "https://h/r?x=1", "https://h/r#x", "ext::sh -c x", "/srv/repo", "https://h/r\nx", "https://" + strings.Repeat("a", 500),
		"https://127.0.0.1/r", "https://[::1]/r", "https://169.254.169.254/latest", "https://localhost/r", "https://x.localhost/r", "https://10.0.0.5:8443/r"} {
		if _, err := ParseMirrorURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// gitServer serves the bare repositories in root over https, like a
// public git host.
func gitServer(t *testing.T, root string) *httptest.Server {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	srv := httptest.NewTLSServer(&cgi.Handler{Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}})
	t.Cleanup(srv.Close)
	return srv
}

func TestSyncMirror(t *testing.T) {
	src := t.TempDir()
	upstream := filepath.Join(src, "up.git")
	work := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(src, "init", "-q", "--bare", "-b", "trunk", upstream)
	git(work, "init", "-q", "-b", "trunk")
	os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644)
	git(work, "add", "a")
	git(work, "commit", "-qm", "one")
	git(work, "tag", "v1")
	git(work, "branch", "dev")
	git(work, "push", "-q", upstream, "trunk", "dev", "v1")
	srv := gitServer(t, src)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	reposDir := t.TempDir()
	if err := Create(reposDir, "alice", "m", "", true); err != nil {
		t.Fatal(err)
	}
	r, _ := Load(reposDir, "alice", "m")
	if err := SetMirror(r, "https://127.0.0.1:"+port+"/up.git"); err == nil {
		t.Fatal("loopback source accepted")
	}
	// Nor can a hand-edited mirror file get past the check when syncing.
	os.WriteFile(filepath.Join(r.Dir, mirrorFile), []byte("https://127.0.0.1:"+port+"/up.git\n"), 0o644)
	r, _ = Load(reposDir, "alice", "m")
	if !MirrorDue(r, time.Now()) {
		t.Fatal("a mirror never synced is not due")
	}

	// The real check refuses the test server's loopback address.
	ctx := context.Background()
	if err := SyncMirror(ctx, r); err == nil || !strings.Contains(err.Error(), "public internet") {
		t.Fatalf("synced from a loopback address: %v", err)
	}
	if st := ReadMirrorStatus(r.Dir); st == nil || st.Error == "" {
		t.Fatalf("failure not recorded: %+v", st)
	}

	SetMirror(r, "https://mirror.test:"+port+"/up.git")
	r, _ = Load(reposDir, "alice", "m")
	// Pretend mirror.test is public and points at the test server.
	mirrorAddrs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host != "mirror.test" {
			return nil, errors.New("unexpected host " + host)
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	mirrorEnv = []string{"GIT_SSL_NO_VERIFY=1"}
	defer func() { mirrorAddrs, mirrorEnv = outbound.PublicAddrs, nil }()
	if err := SyncMirror(ctx, r); err != nil {
		t.Fatal(err)
	}
	refs := func() string {
		out, _ := exec.Command("git", "--git-dir="+r.Dir, "for-each-ref", "--format=%(refname)").Output()
		return strings.TrimSpace(string(out))
	}
	if got := refs(); got != "refs/heads/dev\nrefs/heads/trunk\nrefs/tags/v1" {
		t.Fatalf("refs after sync:\n%s", got)
	}
	if head, _ := exec.Command("git", "--git-dir="+r.Dir, "symbolic-ref", "HEAD").Output(); strings.TrimSpace(string(head)) != "refs/heads/trunk" {
		t.Fatalf("HEAD not the source's: %s", head)
	}
	if st := ReadMirrorStatus(r.Dir); st == nil || st.Error != "" || MirrorDue(r, time.Now()) {
		t.Fatalf("success not recorded: %+v", st)
	}
	if !MirrorDue(r, time.Now().Add(MirrorInterval)) {
		t.Fatal("not due after the interval")
	}

	// A branch deleted at the source goes away here too.
	git(work, "push", "-q", upstream, ":dev")
	RequestMirrorSync(t.TempDir(), r)
	if !MirrorDue(r, time.Now()) {
		t.Fatal("requested sync not due")
	}
	if err := SyncMirror(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got := refs(); strings.Contains(got, "dev") {
		t.Fatalf("deleted branch kept:\n%s", got)
	}

	if err := SetMirror(r, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := Load(reposDir, "alice", "m"); r.Mirror != "" || ReadMirrorStatus(r.Dir) != nil {
		t.Fatal("stopping the mirror left its files")
	}
}
