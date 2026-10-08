package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-git-server/internal/account"
)

func TestLoadSecretBox(t *testing.T) {
	t.Setenv(keyEnv, "")
	t.Setenv(keyFileEnv, "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	dir := t.TempDir()
	s, _ := Open(dir)
	defer s.Close()

	// Default location: created on first use, private, and reused.
	b1, err := LoadSecretBox(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "secret.key")
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	b2, _ := LoadSecretBox(dir, s)
	sealed := b1.SealTOTP("a", "SECRET")
	if got, err := b2.OpenTOTP("a", sealed); err != nil || got != "SECRET" {
		t.Fatal("key not reused")
	}

	// Never silently replace a lost key while encrypted secrets exist.
	s.Create(&User{Name: "a", TOTPSecret: sealed})
	os.Remove(keyPath)
	if _, err := LoadSecretBox(dir, s); !errors.Is(err, errNoKey) {
		t.Fatalf("missing key with encrypted secrets: %v", err)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("a new key was created")
	}
	s.Delete("a") // the checks below use other keys

	// Key files readable by other users are refused.
	other := filepath.Join(t.TempDir(), "k")
	os.WriteFile(other, []byte(account.NewKeyHex()), 0o644)
	t.Setenv(keyFileEnv, other)
	if _, err := LoadSecretBox(dir, s); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("world-readable key: %v", err)
	}
	os.Chmod(other, 0o600)
	if _, err := LoadSecretBox(dir, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv(keyFileEnv, filepath.Join(t.TempDir(), "missing"))
	if _, err := LoadSecretBox(dir, s); err == nil {
		t.Fatal("missing explicit key file accepted")
	}

	// systemd credential and environment variable.
	creds := t.TempDir()
	keyHex := account.NewKeyHex()
	os.WriteFile(filepath.Join(creds, keyCred), []byte(keyHex+"\n"), 0o400)
	t.Setenv("CREDENTIALS_DIRECTORY", creds)
	bc, err := LoadSecretBox(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(keyEnv, keyHex)
	be, err := LoadSecretBox(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := be.OpenTOTP("x", bc.SealTOTP("x", "S")); err != nil || got != "S" {
		t.Fatal("credential and env key differ")
	}
	// A different key than the one the database was encrypted with is refused.
	if err := s.Create(&User{Name: "b", TOTPSecret: be.SealTOTP("b", "S")}); err != nil { // "a" can't be reused
		t.Fatal(err)
	}
	t.Setenv(keyEnv, account.NewKeyHex())
	if _, err := LoadSecretBox(dir, s); !errors.Is(err, errNoKey) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong key accepted: %v", err)
	}
	t.Setenv(keyEnv, "not hex")
	if _, err := LoadSecretBox(dir, s); err == nil {
		t.Fatal("bad env key accepted")
	}
}
