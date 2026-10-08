package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// Recovery codes let a user log in without their authenticator app (a lost
// phone), each one once. A code is 16 base32 characters (80 random bits),
// shown as four groups: "abcd-efgh-2345-mnop". Only a hash is stored.
const (
	RecoveryCodeCount = 10
	recoveryCodeLen   = 16
)

var recoveryAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewRecoveryCodes returns RecoveryCodeCount fresh codes, formatted for display.
func NewRecoveryCodes() []string {
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		b := make([]byte, recoveryCodeLen*5/8)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		c := strings.ToLower(recoveryAlphabet.EncodeToString(b))
		codes[i] = c[0:4] + "-" + c[4:8] + "-" + c[8:12] + "-" + c[12:16]
	}
	return codes
}

// NormalizeRecoveryCode returns code without spaces and dashes, in lower
// case, or "" if it can't be a recovery code. Users may type it either way.
func NormalizeRecoveryCode(code string) string {
	c := strings.ToLower(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(code))
	if len(c) != recoveryCodeLen {
		return ""
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7') {
			return ""
		}
	}
	return c
}

// HashRecoveryCode is what the database stores for a code. The user name is
// part of it, so a stored hash is worthless for any other account. 80 random
// bits make a plain SHA-256 safe against guessing from a stolen database.
func HashRecoveryCode(user, code string) string {
	sum := sha256.Sum256([]byte("gitserver recovery v1\x00" + user + "\x00" + NormalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}
