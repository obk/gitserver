package web

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"go-git-server/internal/account"
	"go-git-server/internal/gitrepo"
	"go-git-server/internal/sshgit"
	"go-git-server/internal/store"
)

var testBinary string // gitserver built for SSH tests

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	dir, err := os.MkdirTemp("", "gitserver-test-bin-")
	if err != nil {
		panic(err)
	}
	testBinary = filepath.Join(dir, "gitserver")
	if out, err := exec.Command("go", "build", "-o", testBinary, "go-git-server/cmd/gitserver").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("go build: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testEnv runs a full server against a temporary data directory with users
// alice (admin) and bob, and repositories ~alice/pub (public),
// ~alice/secret (private) and ~bob/notes (private).
type testEnv struct {
	t       *testing.T
	srv     *httptest.Server
	s       *Server
	data    string
	work    string
	gitHome string
	secrets map[string]string // TOTP secrets
	pubKeys map[string]string // path of each user's public key file
}

func newTestKey(t *testing.T) string {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
}

func newTestEnv(t *testing.T) *testEnv {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	data := t.TempDir()
	s, err := NewServer(Config{DataDir: data, SiteName: "test", Insecure: true, SSHHost: "git.test",
		TrustProxy: true, RealIPHeader: "X-Real-IP"})
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, s: s, data: data, work: t.TempDir(), gitHome: t.TempDir(),
		secrets: map[string]string{}, pubKeys: map[string]string{}}
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)

	for _, name := range []string{"alice", "bob"} {
		hash, _ := account.HashPassword(name + "-password-123")
		line := newTestKey(t)
		key, err := sshgit.ParseKey(line + " " + name + "@test")
		if err != nil {
			t.Fatal(err)
		}
		e.secrets[name] = account.NewTOTPSecret()
		if err := s.store.Create(&store.User{Name: name, PasswordHash: hash, TOTPSecret: s.box.SealTOTP(name, e.secrets[name]), Admin: name == "alice", SSHKeys: []store.SSHKey{key}}); err != nil {
			t.Fatal(err)
		}
		e.pubKeys[name] = filepath.Join(e.gitHome, name+".pub")
		os.WriteFile(e.pubKeys[name], []byte(line+"\n"), 0o644)
	}
	reposDir := filepath.Join(data, "repos")
	for _, r := range []struct {
		owner, name string
		public      bool
	}{{"alice", "pub", true}, {"alice", "secret", false}, {"bob", "notes", false}} {
		if err := gitrepo.Create(reposDir, r.owner, r.name, "repo "+r.name, r.public); err != nil {
			t.Fatal(err)
		}
	}

	// A stand-in for ssh that does what sshd does on the server: ask
	// "gitserver ssh-keys" for the authorized_keys line of the user's key,
	// then run its forced command with SSH_ORIGINAL_COMMAND set.
	fake := filepath.Join(e.gitHome, "ssh")
	script := `#!/bin/sh
for last; do :; done
set -- $(cat "$GS_PUBKEY")
line=$("` + testBinary + `" ssh-keys -data "` + data + `" git "$1" "$2")
[ -n "$line" ] || { echo "Permission denied (publickey)." >&2; exit 255; }
case "$line" in restrict,command=*) ;; *) echo "bad line: $line" >&2; exit 255;; esac
cmd=${line#*command=\"}; cmd=${cmd%%\"*}
SSH_ORIGINAL_COMMAND="$last" exec $cmd
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// git runs git as the given user ("" for none) over the fake SSH.
func (e *testEnv) git(user, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	pub := e.pubKeys[user]
	if pub == "" {
		pub = filepath.Join(e.gitHome, "unregistered.pub")
		os.WriteFile(pub, []byte(newTestKey(e.t)+"\n"), 0o644)
	}
	cmd.Env = append(os.Environ(), "HOME="+e.gitHome, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND="+filepath.Join(e.gitHome, "ssh"), "GS_PUBKEY="+pub,
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *testEnv) pushInitial(owner, repo string) {
	dir := filepath.Join(e.work, owner+"-"+repo+"-src")
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "README"), []byte("hello <script>alert(1)</script>\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\n"), 0o644)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"commit", "-q", "-m", "initial <b>commit</b>"},
		{"push", "-q", "git@git.test:~" + owner + "/" + repo, "main"},
	} {
		if out, err := e.git(owner, dir, args...); err != nil {
			e.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func (e *testEnv) get(c *http.Client, path string) (int, string) {
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *testEnv) post(c *http.Client, path string, form url.Values, ip string) (int, string) {
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ip != "" {
		req.Header.Set("X-Real-IP", ip)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *testEnv) code(user string) string {
	key, _ := account.Base32.DecodeString(e.secrets[user])
	return account.HOTP(key, uint64(time.Now().Unix()/account.TOTPPeriod))
}

// login returns a client with a session for user.
func (e *testEnv) login(user string) *http.Client {
	e.s.store.Update(user, func(u *store.User) error { u.TOTPLast = 0; return nil }) // allow reusing the current code
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	status, body := e.post(c, "/login", url.Values{"username": {user}, "password": {user + "-password-123"}, "code": {e.code(user)}}, "")
	if status != 200 || !strings.Contains(body, "Log out") {
		e.t.Fatalf("login %s failed (%d):\n%s", user, status, body)
	}
	return c
}

func csrfToken(t *testing.T, body string) string {
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no csrf token in page")
	}
	return m[1]
}

func TestGitSSH(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	e.pushInitial("alice", "secret")
	e.pushInitial("bob", "notes")

	if out, err := e.git("bob", e.work, "clone", "-q", "git@git.test:~alice/pub", "bob-pub"); err != nil {
		t.Fatalf("bob cannot clone public repo: %v\n%s", err, out)
	}
	out, err := e.git("bob", e.work, "clone", "-q", "git@git.test:~alice/secret", "bob-secret")
	if err == nil || !strings.Contains(out, "not found or access denied") {
		t.Fatalf("bob cloned alice's private repo: %v\n%s", err, out)
	}
	if _, err := e.git("alice", e.work, "clone", "-q", "git@git.test:~bob/notes", "alice-notes"); err == nil {
		t.Fatal("admin alice cloned bob's private repo")
	}
	if _, err := e.git("", e.work, "clone", "-q", "git@git.test:~alice/pub", "nokey"); err == nil {
		t.Fatal("clone without a registered key succeeded")
	}
	if out, err := e.git("alice", e.work, "clone", "-q", "ssh://git@git.test/~alice/secret.git", "alice-secret"); err != nil {
		t.Fatalf("owner cannot clone (ssh:// URL): %v\n%s", err, out)
	}

	// Only the owner may push, even to a public repo.
	dir := filepath.Join(e.work, "bob-pub")
	os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o644)
	e.git("bob", dir, "add", "x")
	e.git("bob", dir, "commit", "-q", "-m", "x")
	if out, err := e.git("bob", dir, "push", "-q", "origin", "main"); err == nil {
		t.Fatalf("bob pushed to alice's repo\n%s", out)
	}
	if out, err := e.git("alice", dir, "push", "-q", "origin", "main"); err != nil {
		t.Fatalf("owner push failed: %v\n%s", err, out)
	}

	// The authorized_keys line is restricted and only for the git user.
	line, _ := os.ReadFile(e.pubKeys["alice"])
	f := strings.Fields(string(line))
	out2, _ := exec.Command(testBinary, "ssh-keys", "-data", e.data, "git", f[0], f[1]).Output()
	if !strings.HasPrefix(string(out2), `restrict,command="`) || !strings.Contains(string(out2), " ssh-serve -data "+e.data+" alice\" ") {
		t.Fatalf("bad authorized_keys line: %s", out2)
	}
	if out3, _ := exec.Command(testBinary, "ssh-keys", "-data", e.data, "root", f[0], f[1]).Output(); len(out3) != 0 {
		t.Fatalf("key accepted for another system user: %s", out3)
	}
	// No shell: an interactive login only gets a greeting.
	cmd := exec.Command(testBinary, "ssh-serve", "-data", e.data, "alice")
	cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND=")
	if out4, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out4), "no shell access") {
		t.Fatalf("interactive ssh: %v %s", err, out4)
	}
	cmd = exec.Command(testBinary, "ssh-serve", "-data", e.data, "alice")
	cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND=sh -c id")
	if out5, err := cmd.CombinedOutput(); err == nil || strings.Contains(string(out5), "uid=") {
		t.Fatalf("arbitrary command ran: %s", out5)
	}

	// Deleting the user revokes access at once.
	e.s.store.Delete("bob")
	if _, err := e.git("bob", e.work, "clone", "-q", "git@git.test:~alice/pub", "bob-pub2"); err == nil {
		t.Fatal("deleted user can still clone")
	}
}

