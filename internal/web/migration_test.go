package web

import (
	"bytes"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-git-server/internal/account"
	"go-git-server/internal/sshgit"
	"go-git-server/internal/store"
)

// Secrets stored before encryption existed are encrypted on server start,
// login keeps working, and no plaintext copy stays in the database files.
func TestTOTPMigration(t *testing.T) {
	t.Setenv("GITSERVER_KEY", "")
	t.Setenv("GITSERVER_KEY_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	dir := t.TempDir()
	secret := account.NewTOTPSecret()
	hash, _ := account.HashPassword("alice-password-123")
	s, _ := store.Open(dir)
	key, _ := sshgit.ParseKey(newTestKey(t))
	s.Create(&store.User{Name: "alice", PasswordHash: hash, TOTPSecret: secret, SSHKeys: []store.SSHKey{key}})
	s.Close()

	srv, err := NewServer(Config{DataDir: dir, SiteName: "t", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := srv.store.Get("alice")
	if !strings.HasPrefix(u.TOTPSecret, account.TOTPPrefix) {
		t.Fatalf("secret not encrypted: %q", u.TOTPSecret)
	}
	for _, f := range []string{"gitserver.db", "gitserver.db-wal", "gitserver.db-shm"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("plaintext 2FA secret still on disk in %s", f)
		}
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	k, _ := account.Base32.DecodeString(secret)
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).PostForm(ts.URL+"/login", url.Values{"username": {"alice"},
		"password": {"alice-password-123"}, "code": {account.HOTP(k, uint64(time.Now().Unix()/account.TOTPPeriod))}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login after migration: %d", resp.StatusCode)
	}

	// A secret moved into another account's row is useless there.
	s2 := srv.store
	s2.Update("alice", func(u *store.User) error { return nil })
	hash2, _ := account.HashPassword("mallory-password-1")
	key2, _ := sshgit.ParseKey(newTestKey(t))
	s2.Create(&store.User{Name: "mallory", PasswordHash: hash2, TOTPSecret: u.TOTPSecret, SSHKeys: []store.SSHKey{key2}})
	jar2, _ := cookiejar.New(nil)
	resp, _ = (&http.Client{Jar: jar2}).PostForm(ts.URL+"/login", url.Values{"username": {"mallory"},
		"password": {"mallory-password-1"}, "code": {account.HOTP(k, uint64(time.Now().Unix()/account.TOTPPeriod))}})
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("login with another account's 2FA secret")
	}
}
