// Package gitserver only holds repository-wide tests; the code is in
// cmd/gitserver and internal/.
package gitserver

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestReadmeCodeLinks keeps WIKI.md's code links honest: every link of the
// form [`name`](file#L12) must point at a line that contains `name`. When code
// moves, this fails and says which link to update.
func TestReadmeCodeLinks(t *testing.T) {
	readme, err := os.ReadFile("WIKI.md")
	if err != nil {
		t.Fatal(err)
	}
	links := regexp.MustCompile("\\[`([^`]+)`\\]\\(([^)#\\s]+)#L([0-9]+)\\)").FindAllStringSubmatch(string(readme), -1)
	if len(links) < 50 {
		t.Fatalf("only %d code links found in WIKI.md", len(links))
	}
	files := map[string][]string{}
	for _, l := range links {
		name, file := l[1], l[2]
		n, _ := strconv.Atoi(l[3])
		lines, ok := files[file]
		if !ok {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Errorf("WIKI.md links to missing file %s", file)
				continue
			}
			lines = strings.Split(string(b), "\n")
			files[file] = lines
		}
		if n < 1 || n > len(lines) || !strings.Contains(lines[n-1], name) {
			want := -1
			for i, line := range lines {
				if strings.Contains(line, name) {
					want = i + 1
					break
				}
			}
			t.Errorf("WIKI.md: [`%s`](%s#L%d) is out of date (first match is now line %d)", name, file, n, want)
		}
	}
}
