package account

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

// MaxWebsiteLen caps the website link on a profile.
const MaxWebsiteLen = 200

var errWebsite = errors.New("the website must be an http or https address, like https://example.com")

// CleanWebsite checks a website link for a profile and returns it in the
// form it is stored: "" (no website) or an absolute http(s) URL. A missing
// scheme means https, so "example.com" works. Anything else (javascript:,
// user:password@, spaces) is refused, not repaired.
func CleanWebsite(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > MaxWebsiteLen {
		return "", errors.New("the website address is too long")
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\\' }) >= 0 {
		return "", errWebsite
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return "", errWebsite
	}
	host := u.Hostname()
	if host == "" || strings.HasPrefix(host, ".") || strings.HasPrefix(host, "-") {
		return "", errWebsite
	}
	return u.String(), nil
}

// WebsiteLabel is how a website link is shown: without the scheme and a
// trailing slash, e.g. "example.com/blog".
func WebsiteLabel(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	return strings.TrimSuffix(s, "/")
}
