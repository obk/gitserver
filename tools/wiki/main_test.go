package main

import (
	"os"
	"strings"
	"testing"
)

const testURL = "https://github.com/o/r/blob/main/"

func pagesByFile(t *testing.T, src string) map[string]string {
	t.Helper()
	pages, err := split(src, testURL)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, p := range pages {
		m[p.file] = p.body
	}
	return m
}

func TestSplit(t *testing.T) {
	src := "# proj\n\nIntro, see [setup](#setup-and-use).\n\n---\n\n## Contents\n\n1. [old](#x)\n\n---\n\n" +
		"## Setup and use\n\nRun [`main.go`](cmd/main.go#L3), see [repos](#repositories-1) and [the faq](#faq).\n\n" +
		"### Repositories\n\nSee [`docs`](./docs/a.md) and [web](https://example.com).\n\n```sh\n## not a heading\nx](#nope)\n```\n\n---\n\n" +
		"## Admin `tools`\n\n### Repositories\n\nBack to [repos](#repositories).\n\n## FAQ\n\nNothing.\n"
	p := pagesByFile(t, src)
	for _, f := range []string{"Home", "Setup-and-use", "Admin-tools", "FAQ", "_Sidebar", "_Footer"} {
		if _, ok := p[f]; !ok {
			t.Fatalf("no page %s; got %v", f, p)
		}
	}
	if len(p) != 6 {
		t.Fatalf("%d pages", len(p))
	}
	for file, want := range map[string][]string{
		"Home": {"Intro, see [setup](Setup-and-use).", "## Contents\n\n1. [Setup and use](Setup-and-use)\n2. [Admin tools](Admin-tools)\n3. [FAQ](FAQ)"},
		"Setup-and-use": {
			"[`main.go`](" + testURL + "cmd/main.go#L3)",
			"[repos](Admin-tools#repositories)", // repositories-1 in the README
			"[the faq](FAQ)",
			"[`docs`](" + testURL + "docs/a.md)", "[web](https://example.com)",
			"## not a heading\nx](#nope)", // code is left alone
		},
		"Admin-tools": {"Back to [repos](Setup-and-use#repositories)."},
		"_Sidebar":    {"- [Admin tools](Admin-tools)"},
		"_Footer":     {testURL + "README.md"},
	} {
		for _, w := range want {
			if !strings.Contains(p[file], w) {
				t.Errorf("%s lacks %q:\n%s", file, w, p[file])
			}
		}
	}
	for file, body := range p {
		if strings.Contains(body, "[old]") || strings.Contains(body, "# proj") || strings.HasSuffix(strings.TrimSpace(body), "---") {
			t.Errorf("%s keeps README-only parts:\n%s", file, body)
		}
	}
}

func TestSplitErrors(t *testing.T) {
	for _, src := range []string{
		"# p\n\n## A\n\nSee [b](#b).\n",
		"# p\n\n## A\n\n```\ncode\n",
		"# p\n\nno sections\n",
	} {
		if _, err := split(src, testURL); err == nil {
			t.Errorf("no error for %q", src)
		}
	}
}

// The real README must convert: every anchor link must match a heading.
func TestReadme(t *testing.T) {
	src, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	pages, err := split(string(src), testURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) < 10 {
		t.Fatalf("only %d pages", len(pages))
	}
}
