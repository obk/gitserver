package account

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238): HMAC-SHA1, 6 digits, 30 second steps, ±1 step of clock skew.
const (
	TOTPPeriod = 30
	totpDigits = 6
	totpSkew   = 1
)

var Base32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return Base32.EncodeToString(b)
}

func HOTP(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, code%1000000)
}

// CheckTOTP verifies code and returns the time step it matched. The step
// must be newer than last, so a code can never be used twice.
func CheckTOTP(secret, code string, last int64, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := Base32.DecodeString(strings.ToUpper(secret))
	if err != nil || len(key) == 0 {
		return 0, false
	}
	cur := now.Unix() / TOTPPeriod
	matched := int64(-1)
	for i := -totpSkew; i <= totpSkew; i++ {
		step := cur + int64(i)
		if subtle.ConstantTimeCompare([]byte(HOTP(key, uint64(step))), []byte(code)) == 1 && step > last {
			matched = step
		}
	}
	return matched, matched >= 0
}

func TOTPURI(issuer, account, secret string) string {
	q := url.Values{
		"secret":    {secret},
		"issuer":    {issuer},
		"algorithm": {"SHA1"},
		"digits":    {fmt.Sprint(totpDigits)},
		"period":    {fmt.Sprint(TOTPPeriod)},
	}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}
