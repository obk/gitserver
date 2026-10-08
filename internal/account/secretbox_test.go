package account

import (
	"strings"
	"testing"
)

func testBox(t *testing.T) *SecretBox {
	key, _ := ParseSecretKey(NewKeyHex())
	b, err := NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSecretBox(t *testing.T) {
	b := testBox(t)
	secret := NewTOTPSecret()
	sealed := b.SealTOTP("alice", secret)
	if !strings.HasPrefix(sealed, TOTPPrefix) || strings.Contains(sealed, secret) {
		t.Fatalf("not encrypted: %q", sealed)
	}
	if sealed == b.SealTOTP("alice", secret) {
		t.Fatal("same ciphertext twice (nonce reuse)")
	}
	if got, err := b.OpenTOTP("alice", sealed); err != nil || got != secret {
		t.Fatalf("round trip: %q %v", got, err)
	}
	// Copying alice's encrypted secret into another account must not work.
	if _, err := b.OpenTOTP("mallory", sealed); err == nil {
		t.Fatal("secret decrypted for a different user")
	}
	raw := []byte(sealed)
	raw[len(raw)-3] ^= 1
	if _, err := b.OpenTOTP("alice", string(raw)); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := testBox(t).OpenTOTP("alice", sealed); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	if _, err := b.OpenTOTP("alice", secret); err == nil {
		t.Fatal("plaintext secret accepted")
	}
}
