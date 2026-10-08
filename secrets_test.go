package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testBox(t *testing.T) *secretBox {
	key, _ := parseKey(newKeyHex())
	b, err := newSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSecretBox(t *testing.T) {
	b := testBox(t)
	secret := newTOTPSecret()
	sealed := b.sealTOTP("alice", secret)
	if !strings.HasPrefix(sealed, totpPrefix) || strings.Contains(sealed, secret) {
		t.Fatalf("not encrypted: %q", sealed)
	}
	if sealed == b.sealTOTP("alice", secret) {
		t.Fatal("same ciphertext twice (nonce reuse)")
	}
	if got, err := b.openTOTP("alice", sealed); err != nil || got != secret {
		t.Fatalf("round trip: %q %v", got, err)
	}
	// Copying alice's encrypted secret into another account must not work.
	if _, err := b.openTOTP("mallory", sealed); err == nil {
		t.Fatal("secret decrypted for a different user")
	}
	raw := []byte(sealed)
	raw[len(raw)-3] ^= 1
	if _, err := b.openTOTP("alice", string(raw)); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := testBox(t).openTOTP("alice", sealed); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	if _, err := b.openTOTP("alice", secret); err == nil {
		t.Fatal("plaintext secret accepted")
	}
}

func TestLoadSecretBox(t *testing.T) {
	t.Setenv(keyEnv, "")
	t.Setenv(keyFileEnv, "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	dir := t.TempDir()
	s, _ := openStore(dir)
	defer s.Close()

	// Default location: created on first use, private, and reused.
	b1, err := loadSecretBox(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "secret.key")
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	b2, _ := loadSecretBox(dir, s)
	sealed := b1.sealTOTP("a", "SECRET")
	if got, err := b2.openTOTP("a", sealed); err != nil || got != "SECRET" {
		t.Fatal("key not reused")
	}

	// Never silently replace a lost key while encrypted secrets exist.
	s.Create(&User{Name: "a", TOTPSecret: sealed})
	os.Remove(keyPath)
	if _, err := loadSecretBox(dir, s); !errors.Is(err, errNoKey) {
		t.Fatalf("missing key with encrypted secrets: %v", err)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("a new key was created")
	}
	s.Delete("a") // the checks below use other keys

	// Key files readable by other users are refused.
	other := filepath.Join(t.TempDir(), "k")
	os.WriteFile(other, []byte(newKeyHex()), 0o644)
	t.Setenv(keyFileEnv, other)
	if _, err := loadSecretBox(dir, s); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("world-readable key: %v", err)
	}
	os.Chmod(other, 0o600)
	if _, err := loadSecretBox(dir, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv(keyFileEnv, filepath.Join(t.TempDir(), "missing"))
	if _, err := loadSecretBox(dir, s); err == nil {
		t.Fatal("missing explicit key file accepted")
	}

	// systemd credential and environment variable.
	creds := t.TempDir()
	keyHex := newKeyHex()
	os.WriteFile(filepath.Join(creds, keyCred), []byte(keyHex+"\n"), 0o400)
	t.Setenv("CREDENTIALS_DIRECTORY", creds)
	bc, err := loadSecretBox(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(keyEnv, keyHex)
	be, err := loadSecretBox(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := be.openTOTP("x", bc.sealTOTP("x", "S")); err != nil || got != "S" {
		t.Fatal("credential and env key differ")
	}
	// A different key than the one the database was encrypted with is refused.
	s.Create(&User{Name: "a", TOTPSecret: be.sealTOTP("a", "S")})
	t.Setenv(keyEnv, newKeyHex())
	if _, err := loadSecretBox(dir, s); !errors.Is(err, errNoKey) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong key accepted: %v", err)
	}
	t.Setenv(keyEnv, "not hex")
	if _, err := loadSecretBox(dir, s); err == nil {
		t.Fatal("bad env key accepted")
	}
}

// Secrets stored before encryption existed are encrypted on server start,
// login keeps working, and no plaintext copy stays in the database files.
func TestTOTPMigration(t *testing.T) {
	t.Setenv(keyEnv, "")
	t.Setenv(keyFileEnv, "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	dir := t.TempDir()
	secret := newTOTPSecret()
	hash, _ := hashPassword("alice-password-123")
	s, _ := openStore(dir)
	key, _ := parseSSHKey(newTestKey(t))
	s.Create(&User{Name: "alice", PasswordHash: hash, TOTPSecret: secret, SSHKeys: []SSHKey{key}})
	s.Close()

	srv, err := NewServer(Config{DataDir: dir, SiteName: "t", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := srv.store.Get("alice")
	if !strings.HasPrefix(u.TOTPSecret, totpPrefix) {
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
	k, _ := b32.DecodeString(secret)
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).PostForm(ts.URL+"/login", url.Values{"username": {"alice"},
		"password": {"alice-password-123"}, "code": {hotp(k, uint64(time.Now().Unix()/totpPeriod))}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login after migration: %d", resp.StatusCode)
	}

	// A secret moved into another account's row is useless there.
	s2 := srv.store
	s2.Update("alice", func(u *User) error { return nil })
	hash2, _ := hashPassword("mallory-password-1")
	key2, _ := parseSSHKey(newTestKey(t))
	s2.Create(&User{Name: "mallory", PasswordHash: hash2, TOTPSecret: u.TOTPSecret, SSHKeys: []SSHKey{key2}})
	jar2, _ := cookiejar.New(nil)
	resp, _ = (&http.Client{Jar: jar2}).PostForm(ts.URL+"/login", url.Values{"username": {"mallory"},
		"password": {"mallory-password-1"}, "code": {hotp(k, uint64(time.Now().Unix()/totpPeriod))}})
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("login with another account's 2FA secret")
	}
}
