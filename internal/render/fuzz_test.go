package render

import (
	"net/url"
	"path"
	"strings"
	"testing"
)

// Fuzz tests for functions that check attacker-controlled input. Each one
// states what an accepted value must never contain. `go test` runs the seed
// corpus; `go test -fuzz FuzzName` searches for more (see WIKI.md).

// README links rewritten into the repository must stay inside it, also
// from a README in a subfolder.
func FuzzRewriteRelative(f *testing.F) {
	for _, s := range []string{"docs/a.md", "../x", "a/../../b", "https://evil.com", "//evil.com", "#top", "a.png?raw=1#x", "./a", "a%2F..%2F..%2Fb", "../../../x"} {
		f.Add(s, "")
		f.Add(s, "sub/dir")
	}
	const linkBase = "/~a/r/tree/"
	repoBase, _ := url.Parse("https://git.example.com/~a/r/")
	f.Fuzz(func(t *testing.T, dest, dir string) {
		out := string(rewriteRelative([]byte(dest), linkBase, dir, "h=v1"))
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

// The EXIF reader walks offsets taken from the uploaded file: it must never
// read outside it, and only ever report a valid orientation.
func FuzzJPEGOrientation(f *testing.F) {
	f.Add([]byte("\xff\xd8\xff\xe1\x00\x22Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte("\xff\xd8\xff\xe1\x00\x10Exif\x00\x00II\x2a\x00\xff\xff\xff\x7f"))
	f.Add([]byte("\xff\xd8\xff\xe1\xff\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if o := jpegOrientation(b); o < 1 || o > 8 {
			t.Fatalf("jpegOrientation = %d", o)
		}
	})
}