func TestWebAccess(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	e.pushInitial("alice", "secret")
	e.pushInitial("bob", "notes")
	anon := &http.Client{}

	_, body := e.get(anon, "/")
	if !strings.Contains(body, "/~alice/pub/") || strings.Contains(body, "secret") || strings.Contains(body, "notes") {
		t.Fatalf("anonymous landing page wrong:\n%s", body)
	}
	for _, p := range []string{"/~alice/secret/", "/~alice/secret/tree", "/~alice/secret/raw/README", "/~bob/notes/", "/~nobody/x/", "/alice/pub/"} {
		if code, _ := e.get(anon, p); code != http.StatusNotFound {
			t.Errorf("anonymous %s: %d, want 404", p, code)
		}
	}
	code, body := e.get(anon, "/~alice/pub/")
	if code != 200 || !strings.Contains(body, "git@git.test:~alice/pub") || !strings.Contains(body, "initial &lt;b&gt;commit&lt;/b&gt;") {
		t.Fatalf("summary page wrong (%d):\n%s", code, body)
	}
	hash := regexp.MustCompile(`/~alice/pub/commit/([0-9a-f]{40})`).FindStringSubmatch(body)
	if hash == nil {
		t.Fatal("no commit link")
	}
	if code, body := e.get(anon, "/~alice/pub/commit/"+hash[1]); code != 200 || !strings.Contains(body, `<span class="pfx">&#43;</span><span class="hl-kn">package</span>`) {
		t.Fatalf("commit page wrong (%d):\n%s", code, body)
	}
	if code, body := e.get(anon, "/~alice/pub/tree/README"); code != 200 || strings.Contains(body, "<script>") {
		t.Fatalf("README not escaped (%d)", code)
	}
	resp, _ := anon.Get(e.srv.URL + "/~alice/pub/raw/README")
	if resp.Header.Get("Content-Security-Policy") != "sandbox; default-src 'none'" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("raw headers unsafe: %v", resp.Header)
	}
	resp.Body.Close()
	if code, _ := e.get(anon, "/~alice/pub/tree/../../etc/passwd"); code == 200 {
		t.Fatal("path traversal")
	}

	// Private repos are visible to their owner only, not even to admins.
	bob := e.login("bob")
	_, body = e.get(bob, "/")
	if !strings.Contains(body, "/~bob/notes/") || strings.Contains(body, "/~alice/secret/") {
		t.Fatalf("bob's index wrong:\n%s", body)
	}
	if code, _ := e.get(bob, "/~alice/secret/"); code != http.StatusNotFound {
		t.Fatalf("bob sees alice's private repo: %d", code)
	}
	_, body = e.get(bob, "/~alice")
	if !strings.Contains(body, "/~alice/pub/") || strings.Contains(body, "secret") {
		t.Fatalf("alice's user page as bob:\n%s", body)
	}
	alice := e.login("alice")
	if code, _ := e.get(alice, "/~bob/notes/"); code != http.StatusNotFound {
		t.Fatalf("admin sees bob's private repo: %d", code)
	}
	if code, _ := e.get(alice, "/~alice/secret/"); code != 200 {
		t.Fatalf("owner cannot see own private repo: %d", code)
	}
	// Only the owner gets the settings tab and can change settings.
	_, body = e.get(bob, "/~alice/pub/")
	if strings.Contains(body, "/~alice/pub/settings") {
		t.Fatal("non-owner sees settings tab")
	}
	csrf := csrfToken(t, body)
	if code, _ := e.post(bob, "/~alice/pub/settings", url.Values{"csrf": {csrf}, "visibility": {"private"}}, ""); code != http.StatusNotFound {
		t.Fatalf("non-owner changed settings: %d", code)
	}
	if code, _ := e.post(bob, "/~alice/pub/delete", url.Values{"csrf": {csrf}, "confirm": {"pub"}}, ""); code != http.StatusNotFound {
		t.Fatalf("non-owner deleted repo: %d", code)
	}
	if _, err := gitrepo.Load(filepath.Join(e.data, "repos"), "alice", "pub"); err != nil {
		t.Fatal("repo gone")
	}
}

