package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// TOTP secrets cannot be hashed (the server needs them to compute codes), so
// they are encrypted with AES-256-GCM under a key that is never stored in
// the database. A stolen gitserver.db or backup only holds ciphertext.
//
// The key is found, in order:
//
//  1. GITSERVER_KEY: 64 hex characters (gitserverctl passes it this way)
//  2. $CREDENTIALS_DIRECTORY/secret.key: systemd LoadCredential (the service)
//  3. GITSERVER_KEY_FILE: path to a key file
//  4. <data>/secret.key: created on first use (local development)
//
// In production the key lives in /etc/gitserver/secret.key (root, 0600).
// Only the web server and the user/totp CLI commands need it; SSH never does.

const (
	totpPrefix = "v1:" // marks an encrypted secret; base32 never contains ':'
	keyEnv     = "GITSERVER_KEY"
	keyFileEnv = "GITSERVER_KEY_FILE"
	keyCred    = "secret.key"
)

var errNoKey = errors.New("TOTP encryption key not available")

type secretBox struct {
	aead cipher.AEAD
}

func newSecretBox(key []byte) (*secretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("the TOTP key must be 32 bytes (64 hex characters), got %d bytes", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead: aead}, nil
}

func parseKey(text string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, errors.New("the TOTP key must be 64 hex characters")
	}
	return key, nil
}

func newKeyHex() string {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return hex.EncodeToString(key)
}

// loadSecretBox finds the key (see above) and checks that it decrypts the
// secrets already in the database. It only creates a new key file at the
// default location, and never when encrypted secrets already exist.
func loadSecretBox(dataDir string, store *Store) (*secretBox, error) {
	box, err := findSecretBox(dataDir, store)
	if err != nil {
		return nil, err
	}
	name, sealed, err := store.anyEncryptedTOTP()
	if err != nil {
		return nil, err
	}
	if sealed != "" {
		if _, err := box.openTOTP(name, sealed); err != nil {
			return nil, fmt.Errorf("%w: this key does not match the database (it cannot decrypt the 2FA secret of %s); restore the original key (on a server: /etc/gitserver/secret.key)", errNoKey, name)
		}
	}
	return box, nil
}

func findSecretBox(dataDir string, store *Store) (*secretBox, error) {
	if v := os.Getenv(keyEnv); v != "" {
		key, err := parseKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyEnv, err)
		}
		return newSecretBox(key)
	}
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		if b, err := os.ReadFile(filepath.Join(dir, keyCred)); err == nil {
			key, err := parseKey(string(b))
			if err != nil {
				return nil, fmt.Errorf("systemd credential %s: %w", keyCred, err)
			}
			return newSecretBox(key)
		}
	}
	path, explicit := os.Getenv(keyFileEnv), true
	if path == "" {
		path, explicit = filepath.Join(dataDir, "secret.key"), false
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		if n, err := store.countEncryptedTOTP(); err != nil {
			return nil, err
		} else if n > 0 {
			return nil, fmt.Errorf("%w: %s is missing, but %d account(s) have encrypted 2FA secrets; restore the key from your backup (on a server: /etc/gitserver/secret.key)", errNoKey, path, n)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		keyHex := newKeyHex()
		_, err = f.WriteString(keyHex + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		log.Printf("created TOTP encryption key %s; back it up, 2FA secrets cannot be decrypted without it", path)
		key, _ := parseKey(keyHex)
		return newSecretBox(key)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoKey, err)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("TOTP key %s is readable by other users (mode %v); run: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoKey, err)
	}
	key, err := parseKey(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return newSecretBox(key)
}

// The user name is authenticated together with the secret, so an encrypted
// secret only decrypts for the account it was created for.
func totpAD(user string) []byte { return []byte("gitserver totp v1\x00" + user) }

func (b *secretBox) sealTOTP(user, secret string) string {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(secret), totpAD(user))
	return totpPrefix + base64.RawStdEncoding.EncodeToString(sealed)
}

func (b *secretBox) openTOTP(user, stored string) (string, error) {
	enc, ok := strings.CutPrefix(stored, totpPrefix)
	if !ok {
		return "", errors.New("2FA secret is not encrypted")
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil || len(raw) < b.aead.NonceSize()+b.aead.Overhead() {
		return "", errors.New("2FA secret is corrupt")
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], totpAD(user))
	if err != nil {
		return "", errors.New("2FA secret cannot be decrypted (wrong key, or it belongs to another account)")
	}
	return string(plain), nil
}

// encryptTOTPSecrets encrypts secrets still stored in plaintext (from before
// encryption existed) and wipes the old plaintext from the database file.
func encryptTOTPSecrets(store *Store, box *secretBox) error {
	users, err := store.List()
	if err != nil {
		return err
	}
	n := 0
	for _, u := range users {
		if u.TOTPSecret == "" || strings.HasPrefix(u.TOTPSecret, totpPrefix) {
			continue
		}
		err := store.Update(u.Name, func(u *User) error {
			if !strings.HasPrefix(u.TOTPSecret, totpPrefix) {
				u.TOTPSecret = box.sealTOTP(u.Name, u.TOTPSecret)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("encrypting 2FA secret of %s: %w", u.Name, err)
		}
		n++
	}
	if n > 0 {
		log.Printf("encrypted the 2FA secrets of %d account(s)", n)
		return store.purgeFreedPages()
	}
	return nil
}
