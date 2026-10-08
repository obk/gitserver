package web

import "strings"

// describeAgent turns a User-Agent header into something a person
// recognizes, like "Firefox on Linux". Unknown ones are shown shortened.
// It is only a label for the sessions and logins lists; the header is
// whatever the client sent.
func describeAgent(ua string) string {
	if ua == "" {
		return "unknown browser"
	}
	browser := ""
	// Order matters: Edge and Opera also say "Chrome", Chrome says "Safari".
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"},
		{"Chrome/", "Chrome"}, {"Chromium/", "Chromium"}, {"Safari/", "Safari"},
		{"curl/", "curl"}, {"git/", "git"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, o := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iPhone"}, {"iPad", "iPad"},
		{"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			system = o.name
			break
		}
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return "a browser on " + system
	}
	if len(ua) > 60 {
		return ua[:60] + "…"
	}
	return ua
}
