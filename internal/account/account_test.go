package account

import (
	"testing"
	"time"
)

func TestBlocklists(t *testing.T) {
	if n := len(blocklist()); n < 800 {
		t.Fatalf("blocklists not loaded: %d names", n)
	}
	for _, name := range []string{"admin", "python", "pop3", "webmail", "dashboard", "create"} {
		if ValidUserName(name) {
			t.Errorf("%q should be blocked", name)
		}
	}
	for _, name := range []string{"alice", "bob", "carol", "obk"} {
		if !ValidUserName(name) {
			t.Errorf("%q should be allowed", name)
		}
	}
}

func TestHOTPVectors(t *testing.T) {
	// RFC 4226 appendix D.
	key := []byte("12345678901234567890")
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for i, w := range want {
		if got := HOTP(key, uint64(i)); got != w {
			t.Errorf("hotp(%d) = %s, want %s", i, got, w)
		}
	}
}

func TestTOTPReplayAndSkew(t *testing.T) {
	secret := NewTOTPSecret()
	key, _ := Base32.DecodeString(secret)
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / TOTPPeriod
	code := HOTP(key, uint64(step))

	got, ok := CheckTOTP(secret, code, 0, now)
	if !ok || got != step {
		t.Fatalf("valid code rejected")
	}
	if _, ok := CheckTOTP(secret, code, got, now); ok {
		t.Fatal("replayed code accepted")
	}
	if _, ok := CheckTOTP(secret, HOTP(key, uint64(step-1)), 0, now); !ok {
		t.Fatal("code from previous step rejected")
	}
	if _, ok := CheckTOTP(secret, HOTP(key, uint64(step-2)), 0, now); ok {
		t.Fatal("code from two steps ago accepted")
	}
	if _, ok := CheckTOTP(secret, "", 0, now); ok {
		t.Fatal("empty code accepted")
	}
}

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := CheckPassword(h, "correct horse battery"); !ok || err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	ok1, _ := CheckPassword(h, "wrong")
	ok2, _ := CheckPassword("garbage", "x")
	if ok1 || ok2 {
		t.Fatal("wrong password accepted")
	}
}
