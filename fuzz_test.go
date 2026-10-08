package main

import (
	"net/url"
	"path"
	"strings"
	"testing"
)

// Fuzz tests for the functions that check attacker-controlled input. Each
// one states what an accepted value must never contain. `go test` runs the
// seed corpus; `go test -fuzz FuzzName` searches for more (see README).

func FuzzParseSSHCommand(f *testing.F) {
	for _, s := range []string{"git-upload-pack '~obk/x.git'", "git-receive-pack '/~a/b/'", "git-upload-archive '~a/b'",
		"git-upload-pack '~a/../b'", "git-upload-pack '~a/b'; id", "sh -c id", "git-upload-pack '~A/b'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		service, owner, repo, err := parseSSHCommand(cmd)
		if err != nil {
			return
		}
		if service != "upload-pack" && service != "receive-pack" && service != "upload-archive" {
			t.Fatalf("unknown service %q from %q", service, cmd)
		}
		if !userNameRe.MatchString(owner) {
			t.Fatalf("bad owner %q from %q", owner, cmd)
		}
		if repo == "" || strings.ContainsAny(repo, "/\\'\x00 \n") || strings.HasPrefix(repo, "-") || strings.HasPrefix(repo, ".") {
			t.Fatalf("bad repo %q from %q", repo, cmd)
		}
	})
}

func FuzzValidRev(f *testing.F) {
	for _, s := range []string{"main", "refs/heads/main", "v1.0", "--output=x", "HEAD:secret", "a..b", "HEAD^{tree}", "x y", "abcd"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rev string) {
		if !validRev(rev) {
			return
		}
		// Not an option, not rev:path or a range, no revision operators, no whitespace.
		if strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, ":\\?*[~^ \t\n\x00") || strings.Contains(rev, "..") {
			t.Fatalf("validRev accepted %q", rev)
		}
	})
}

func FuzzCleanTreePath(f *testing.F) {
	for _, s := range []string{"", "a/b.go", "/a/", "../etc/passwd", "a/./b", "a//b", "a/..", "a\x00b", "..."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		clean, ok := cleanTreePath(p)
		if !ok {
			return
		}
		if clean == "" {
			return
		}
		if strings.HasPrefix(clean, "/") || strings.HasSuffix(clean, "/") || strings.ContainsRune(clean, 0) {
			t.Fatalf("cleanTreePath(%q) = %q", p, clean)
		}
		for _, seg := range strings.Split(clean, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("cleanTreePath(%q) = %q has segment %q", p, clean, seg)
			}
		}
		if path.Clean(clean) != clean {
			t.Fatalf("cleanTreePath(%q) = %q is not clean", p, clean)
		}
	})
}

// The login redirect must never leave the site, however a browser parses it.
func FuzzSafeNext(f *testing.F) {
	for _, s := range []string{"/", "/~a/b?h=main", "//evil.com", "/\\evil.com", "/\t/evil.com", "https://evil.com", "/.//evil.com",
		"/%2F%2Fevil.com", "javascript:alert(1)", "/a/../..//evil.com", "/@evil.com"} {
		f.Add(s)
	}
	base, _ := url.Parse("https://git.example.com/login")
	f.Fuzz(func(t *testing.T, next string) {
		got := safeNext(next)
		// Browsers ignore tabs and newlines and treat \ like /.
		browser := strings.NewReplacer("\t", "", "\n", "", "\r", "", "\\", "/").Replace(got)
		// http.Redirect cleans the path before sending it.
		p, q, found := strings.Cut(browser, "?")
		redirected := path.Clean(p)
		if strings.HasSuffix(p, "/") && redirected != "/" {
			redirected += "/"
		}
		if found {
			redirected += "?" + q
		}
		for _, loc := range []string{got, browser, redirected} {
			u, err := url.Parse(loc)
			if err != nil {
				continue
			}
			if r := base.ResolveReference(u); r.Host != base.Host || r.Scheme != base.Scheme {
				t.Fatalf("safeNext(%q) = %q leaves the site as %q", next, got, r)
			}
		}
	})
}

// README links rewritten into the repository must stay inside it.
func FuzzRewriteRelative(f *testing.F) {
	for _, s := range []string{"docs/a.md", "../x", "a/../../b", "https://evil.com", "//evil.com", "#top", "a.png?raw=1#x", "./a", "a%2F..%2F..%2Fb"} {
		f.Add(s)
	}
	const linkBase = "/~a/r/tree/"
	repoBase, _ := url.Parse("https://git.example.com/~a/r/")
	f.Fuzz(func(t *testing.T, dest string) {
		out := string(rewriteRelative([]byte(dest), linkBase))
		if out == dest {
			return // left alone: absolute, fragment or outside the repository
		}
		if !strings.HasPrefix(out, linkBase) {
			t.Fatalf("rewriteRelative(%q) = %q", dest, out)
		}
		u, err := url.Parse(out)
		if err != nil {
			t.Fatalf("rewriteRelative(%q) = %q does not parse: %v", dest, out, err)
		}
		r := repoBase.ResolveReference(u)
		if r.Host != repoBase.Host || !strings.HasPrefix(path.Clean(r.Path)+"/", linkBase) {
			t.Fatalf("rewriteRelative(%q) = %q resolves to %q", dest, out, r)
		}
	})
}
