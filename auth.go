package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
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
var argonSem = make(chan struct{}, 4)

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	argonSem <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-argonSem
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func checkPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	if m == 0 || m > 1<<20 || t == 0 || t > 16 || p == 0 {
		return false
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	argonSem <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-argonSem
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked against when a username does not exist, so that
// response timing does not reveal which usernames are valid.
var dummyHash = sync.OnceValue(func() string {
	h, err := hashPassword(randomToken(16))
	if err != nil {
		panic(err)
	}
	return h
})

// TOTP (RFC 6238): HMAC-SHA1, 6 digits, 30 second steps, ±1 step of clock skew.
const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, code%1000000)
}

// checkTOTP verifies code and returns the time step it matched. The step
// must be newer than last, so a code can never be used twice.
func checkTOTP(secret, code string, last int64, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil || len(key) == 0 {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	matched := int64(-1)
	for i := -totpSkew; i <= totpSkew; i++ {
		step := cur + int64(i)
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(step))), []byte(code)) == 1 && step > last {
			matched = step
		}
	}
	return matched, matched >= 0
}

func totpURI(issuer, account, secret string) string {
	q := url.Values{
		"secret":    {secret},
		"issuer":    {issuer},
		"algorithm": {"SHA1"},
		"digits":    {fmt.Sprint(totpDigits)},
		"period":    {fmt.Sprint(totpPeriod)},
	}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Sessions live in memory; restarting the server logs everyone out.
const (
	sessionMaxAge = 12 * time.Hour
	sessionIdle   = 2 * time.Hour
)

type session struct {
	user    string
	credFP  string // credentialFingerprint at login
	csrf    string
	created time.Time
	seen    time.Time
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]*session // keyed by SHA-256 of the cookie value
}

func newSessionStore() *sessionStore {
	s := &sessionStore{m: make(map[string]*session)}
	go func() {
		for range time.Tick(10 * time.Minute) {
			s.gc()
		}
	}()
	return s
}

// credentialFingerprint changes whenever the password or TOTP secret
// changes, which ends all existing sessions of that user.
func credentialFingerprint(u *User) string {
	return hashToken(u.PasswordHash + "\x00" + u.TOTPSecret)
}

func (s *sessionStore) create(user, credFP string) string {
	id := randomToken(32)
	now := time.Now()
	s.mu.Lock()
	s.m[hashToken(id)] = &session{user: user, credFP: credFP, csrf: randomToken(32), created: now, seen: now}
	s.mu.Unlock()
	return id
}

func (s *sessionStore) get(id string) (session, bool) {
	key := hashToken(id)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[key]
	if !ok {
		return session{}, false
	}
	if now.Sub(sess.created) > sessionMaxAge || now.Sub(sess.seen) > sessionIdle {
		delete(s.m, key)
		return session{}, false
	}
	sess.seen = now
	return *sess, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.m, hashToken(id))
	s.mu.Unlock()
}

// deleteUserExcept ends all sessions of user other than the one with id keep.
func (s *sessionStore) deleteUserExcept(user, keep string) {
	keepKey := hashToken(keep)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sess := range s.m {
		if sess.user == user && k != keepKey {
			delete(s.m, k)
		}
	}
}

func (s *sessionStore) gc() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sess := range s.m {
		if now.Sub(sess.created) > sessionMaxAge || now.Sub(sess.seen) > sessionIdle {
			delete(s.m, k)
		}
	}
}

// limiter counts failed authentication attempts per key in a fixed window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*bucket
}

type bucket struct {
	n     int
	start time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	l := &limiter{max: max, window: window, m: make(map[string]*bucket)}
	go func() {
		for range time.Tick(window) {
			l.gc()
		}
	}()
	return l
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		return false
	}
	if time.Since(b.start) > l.window {
		delete(l.m, key)
		return false
	}
	return b.n >= l.max
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil || time.Since(b.start) > l.window {
		b = &bucket{start: time.Now()}
		l.m[key] = b
	}
	b.n++
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func (l *limiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.m {
		if time.Since(b.start) > l.window {
			delete(l.m, k)
		}
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