func TestRepoManagement(t *testing.T) {
	e := newTestEnv(t)
	bob := e.login("bob")
	_, body := e.get(bob, "/create")
	csrf := csrfToken(t, body)
	if code, _ := e.post(bob, "/create", url.Values{"name": {"proj"}, "description": {"d"}, "visibility": {"private"}}, ""); code != http.StatusForbidden {
		t.Fatalf("create without CSRF: %d", code)
	}
	for _, bad := range []string{"", "../x", "a/b", "x.git", ".hidden"} {
		if code, _ := e.post(bob, "/create", url.Values{"csrf": {csrf}, "name": {bad}}, ""); code != http.StatusBadRequest {
			t.Errorf("bad name %q: %d", bad, code)
		}
	}
	code, body := e.post(bob, "/create", url.Values{"csrf": {csrf}, "name": {"proj"}, "description": {"my project"}, "visibility": {"private"}}, "")
	if code != 200 || !strings.Contains(body, "git@git.test:~bob/proj") || !strings.Contains(body, "private") {
		t.Fatalf("create failed (%d):\n%s", code, body)
	}
	if code, _ := e.get(&http.Client{}, "/~bob/proj/"); code != http.StatusNotFound {
		t.Fatal("new private repo visible anonymously")
	}
	if code, _ := e.post(bob, "/~bob/proj/settings", url.Values{"csrf": {csrf}, "description": {"public now"}, "visibility": {"public"}}, ""); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	if code, _ := e.get(&http.Client{}, "/~bob/proj/"); code != 200 {
		t.Fatal("repo not public after settings change")
	}
	if code, _ := e.post(bob, "/~bob/proj/delete", url.Values{"csrf": {csrf}, "confirm": {"wrong"}}, ""); code != http.StatusBadRequest {
		t.Fatalf("delete with wrong confirmation: %d", code)
	}
	e.post(bob, "/~bob/proj/delete", url.Values{"csrf": {csrf}, "confirm": {"proj"}}, "")
	if _, err := os.Stat(gitrepo.Dir(filepath.Join(e.data, "repos"), "bob", "proj")); !os.IsNotExist(err) {
		t.Fatal("repo not deleted")
	}
}

