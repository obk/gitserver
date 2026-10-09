package store

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"go-git-server/internal/account"
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
	keyEnv     = "GITSERVER_KEY"
	keyFileEnv = "GITSERVER_KEY_FILE"
	keyCred    = "secret.key"
)

var errNoKey = errors.New("TOTP encryption key not available")

// LoadSecretBox finds the key (see above) and checks that it decrypts the
// secrets already in the database. It only creates a new key file at the
// default location, and never when encrypted secrets already exist.
func LoadSecretBox(dataDir string, store *Store) (*account.SecretBox, error) {
	box, err := findSecretBox(dataDir, store)
	if err != nil {
		return nil, err
	}
	name, sealed, err := store.anyEncryptedTOTP()
	if err != nil {
		return nil, err
	}
	if sealed != "" {
		if _, err := box.OpenTOTP(name, sealed); err != nil {
			return nil, fmt.Errorf("%w: this key does not match the database (it cannot decrypt the 2FA secret of %s); restore the original key (on a server: /etc/gitserver/secret.key)", errNoKey, name)
		}
	}
	return box, nil
}

func findSecretBox(dataDir string, store *Store) (*account.SecretBox, error) {
	if v := os.Getenv(keyEnv); v != "" {
		key, err := account.ParseSecretKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyEnv, err)
		}
		return account.NewSecretBox(key)
	}
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		// #nosec G703 -- the operator's key path (systemd credentials)
		if b, err := os.ReadFile(filepath.Join(dir, keyCred)); err == nil {
			key, err := account.ParseSecretKey(string(b))
			if err != nil {
				return nil, fmt.Errorf("systemd credential %s: %w", keyCred, err)
			}
			return account.NewSecretBox(key)
		}
	}
	path, explicit := os.Getenv(keyFileEnv), true
	if path == "" {
		path, explicit = filepath.Join(dataDir, "secret.key"), false
	}
	// #nosec G703 -- the operator's key path
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		if n, err := store.countEncryptedTOTP(); err != nil {
			return nil, err
		} else if n > 0 {
			return nil, fmt.Errorf("%w: %s is missing, but %d account(s) have encrypted 2FA secrets; restore the key from your backup (on a server: /etc/gitserver/secret.key)", errNoKey, path, n)
		}
		// #nosec G703 -- the operator's key path
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		keyHex := account.NewKeyHex()
		_, err = f.WriteString(keyHex + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		// #nosec G706 -- the operator's key path
		log.Printf("created TOTP encryption key %s; back it up, 2FA secrets cannot be decrypted without it", path)
		key, _ := account.ParseSecretKey(keyHex)
		return account.NewSecretBox(key)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoKey, err)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("TOTP key %s is readable by other users (mode %v); run: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	// #nosec G703 -- the operator's key path
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoKey, err)
	}
	key, err := account.ParseSecretKey(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return account.NewSecretBox(key)
}

// EncryptTOTPSecrets encrypts secrets still stored in plaintext (from before
// encryption existed) and wipes the old plaintext from the database file.
func EncryptTOTPSecrets(store *Store, box *account.SecretBox) error {
	users, err := store.List()
	if err != nil {
		return err
	}
	n := 0
	for _, u := range users {
		if u.TOTPSecret == "" || strings.HasPrefix(u.TOTPSecret, account.TOTPPrefix) {
			continue
		}
		err := store.Update(u.Name, func(u *User) error {
			if !strings.HasPrefix(u.TOTPSecret, account.TOTPPrefix) {
				u.TOTPSecret = box.SealTOTP(u.Name, u.TOTPSecret)
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
