package account

import "testing"

func TestCleanWebsite(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"  ":                             "",
		"example.com":                    "https://example.com",
		" https://example.com/blog ":     "https://example.com/blog",
		"http://example.com":             "http://example.com",
		"https://example.com/a?b=c#d":    "https://example.com/a?b=c#d",
		"https://xn--bcher-kva.example/": "https://xn--bcher-kva.example/",
		"https://bücher.example":         "https://b%C3%BCcher.example",
	} {
		got, err := CleanWebsite(in)
		if err != nil || got != want {
			t.Errorf("CleanWebsite(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"javascript:alert(1)", "javascript://example.com/%0aalert(1)", "data:text/html,x", "ftp://example.com",
		"https://user:pw@example.com", "https://exa mple.com", "https://example.com/\nx", "https://", "//",
		"https://example.com\\@evil.com", "mailto:a@example.com", "https://.example.com",
		"https://" + string(make([]byte, MaxWebsiteLen)),
	} {
		if got, err := CleanWebsite(in); err == nil {
			t.Errorf("CleanWebsite(%q) = %q, want an error", in, got)
		}
	}
	if got := WebsiteLabel("https://example.com/"); got != "example.com" {
		t.Errorf("WebsiteLabel = %q", got)
	}
}