func TestSSHKeysPage(t *testing.T) {
	e := newTestEnv(t)
	bob := e.login("bob")
	_, body := e.get(bob, "/settings/keys")
	csrf := csrfToken(t, body)
	u, _ := e.s.store.Get("bob")
	if code, body := e.post(bob, "/settings/keys/delete", url.Values{"csrf": {csrf}, "id": {u.SSHKeys[0].ID}}, ""); code != http.StatusBadRequest || !strings.Contains(body, "only SSH key") {
		t.Fatalf("last key deleted: %d", code)
	}
	alice, _ := e.s.store.Get("alice")
	if code, body := e.post(bob, "/settings/keys", url.Values{"csrf": {csrf}, "key": {alice.SSHKeys[0].Key}}, ""); code != http.StatusBadRequest || !strings.Contains(body, "already registered") {
		t.Fatalf("another user's key accepted: %d", code)
	}
	e.post(bob, "/settings/keys", url.Values{"csrf": {csrf}, "key": {newTestKey(t) + " second"}}, "")
	if u, _ := e.s.store.Get("bob"); len(u.SSHKeys) != 2 {
		t.Fatalf("key not added: %d keys", len(u.SSHKeys))
	}

	// An account without keys is sent to the key page until it adds one.
	e.s.store.Update("bob", func(u *store.User) error { u.SSHKeys = nil; return nil })
	noFollow := &http.Client{Jar: bob.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := noFollow.Get(e.srv.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/keys" {
		t.Fatalf("keyless user not redirected: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLogin(t *testing.T) {
	e := newTestEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	good := e.code("alice")
	if code, _ := e.post(c, "/login", url.Values{"username": {"alice"}, "password": {"wrong-password-x"}, "code": {good}}, ""); code != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", code)
	}
	if code, _ := e.post(c, "/login", url.Values{"username": {"alice"}, "password": {"alice-password-123"}, "code": {good}}, ""); code != 200 {
		t.Fatalf("login failed: %d", code)
	}
	jar2, _ := cookiejar.New(nil)
	if code, _ := e.post(&http.Client{Jar: jar2}, "/login", url.Values{"username": {"alice"}, "password": {"alice-password-123"}, "code": {good}}, ""); code != http.StatusUnauthorized {
		t.Fatalf("TOTP replay accepted: %d", code)
	}

	// Open redirect: next must stay on this site.
	e.s.store.Update("alice", func(u *store.User) error { u.TOTPLast = 0; return nil })
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", e.srv.URL+"/login", strings.NewReader(url.Values{"username": {"alice"},
		"password": {"alice-password-123"}, "code": {good}, "next": {"/\t/evil.example"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ := noFollow.Do(req)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("open redirect: Location %q", loc)
	}

	// Cross-origin POSTs are rejected.
	req, _ = http.NewRequest("POST", e.srv.URL+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ = c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", resp.StatusCode)
	}

	// A password change (even from the CLI) ends existing sessions.
	if code, _ := e.get(c, "/settings/keys"); code != 200 {
		t.Fatalf("session not working: %d", code)
	}
	hash, _ := account.HashPassword("new-password-456")
	e.s.store.Update("alice", func(u *store.User) error { u.PasswordHash = hash; return nil })
	if _, body := e.get(c, "/"); strings.Contains(body, "Log out") {
		t.Fatal("session survived a password change")
	}
}

func TestLoginRateLimits(t *testing.T) {
	e := newTestEnv(t)
	try := func(ip, password, code string) int {
		jar, _ := cookiejar.New(nil)
		s, _ := e.post(&http.Client{Jar: jar}, "/login", url.Values{"username": {"alice"}, "password": {password}, "code": {code}}, ip)
		return s
	}
	// A stranger guessing passwords is limited per IP...
	var last int
	for range 12 {
		last = try("198.51.100.7", "guess-guess-guess", "123456")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("no per-IP limit: %d", last)
	}
	// ...but cannot lock alice out: she still logs in from her own IP.
	e.s.store.Update("alice", func(u *store.User) error { u.TOTPLast = 0; return nil })
	if s := try("203.0.113.5", "alice-password-123", e.code("alice")); s != 200 {
		t.Fatalf("alice locked out by a stranger: %d", s)
	}
	// Wrong codes after a correct password do count per account, from any IP.
	for i := range 10 {
		try(fmt.Sprintf("192.0.2.%d", i+1), "alice-password-123", "000000")
	}
	e.s.store.Update("alice", func(u *store.User) error { u.TOTPLast = 0; return nil })
	if s := try("192.0.2.200", "alice-password-123", e.code("alice")); s != http.StatusTooManyRequests {
		t.Fatalf("TOTP guessing not limited per account: %d", s)
	}
}

func TestInviteSignup(t *testing.T) {
	e := newTestEnv(t)
	code, _, err := e.s.store.CreateInvite("alice", false, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	if status, _ := e.get(c, "/signup?code=wrong"); status != http.StatusNotFound {
		t.Fatalf("bad invite code: %d", status)
	}
	if status, body := e.get(c, "/signup?code="+url.QueryEscape(code)); status != 200 || !strings.Contains(body, `name="key"`) {
		t.Fatalf("signup form (%d):\n%s", status, body)
	}
	key := newTestKey(t)
	form := func(name, pw, k string) url.Values {
		return url.Values{"code": {code}, "username": {name}, "password": {pw}, "password2": {pw}, "key": {k}}
	}
	alice, _ := e.s.store.Get("alice")
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{form("alice", "long-enough-pw", key), "taken"},
		{form("carol", "short", key), "12 characters"},
		{form("admin", "long-enough-pw", key), "reserved"},
		{form("python", "long-enough-pw", key), "reserved"},
		{form("carol", "long-enough-pw", ""), "SSH key"},
		{form("carol", "long-enough-pw", "not a key"), "SSH key"},
		{form("carol", "long-enough-pw", alice.SSHKeys[0].Key), "already registered"},
	} {
		if status, body := e.post(c, "/signup", tc.form, ""); status == 200 || !strings.Contains(body, tc.want) {
			t.Errorf("signup %v: %d, want error containing %q", tc.form, status, tc.want)
		}
	}
	status, body := e.post(c, "/signup", form("carol", "carol-password-1", key+" carol@laptop"), "")
	token := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(body)
	secret := regexp.MustCompile(`<pre class="secret">([A-Z2-7]+)</pre>`).FindStringSubmatch(body)
	if status != 200 || token == nil || secret == nil || !strings.Contains(body, "<svg") {
		t.Fatalf("TOTP step missing (%d):\n%s", status, body)
	}
	if _, err := e.s.store.Get("carol"); err == nil {
		t.Fatal("user created before TOTP was confirmed")
	}
	if status, _ := e.post(c, "/signup/confirm", url.Values{"token": {token[1]}, "totp": {"000000"}}, ""); status != http.StatusBadRequest {
		t.Fatalf("wrong TOTP accepted: %d", status)
	}
	k, _ := account.Base32.DecodeString(secret[1])
	good := account.HOTP(k, uint64(time.Now().Unix()/account.TOTPPeriod))
	status, body = e.post(c, "/signup/confirm", url.Values{"token": {token[1]}, "totp": {good}}, "")
	if status != 200 || !strings.Contains(body, "~carol") {
		t.Fatalf("signup did not log in (%d):\n%s", status, body)
	}
	if !strings.Contains(body, "Your recovery codes") || len(recoveryCodeRe.FindAllString(body, -1)) != 10 {
		t.Fatalf("new account got no recovery codes:\n%s", body)
	}
	if l, _ := e.s.store.RecentLogins("carol", 5); len(l) != 1 || l[0].Method != "signup" {
		t.Fatalf("signup not recorded as a login: %+v", l)
	}
	u, err := e.s.store.Get("carol")
	if err != nil || u.Admin || u.InvitedBy != "alice" || len(u.SSHKeys) != 1 || u.SSHKeys[0].Comment != "carol@laptop" {
		t.Fatalf("bad user record: %+v, %v", u, err)
	}
	if plain, err := e.s.box.OpenTOTP("carol", u.TOTPSecret); err != nil || plain != secret[1] || strings.Contains(u.TOTPSecret, secret[1]) {
		t.Fatalf("signup did not store the 2FA secret encrypted: %q, %v", u.TOTPSecret, err)
	}
	if status, _ := e.get(c, "/settings/invites"); status != http.StatusNotFound {
		t.Fatalf("non-admin reached invites page: %d", status)
	}
	if status, _ := e.get(&http.Client{}, "/signup?code="+url.QueryEscape(code)); status != http.StatusNotFound {
		t.Fatalf("used invite accepted: %d", status)
	}
	if status, _ := e.post(c, "/signup/confirm", url.Values{"token": {token[1]}, "totp": {good}}, ""); status == 200 {
		t.Fatal("signup token reusable")
	}
}

func TestInviteExpiryAndRevoke(t *testing.T) {
	e := newTestEnv(t)
	code, _, _ := e.s.store.CreateInvite("alice", true, -time.Minute)
	if _, err := e.s.store.LookupInvite(code); err == nil {
		t.Fatal("expired invite accepted")
	}
	code, inv, _ := e.s.store.CreateInvite("alice", true, time.Hour)
	if err := e.s.store.RevokeInvite(inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.store.LookupInvite(code); err == nil {
		t.Fatal("revoked invite accepted")
	}
}

func TestLandingPage(t *testing.T) {
	e := newTestEnv(t)
	_, body := e.get(&http.Client{}, "/")
	if !strings.Contains(body, `class="hero"`) || !strings.Contains(body, "Public repositories") {
		t.Fatalf("landing page wrong:\n%s", body)
	}
	os.WriteFile(filepath.Join(e.data, "intro.md"), []byte("Hosting for **friends**.\n\n<script>x</script>"), 0o644)
	_, body = e.get(&http.Client{}, "/")
	if !strings.Contains(body, "<strong>friends</strong>") || strings.Contains(body, "<script>x") {
		t.Fatalf("intro.md not rendered safely:\n%s", body)
	}
}

func TestGitHTTPSClone(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	e.pushInitial("alice", "secret")

	for _, path := range []string{"/~alice/pub", "/~alice/pub.git"} {
		dir := "https-" + strings.Trim(strings.ReplaceAll(path, "/", "-"), "-~")
		if out, err := e.git("", e.work, "clone", "-q", e.srv.URL+path, dir); err != nil {
			t.Fatalf("anonymous HTTPS clone of %s: %v\n%s", path, err, out)
		}
		if _, err := os.Stat(filepath.Join(e.work, dir, "README")); err != nil {
			t.Fatalf("clone of %s has no files", path)
		}
	}
	// A private repo looks exactly like a missing one.
	outPriv, errPriv := e.git("", e.work, "clone", "-q", e.srv.URL+"/~alice/secret", "p1")
	outMiss, _ := e.git("", e.work, "clone", "-q", e.srv.URL+"/~alice/nope", "p2")
	if errPriv == nil || !strings.Contains(outPriv, "Repository not found") || !strings.Contains(outMiss, "Repository not found") {
		t.Fatalf("private repo over HTTPS: %v\n%s\n%s", errPriv, outPriv, outMiss)
	}
	// Pushing over HTTPS is refused with a pointer to SSH; nothing changes.
	dir := filepath.Join(e.work, "https-alice-pub")
	os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o644)
	e.git("", dir, "add", "x")
	e.git("", dir, "commit", "-q", "-m", "x")
	out, err := e.git("", dir, "push", "origin", "main")
	if err == nil || !strings.Contains(out, "push over SSH") || !strings.Contains(out, "git@git.test:~alice/pub") {
		t.Fatalf("HTTPS push: %v\n%s", err, out)
	}
	if log, _ := e.git("alice", e.work, "ls-remote", "git@git.test:~alice/pub"); strings.Count(log, "refs/heads/main") != 1 {
		t.Fatal("ls-remote over SSH failed")
	}
	// Only the smart protocol: no dumb file access, no direct object reads.
	for _, p := range []string{"/~alice/pub/info/refs", "/~alice/pub.git/info/refs?service=bogus", "/~alice/pub.git/HEAD", "/~alice/pub.git/objects/info/packs"} {
		code, _ := e.get(&http.Client{}, p)
		if code == 200 {
			t.Errorf("GET %s: 200", p)
		}
	}
	// The summary page shows the HTTPS URL for public repos only.
	_, body := e.get(&http.Client{}, "/~alice/pub/")
	if !strings.Contains(body, e.srv.URL+"/~alice/pub</code>") || !strings.Contains(body, "git@git.test:~alice/pub") {
		t.Fatalf("clone URLs missing on summary page:\n%s", body)
	}
	alice := e.login("alice")
	if _, body := e.get(alice, "/~alice/secret/"); strings.Contains(body, "HTTPS <span") {
		t.Fatal("HTTPS clone URL shown for a private repo")
	}
}

// A deleted user's name cannot be taken by a new account, which would
// inherit their repositories, even when the repositories are gone too.
func TestDeletedUserNameNotReused(t *testing.T) {
	e := newTestEnv(t)
	if err := e.s.store.Delete("bob"); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(e.data, "repos", "bob"))
	code, _, _ := e.s.store.CreateInvite("alice", false, time.Hour)
	form := url.Values{"code": {code}, "username": {"bob"}, "password": {"bob-password-456"},
		"password2": {"bob-password-456"}, "key": {newTestKey(t)}}
	if status, body := e.post(&http.Client{}, "/signup", form, ""); status == 200 || !strings.Contains(body, "taken") {
		t.Fatalf("signup as deleted bob: %d", status)
	}
}

// Commits that no branch or tag reaches any more (a force-pushed secret)
// are not served on the web or over git.
func TestUnreachableCommits(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	dir := filepath.Join(e.work, "alice-pub-src")
	run := func(args ...string) string {
		out, err := e.git("alice", dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("hunter2\n"), 0o644)
	run("add", "secret.txt")
	run("commit", "-q", "-m", "oops")
	run("push", "-q", "git@git.test:~alice/pub", "main")
	leak := run("rev-parse", "HEAD")
	run("reset", "-q", "--hard", "HEAD~1")
	good := run("rev-parse", "HEAD")
	// Move the branch back on the server itself: a force push would delete
	// the commit from disk (TestForcePushCleanup), but older repositories or
	// a failed cleanup can still hold unreachable commits.
	repoDir := gitrepo.Dir(filepath.Join(e.data, "repos"), "alice", "pub")
	if out, err := exec.Command("git", "--git-dir="+repoDir, "update-ref", "refs/heads/main", good).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}

	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{"/commit/" + leak, "/commit/" + leak[:4], "/tree?h=" + leak, "/tree/secret.txt?h=" + leak[:7],
		"/raw/secret.txt?h=" + leak, "/log?h=" + leak} {
		if code, _ := e.get(anon, "/~alice/pub"+p); code != http.StatusNotFound {
			t.Errorf("unreachable commit served at %s: %d", p, code)
		}
	}
	// Short hashes of reachable commits redirect to the full one.
	resp, err := anon.Get(e.srv.URL + "/~alice/pub/commit/" + good[:7])
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/~alice/pub/commit/"+good {
		t.Fatalf("short hash: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _ := e.get(anon, "/~alice/pub/tree?h="+good); code != 200 {
		t.Fatalf("reachable commit not served: %d", code)
	}

	// Protocol v2 would hand out any object by hash; it is not offered.
	for i, remote := range []string{"git@git.test:~alice/pub", e.srv.URL + "/~alice/pub"} {
		d := filepath.Join(e.work, fmt.Sprintf("fetch-%d", i))
		os.MkdirAll(d, 0o755)
		e.git("alice", d, "init", "-q")
		if out, err := e.git("alice", d, "-c", "protocol.version=2", "fetch", "-q", remote, leak); err == nil {
			t.Errorf("fetched unreachable commit from %s\n%s", remote, out)
		}
		if out, err := e.git("alice", d, "-c", "protocol.version=2", "fetch", "-q", remote, "main"); err != nil {
			t.Errorf("v2 client cannot fetch from %s: %v\n%s", remote, err, out)
		}
	}
}

// Wrong 2FA codes after a correct password are reported at the next login.
// Wrong 2FA codes after a correct password are reported at the next login,
// even if the server restarted in between.
func TestCodeFailureNotice(t *testing.T) {
	e := newTestEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	for range 2 {
		e.post(c, "/login", url.Values{"username": {"alice"}, "password": {"alice-password-123"}, "code": {"000000"}}, "")
	}
	e.post(c, "/login", url.Values{"username": {"alice"}, "password": {"wrong-password-x"}, "code": {"000000"}}, "")
	// A restart (or a crash) between the attack and the next login must not
	// hide it: the counts are in the database.
	s2, err := NewServer(e.s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Close()
	e.s, e.srv = s2, httptest.NewServer(s2.Handler())
	t.Cleanup(e.srv.Close)
	status, body := e.post(c, "/login", url.Values{"username": {"alice"}, "password": {"alice-password-123"}, "code": {e.code("alice")}}, "")
	if status != 200 || !strings.Contains(body, "2 wrong 2FA code(s)") || !strings.Contains(body, "change it now") {
		t.Fatalf("no notice after wrong codes (%d):\n%s", status, body)
	}
	// Reported once: the next login is quiet.
	e.s.store.Update("alice", func(u *store.User) error { u.TOTPLast = 0; return nil })
	jar2, _ := cookiejar.New(nil)
	if _, body := e.post(&http.Client{Jar: jar2}, "/login", url.Values{"username": {"alice"}, "password": {"alice-password-123"}, "code": {e.code("alice")}}, ""); strings.Contains(body, "wrong 2FA") {
		t.Fatal("notice shown twice")
	}
}

func TestLimiterTake(t *testing.T) {
	l := newLimiter(2, time.Minute)
	if !l.take("k") || !l.take("k") || l.take("k") {
		t.Fatal("take does not stop at the limit")
	}
	l.refund("k")
	if !l.take("k") || l.take("k") {
		t.Fatal("refund does not free exactly one attempt")
	}
}

func TestRawSVG(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	dir := filepath.Join(e.work, "alice-pub-src")
	os.WriteFile(filepath.Join(dir, "x.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o644)
	e.git("alice", dir, "add", "x.svg")
	e.git("alice", dir, "commit", "-q", "-m", "svg")
	if out, err := e.git("alice", dir, "push", "-q", "git@git.test:~alice/pub", "main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	for dest, want := range map[string]string{"document": "attachment", "image": "", "": ""} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/~alice/pub/raw/x.svg", nil)
		if dest != "" {
			req.Header.Set("Sec-Fetch-Dest", dest)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Content-Disposition"); resp.StatusCode != 200 || got != want {
			t.Errorf("Sec-Fetch-Dest %q: %d, Content-Disposition %q, want %q", dest, resp.StatusCode, got, want)
		}
	}
}

func TestSSHBusyUser(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")

	// git inherits the lock: while a running upload-pack waits for the
	// client, only MaxGitPerUser-1 slots are left.
	cmd := exec.Command(testBinary, "ssh-serve", "-data", e.data, "alice")
	cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND=git-upload-pack '~alice/pub'")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stdout, buf); err != nil { // git is running: ref advertisement started
		t.Fatal(err)
	}
	free := 0
	var fds []int
	for {
		fd, ok, _ := sshgit.AcquireUserSlot(e.data, "alice")
		if !ok {
			break
		}
		free++
		fds = append(fds, fd)
	}
	for _, fd := range fds {
		syscall.Close(fd)
	}
	stdin.Close()
	io.Copy(io.Discard, stdout)
	cmd.Wait()
	if free != sshgit.MaxGitPerUser-1 {
		t.Fatalf("%d slots free while git runs, want %d", free, sshgit.MaxGitPerUser-1)
	}
	for range sshgit.MaxGitPerUser {
		if _, ok, _ := sshgit.AcquireUserSlot(e.data, "alice"); !ok {
			t.Fatal("could not take slot")
		}
	}
	out, err := e.git("alice", e.work, "clone", "-q", "git@git.test:~alice/pub", "busy")
	if err == nil || !strings.Contains(out, "too many git operations") {
		t.Fatalf("clone with all slots taken: %v\n%s", err, out)
	}
	if out, err := e.git("bob", e.work, "clone", "-q", "git@git.test:~alice/pub", "bob-ok"); err != nil {
		t.Fatalf("bob blocked by alice's slots: %v\n%s", err, out)
	}
}

func TestClonesPerIP(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	key := ipKey("198.51.100.9")
	for range clonesPerIP {
		e.s.clones.acquire(key, clonesPerIP)
	}
	clone := func(ip, dir string) (string, error) {
		return e.git("", e.work, "-c", "http.extraHeader=X-Real-IP: "+ip, "clone", "-q", e.srv.URL+"/~alice/pub", dir)
	}
	if out, err := clone("198.51.100.9", "c1"); err == nil || !strings.Contains(out, "Too many clones from your address") {
		t.Fatalf("clone over the per-IP limit: %v\n%s", err, out)
	}
	if out, err := clone("198.51.100.10", "c2"); err != nil {
		t.Fatalf("other client blocked: %v\n%s", err, out)
	}
	e.s.clones.release(key)
	if out, err := clone("198.51.100.9", "c3"); err != nil {
		t.Fatalf("clone after a slot was freed: %v\n%s", err, out)
	}
	if n := e.s.clones.m[key]; n != clonesPerIP-1 {
		t.Fatalf("finished clones not released: %d running", n)
	}
}

func TestRepoLimit(t *testing.T) {
	e := newTestEnv(t)
	for i := len(mustList(t, e, "bob")); i < maxReposPerUser; i++ {
		dir := gitrepo.Dir(filepath.Join(e.data, "repos"), "bob", fmt.Sprintf("r%d", i))
		os.MkdirAll(dir, 0o750)
		os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	}
	bob := e.login("bob")
	_, body := e.get(bob, "/create")
	status, body := e.post(bob, "/create", url.Values{"csrf": {csrfToken(t, body)}, "name": {"one-more"}}, "")
	if status != http.StatusBadRequest || !strings.Contains(body, "limit of") {
		t.Fatalf("repo over the limit: %d", status)
	}
}

func mustList(t *testing.T, e *testEnv, owner string) []*gitrepo.Repo {
	repos, err := gitrepo.List(filepath.Join(e.data, "repos"), owner)
	if err != nil {
		t.Fatal(err)
	}
	return repos
}

var recoveryCodeRe = regexp.MustCompile(`[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}`)

// Recovery codes are shown once, and each lets its owner log in once
// without the authenticator.
func TestRecoveryCodes(t *testing.T) {
	e := newTestEnv(t)
	alice := e.login("alice")
	_, body := e.get(alice, "/settings/2fa")
	if !strings.Contains(body, "You have no recovery codes") {
		t.Fatalf("no warning without codes:\n%s", body)
	}
	csrf := csrfToken(t, body)
	if status, _ := e.post(alice, "/settings/2fa/recovery", url.Values{"csrf": {csrf}, "current": {"wrong-password-x"}}, ""); status != http.StatusUnauthorized {
		t.Fatalf("codes made with a wrong password: %d", status)
	}
	status, body := e.post(alice, "/settings/2fa/recovery", url.Values{"csrf": {csrf}, "current": {"alice-password-123"}}, "")
	codes := recoveryCodeRe.FindAllString(body, -1)
	if status != 200 || len(codes) != 10 || !strings.Contains(body, "shown only this once") {
		t.Fatalf("codes not shown (%d, %d codes):\n%s", status, len(codes), body)
	}
	if _, body := e.get(alice, "/settings/2fa"); recoveryCodeRe.MatchString(body) || !strings.Contains(body, "<b>10</b> unused") {
		t.Fatal("codes shown twice, or not counted")
	}

	login := func(code string) (int, string) {
		jar, _ := cookiejar.New(nil)
		return e.post(&http.Client{Jar: jar}, "/login", url.Values{"username": {"alice"}, "password": {"alice-password-123"}, "code": {code}}, "")
	}
	// Typed in capitals and without dashes, it is still the code.
	status, body = login(strings.ToUpper(strings.ReplaceAll(codes[3], "-", " ")))
	if status != 200 || !strings.Contains(body, "logged in with a recovery code") || !strings.Contains(body, "9 left") {
		t.Fatalf("recovery code login (%d):\n%s", status, body)
	}
	if status, _ := login(codes[3]); status != http.StatusUnauthorized {
		t.Fatalf("recovery code worked twice: %d", status)
	}
	// The failed reuse counts as a wrong code entered with the password.
	if status, body := login(codes[4]); status != 200 || !strings.Contains(body, "1 wrong 2FA code(s)") {
		t.Fatalf("reused code not reported (%d):\n%s", status, body)
	}
	// Without the password, a recovery code is useless.
	jar, _ := cookiejar.New(nil)
	if status, _ := e.post(&http.Client{Jar: jar}, "/login", url.Values{"username": {"alice"}, "password": {"wrong-password-x"}, "code": {codes[5]}}, ""); status != http.StatusUnauthorized {
		t.Fatalf("recovery code without password: %d", status)
	}
	if n, _ := e.s.store.RecoveryCodesLeft("alice"); n != 8 {
		t.Fatalf("%d codes left, want 8", n)
	}
}

// A user who lost their phone sets up a new authenticator themselves.
func TestNewAuthenticator(t *testing.T) {
	e := newTestEnv(t)
	alice := e.login("alice")
	other := e.login("alice") // another browser
	_, body := e.get(alice, "/settings/2fa")
	csrf := csrfToken(t, body)
	if status, _ := e.post(alice, "/settings/2fa/totp", url.Values{"csrf": {csrf}, "current": {"wrong-password-x"}}, ""); status != http.StatusUnauthorized {
		t.Fatalf("setup with a wrong password: %d", status)
	}
	status, body := e.post(alice, "/settings/2fa/totp", url.Values{"csrf": {csrf}, "current": {"alice-password-123"}}, "")
	m := regexp.MustCompile(`<pre class="secret">([A-Z2-7]+)</pre>`).FindStringSubmatch(body)
	if status != 200 || m == nil || !strings.Contains(body, "<svg") {
		t.Fatalf("no setup step (%d):\n%s", status, body)
	}
	if status, _ := e.post(alice, "/settings/2fa/totp/confirm", url.Values{"csrf": {csrf}, "totp": {"000000"}}, ""); status != http.StatusBadRequest {
		t.Fatalf("wrong confirmation code accepted: %d", status)
	}
	oldSecret := e.secrets["alice"]
	e.secrets["alice"] = m[1]
	status, body = e.post(alice, "/settings/2fa/totp/confirm", url.Values{"csrf": {csrf}, "totp": {e.code("alice")}}, "")
	if status != 200 || !strings.Contains(body, "New authenticator set up") {
		t.Fatalf("confirm (%d):\n%s", status, body)
	}
	if _, body := e.get(other, "/settings/keys"); strings.Contains(body, "Log out") {
		t.Fatal("other session survived the new authenticator")
	}
	// The old app's codes no longer work; the new app's do.
	key, _ := account.Base32.DecodeString(oldSecret)
	oldCode := account.HOTP(key, uint64(time.Now().Unix()/account.TOTPPeriod))
	jar, _ := cookiejar.New(nil)
	if status, _ := e.post(&http.Client{Jar: jar}, "/login", url.Values{"username": {"alice"}, "password": {"alice-password-123"}, "code": {oldCode}}, ""); status != http.StatusUnauthorized {
		t.Fatalf("old authenticator still works: %d", status)
	}
	e.login("alice")
}

// A push that removes commits (force push, deleted branch) deletes them from
// the server's disk; ordinary pushes leave the repository alone.
func TestForcePushCleanup(t *testing.T) {
	e := newTestEnv(t)
	e.pushInitial("alice", "pub")
	dir := filepath.Join(e.work, "alice-pub-src")
	repoDir := gitrepo.Dir(filepath.Join(e.data, "repos"), "alice", "pub")
	run := func(args ...string) string {
		out, err := e.git("alice", dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	onDisk := func(obj string) bool {
		return exec.Command("git", "--git-dir="+repoDir, "cat-file", "-e", obj).Run() == nil
	}
	// An unreferenced object, to see whether a push runs the cleanup.
	out, err := exec.Command("sh", "-c", "echo dangling | git --git-dir="+repoDir+" hash-object -w --stdin").Output()
	if err != nil {
		t.Fatal(err)
	}
	dangling := strings.TrimSpace(string(out))

	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("AWS_SECRET=hunter2\n"), 0o644)
	run("add", "secret.txt")
	run("commit", "-q", "-m", "oops")
	if out := run("push", "git@git.test:~alice/pub", "main"); strings.Contains(out, "deleting") {
		t.Fatalf("a fast-forward push ran the cleanup:\n%s", out)
	}
	leak := run("rev-parse", "HEAD")
	blob := run("rev-parse", "HEAD:secret.txt")
	if !onDisk(dangling) || !onDisk(leak) {
		t.Fatal("a fast-forward push deleted objects")
	}

	run("reset", "-q", "--hard", "HEAD~1")
	out2 := run("push", "-f", "git@git.test:~alice/pub", "main")
	if !strings.Contains(out2, "deleting them from the server's disk") || !strings.Contains(out2, "gone from the server") {
		t.Fatalf("no cleanup message:\n%s", out2)
	}
	for name, obj := range map[string]string{"commit": leak, "blob": blob, "dangling object": dangling} {
		if onDisk(obj) {
			t.Errorf("force-pushed %s still on disk", name)
		}
	}
	if b, _ := exec.Command("grep", "-r", "hunter2", repoDir).Output(); len(b) > 0 {
		t.Errorf("secret still found on disk:\n%s", b)
	}
	// The repository is intact.
	if out, err := exec.Command("git", "--git-dir="+repoDir, "fsck", "--no-dangling").CombinedOutput(); err != nil {
		t.Fatalf("fsck after cleanup: %v\n%s", err, out)
	}
	if _, err := e.git("bob", e.work, "clone", "-q", "git@git.test:~alice/pub", "after-cleanup"); err != nil {
		t.Fatalf("clone after cleanup: %v", err)
	}

	// Deleting a branch removes its commits too.
	run("checkout", "-q", "-b", "wip")
	os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("draft\n"), 0o644)
	run("add", "wip.txt")
	run("commit", "-q", "-m", "wip")
	run("push", "-q", "git@git.test:~alice/pub", "wip")
	wip := run("rev-parse", "HEAD")
	run("push", "-q", "git@git.test:~alice/pub", ":wip")
	if onDisk(wip) {
		t.Error("commit of a deleted branch still on disk")
	}
}

// Users see where they are logged in, can end those sessions, and see
// their recent logins, including the previous one right after logging in.
func TestSecurityPage(t *testing.T) {
	e := newTestEnv(t)
	login := func(user, ip, agent, code string) (*http.Client, string) {
		e.s.store.Update(user, func(u *store.User) error { u.TOTPLast = 0; return nil })
		if code == "" {
			code = e.code(user)
		}
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		req, _ := http.NewRequest("POST", e.srv.URL+"/login", strings.NewReader(url.Values{"username": {user},
			"password": {user + "-password-123"}, "code": {code}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Real-IP", ip)
		req.Header.Set("User-Agent", agent)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), "Log out") {
			t.Fatalf("login %s failed (%d)", user, resp.StatusCode)
		}
		return c, string(b)
	}
	const firefox = "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0"
	const chrome = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"

	a, body := login("alice", "198.51.100.1", firefox, "")
	if strings.Contains(body, "Last login") {
		t.Fatal("previous login shown on the very first login")
	}
	b, body := login("alice", "203.0.113.7", chrome, "")
	if !strings.Contains(body, "Last login:") || !strings.Contains(body, "198.51.100.1") || !strings.Contains(body, "Firefox on Linux") {
		t.Fatalf("previous login not shown:\n%s", body)
	}
	if _, body := e.get(b, "/"); strings.Contains(body, "Last login") {
		t.Fatal("previous login shown twice")
	}
	bob, _ := login("bob", "192.0.2.9", chrome, "")

	_, body = e.get(b, "/settings/security")
	if strings.Count(body, "this browser") != 1 || !strings.Contains(body, "Firefox on Linux") || !strings.Contains(body, "Chrome on Windows") ||
		!strings.Contains(body, "203.0.113.7") || strings.Count(body, "password and authenticator code") != 2 {
		t.Fatalf("security page wrong:\n%s", body)
	}
	if strings.Contains(body, "192.0.2.9") {
		t.Fatal("bob's session listed for alice")
	}
	// End the Firefox session from the Chrome one.
	row := regexp.MustCompile(`(?s)<tr><td>Firefox on Linux<br>.*?name="ref" value="([^"]+)"`).FindStringSubmatch(body)
	if row == nil {
		t.Fatalf("no Firefox row:\n%s", body)
	}
	csrf := csrfToken(t, body)
	if status, body := e.post(b, "/settings/security/logout", url.Values{"csrf": {csrf}, "ref": {row[1]}}, ""); status != 200 || !strings.Contains(body, "That session was logged out") {
		t.Fatalf("logout of one session: %d", status)
	}
	if _, body := e.get(a, "/settings/keys"); strings.Contains(body, "Log out") {
		t.Fatal("the Firefox session is still logged in")
	}
	// Another user's session can't be ended, even with its reference.
	_, bobBody := e.get(bob, "/settings/security")
	bobRef := regexp.MustCompile(`name="ref" value="([^"]+)"`).FindStringSubmatch(bobBody)
	if status, _ := e.post(b, "/settings/security/logout", url.Values{"csrf": {csrf}, "ref": {bobRef[1]}}, ""); status != http.StatusBadRequest {
		t.Fatalf("alice ended bob's session: %d", status)
	}
	if _, body := e.get(bob, "/settings/keys"); !strings.Contains(body, "Log out") {
		t.Fatal("bob was logged out")
	}
	// Log out all others.
	c, _ := login("alice", "192.0.2.50", firefox, "")
	if status, body := e.post(b, "/settings/security/logout-others", url.Values{"csrf": {csrf}}, ""); status != 200 || !strings.Contains(body, "All other sessions") {
		t.Fatalf("logout others: %d", status)
	}
	if _, body := e.get(c, "/settings/keys"); strings.Contains(body, "Log out") {
		t.Fatal("other session survived")
	}
	if _, body := e.get(b, "/settings/keys"); !strings.Contains(body, "Log out") {
		t.Fatal("this session was logged out too")
	}

	// A recovery-code login is recorded as such.
	codes := account.NewRecoveryCodes()
	e.s.store.SetRecoveryCodes("alice", []string{account.HashRecoveryCode("alice", codes[0])})
	login("alice", "192.0.2.60", firefox, codes[0])
	if l, _ := e.s.store.RecentLogins("alice", 1); len(l) != 1 || l[0].Method != "password and recovery code" || l[0].IP != "192.0.2.60" {
		t.Fatalf("recovery login recorded as %+v", l)
	}
}

func TestAuditLogPage(t *testing.T) {
	e := newTestEnv(t)
	bob := e.login("bob")
	_, body := e.get(bob, "/settings/keys")
	e.post(bob, "/settings/keys", url.Values{"csrf": {csrfToken(t, body)}, "key": {newTestKey(t) + " second"}}, "198.51.100.7")
	if status, _ := e.get(bob, "/settings/audit"); status != http.StatusNotFound {
		t.Fatalf("non-admin reached the audit log: %d", status)
	}

	alice := e.login("alice")
	_, body = e.get(alice, "/settings/invites")
	csrf := csrfToken(t, body)
	e.post(alice, "/settings/invites", url.Values{"csrf": {csrf}, "expires": {"1d"}, "admin": {"on"}}, "")
	invites, _ := e.s.store.Invites()
	if len(invites) != 1 {
		t.Fatalf("%d invites", len(invites))
	}
	e.post(alice, "/settings/invites/revoke", url.Values{"csrf": {csrf}, "id": {invites[0].ID}}, "")

	// The command line writes to the same log.
	cmd := exec.Command(testBinary, "user", "admin", "-data", e.data, "bob", "true")
	cmd.Env = append(os.Environ(), "SUDO_USER=carol")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	status, body := e.get(alice, "/settings/audit")
	if status != 200 {
		t.Fatalf("audit log: %d\n%s", status, body)
	}
	for _, want := range []string{
		"SSH key added <b>bob</b>", "198.51.100.7",
		"invite created", "id " + invites[0].ID, "makes an admin", "invite revoked",
		"command line (sudo carol)", "admin rights given <b>bob</b>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("audit log lacks %q", want)
		}
	}
	if strings.Contains(body, "secret") {
		t.Error("audit log mentions a private repository")
	}
	list, _ := e.s.store.AuditLog(10)
	if len(list) != 4 || list[0].Action != "admin rights given" || list[0].IP != "" || list[3].Actor != "bob" {
		t.Fatalf("entries: %+v", list)
	}
}
