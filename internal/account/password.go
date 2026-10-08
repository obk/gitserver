// Package account holds the account rules that need no storage: user
// names, password hashing (argon2id), TOTP codes, random tokens and the
// encryption of TOTP secrets.
package account

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Password hashing: argon2id with RFC 9106 "second recommended" parameters.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
)

var b64 = base64.RawStdEncoding

// argonSem bounds concurrent hashes so login floods can't exhaust memory.
// A request waits at most argonWait for a slot; under a flood the rest are
// turned away (ErrHashBusy) instead of queueing without limit.
var argonSem = make(chan struct{}, 4)

const argonWait = 5 * time.Second

var ErrHashBusy = errors.New("the server is busy; try again in a moment")

func acquireArgon() bool {
	select {
	case argonSem <- struct{}{}:
		return true
	case <-time.After(argonWait):
		return false
	}
}

func HashPassword(pw string) (string, error) {
	if !acquireArgon() {
		return "", ErrHashBusy
	}
	defer func() { <-argonSem }()
	return deriveHash(pw)
}

// deriveHash hashes pw without taking a slot in argonSem.
func deriveHash(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// CheckPassword reports whether pw matches encoded. The error is
// ErrHashBusy if no hashing slot became free in time.
func CheckPassword(encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, nil
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, nil
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, nil
	}
	if m == 0 || m > 1<<20 || t == 0 || t > 16 || p == 0 {
		return false, nil
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, nil
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, nil
	}
	if !acquireArgon() {
		return false, ErrHashBusy
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-argonSem
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyHash is checked against when a username does not exist, so that
// response timing does not reveal which usernames are valid.
var DummyHash = sync.OnceValue(func() string {
	h, err := deriveHash(RandomToken(16))
	if err != nil {
		panic(err)
	}
	return h
})
