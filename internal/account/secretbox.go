package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// TOTPPrefix marks an encrypted secret; base32 never contains ':'.
const TOTPPrefix = "v1:"

type SecretBox struct {
	aead cipher.AEAD
}

func NewSecretBox(key []byte) (*SecretBox, error) {
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
	return &SecretBox{aead: aead}, nil
}

func ParseSecretKey(text string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, errors.New("the TOTP key must be 64 hex characters")
	}
	return key, nil
}

func NewKeyHex() string {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return hex.EncodeToString(key)
}

// The user name is authenticated together with the secret, so an encrypted
// secret only decrypts for the account it was created for.
func totpAD(user string) []byte { return []byte("gitserver totp v1\x00" + user) }

func (b *SecretBox) SealTOTP(user, secret string) string {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(secret), totpAD(user))
	return TOTPPrefix + base64.RawStdEncoding.EncodeToString(sealed)
}

func (b *SecretBox) OpenTOTP(user, stored string) (string, error) {
	enc, ok := strings.CutPrefix(stored, TOTPPrefix)
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
