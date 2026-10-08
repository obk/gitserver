package render

import (
	"strings"
	"testing"
	"time"
)

func TestHighlight(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	src := "package main\n\n/* multi\n   line */\nfunc f() string { return \"<b>\" }"
	lines := HighlightLines(LexerFor("x.go", src), src, deadline)
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5", len(lines))
	}
	if !strings.Contains(string(lines[0]), `<span class="hl-kn">package</span>`) {
		t.Errorf("keyword not highlighted: %s", lines[0])
	}
	if !strings.Contains(string(lines[3]), `class="hl-cm"`) {
		t.Errorf("second line of block comment not highlighted as comment: %s", lines[3])
	}
	if strings.Contains(string(lines[4]), "<b>") || !strings.Contains(string(lines[4]), "&lt;b&gt;") {
		t.Errorf("token not escaped: %s", lines[4])
	}
	if LexerFor("notes.txt", "hello") != nil || LexerFor("README", "just some words") != nil {
		t.Error("plain text got a lexer")
	}
	if HighlightLines(LexerFor("x.go", ""), strings.Repeat("x", maxHighlightBytes+1), deadline) != nil {
		t.Error("oversized input highlighted")
	}

	patch := "diff --git a/m.go b/m.go\n--- a/m.go\n+++ b/m.go\n@@ -1,2 +1,2 @@\n package main\n-var a = 1\n+var a = \"<x>\"\n\\ No newline at end of file\n"
	got := Diff(patch, true)
	classes := ""
	for _, l := range got {
		classes += l.Class + ","
	}
	if classes != "f,m,m,h,c,d,i,m," {
		t.Fatalf("diff line classes = %s", classes)
	}
	if !strings.Contains(string(got[5].HTML), `class="hl-kd">var`) || !strings.Contains(string(got[6].HTML), "&#34;&lt;x&gt;&#34;") && !strings.Contains(string(got[6].HTML), "&quot;&lt;x&gt;&quot;") {
		t.Errorf("diff lines not highlighted/escaped: %q / %q", got[5].HTML, got[6].HTML)
	}
}
