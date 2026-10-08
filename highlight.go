package main

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Syntax highlighting is done server-side with chroma. Tokens get CSS
// classes (hl-*) rather than inline styles, which the CSP forbids; the
// stylesheet is generated from chroma's github / github-dark styles.

const (
	maxHighlightBytes = 512 << 10
	highlightBudget   = 2 * time.Second
)

// lexerFor picks a lexer by file name. For files without an extension it
// falls back to content analysis (shebangs, modelines). nil means plain text.
func lexerFor(name string, content string) chroma.Lexer {
	name = path.Base(name)
	l := lexers.Match(name)
	if l == nil && content != "" && path.Ext(name) == "" {
		l = lexers.Analyse(content)
	}
	if l == nil || l.Config().Name == "plaintext" {
		return nil
	}
	return chroma.Coalesce(l)
}

// highlightLines returns one HTML fragment per line of text, or nil if the
// text cannot be highlighted in time. The fragments contain no newlines.
func highlightLines(l chroma.Lexer, text string, deadline time.Time) []template.HTML {
	if l == nil || len(text) > maxHighlightBytes {
		return nil
	}
	it, err := l.Tokenise(nil, text)
	if err != nil {
		return nil
	}
	var tokens []chroma.Token
	for t := it(); t != chroma.EOF; t = it() {
		tokens = append(tokens, t)
		if len(tokens)%512 == 0 && time.Now().After(deadline) {
			return nil
		}
	}
	want := strings.Count(text, "\n") + 1
	out := make([]template.HTML, 0, want)
	for _, line := range chroma.SplitTokensIntoLines(tokens) {
		out = append(out, renderTokens(line))
	}
	// Lexers may add or drop a trailing newline; match the source line count.
	for len(out) < want {
		out = append(out, "")
	}
	return out[:want]
}

func renderTokens(tokens []chroma.Token) template.HTML {
	var b strings.Builder
	for _, t := range tokens {
		v := strings.TrimSuffix(t.Value, "\n")
		if v == "" {
			continue
		}
		if cls := tokenClass(t.Type); cls != "" {
			b.WriteString(`<span class="`)
			b.WriteString(cls)
			b.WriteString(`">`)
			b.WriteString(html.EscapeString(v))
			b.WriteString(`</span>`)
		} else {
			b.WriteString(html.EscapeString(v))
		}
	}
	return template.HTML(b.String())
}

// tokenClass maps a token type to its CSS class, falling back to the
// token's sub-category and category like chroma's own HTML formatter.
func tokenClass(t chroma.TokenType) string {
	for _, tt := range []chroma.TokenType{t, t.SubCategory(), t.Category()} {
		if cls, ok := chroma.StandardTypes[tt]; ok && cls != "" && cls != "err" && cls != "w" {
			return "hl-" + cls
		}
	}
	return ""
}

func plainLines(text string) []template.HTML {
	lines := strings.Split(text, "\n")
	out := make([]template.HTML, len(lines))
	for i, l := range lines {
		out[i] = template.HTML(html.EscapeString(l))
	}
	return out
}

// renderDiff turns a unified diff into display lines. Code lines are
// highlighted per hunk, separately for the old and new side, so that
// multi-line constructs inside a hunk are coloured correctly.
func renderDiff(patch string, highlight bool) []diffLine {
	deadline := time.Now().Add(highlightBudget)
	lines := strings.Split(strings.TrimRight(patch, "\n"), "\n")
	var out []diffLine
	var lexer chroma.Lexer
	meta := func(class, text string) diffLine {
		return diffLine{Class: class, HTML: template.HTML(html.EscapeString(text))}
	}
	for i := 0; i < len(lines); {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "diff --git "):
			out = append(out, meta("f", line))
			lexer = nil
			if highlight {
				// "diff --git a/x b/x": the new name is after the last " b/".
				if j := strings.LastIndex(line, " b/"); j >= 0 {
					lexer = lexerFor(line[j+3:], "")
				}
			}
			i++
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			out = append(out, meta("m", line))
			i++
		case strings.HasPrefix(line, "@@"):
			out = append(out, meta("h", line))
			i++
			j := i
			for j < len(lines) && isHunkLine(lines[j]) {
				j++
			}
			out = append(out, renderHunk(lines[i:j], lexer, deadline)...)
			i = j
		default:
			out = append(out, meta("", line))
			i++
		}
	}
	return out
}

func isHunkLine(l string) bool {
	return l == "" || strings.ContainsRune(" +-\\", rune(l[0]))
}

func renderHunk(lines []string, lexer chroma.Lexer, deadline time.Time) []diffLine {
	var oldSrc, newSrc []string
	for _, l := range lines {
		if l == "" {
			l = " "
		}
		switch l[0] {
		case ' ':
			oldSrc = append(oldSrc, l[1:])
			newSrc = append(newSrc, l[1:])
		case '-':
			oldSrc = append(oldSrc, l[1:])
		case '+':
			newSrc = append(newSrc, l[1:])
		}
	}
	oldHL := highlightLines(lexer, strings.Join(oldSrc, "\n"), deadline)
	newHL := highlightLines(lexer, strings.Join(newSrc, "\n"), deadline)
	pick := func(hl []template.HTML, src []string, i int) template.HTML {
		if hl != nil && i < len(hl) {
			return hl[i]
		}
		return template.HTML(html.EscapeString(src[i]))
	}

	out := make([]diffLine, 0, len(lines))
	oi, ni := 0, 0
	for _, l := range lines {
		if l == "" {
			l = " "
		}
		switch l[0] {
		case ' ':
			out = append(out, diffLine{Class: "c", Prefix: " ", HTML: pick(newHL, newSrc, ni)})
			oi++
			ni++
		case '+':
			out = append(out, diffLine{Class: "i", Prefix: "+", HTML: pick(newHL, newSrc, ni)})
			ni++
		case '-':
			out = append(out, diffLine{Class: "d", Prefix: "-", HTML: pick(oldHL, oldSrc, oi)})
			oi++
		default: // "\ No newline at end of file"
			out = append(out, diffLine{Class: "m", HTML: template.HTML(html.EscapeString(l))})
		}
	}
	return out
}

// highlightCSS is served as /static/highlight.css.
var highlightCSS = sync.OnceValue(func() []byte {
	var b bytes.Buffer
	b.WriteString("/* Generated from chroma's github and github-dark styles. */\n")
	writeHighlightStyle(&b, styles.Get("github"), "")
	b.WriteString("@media (prefers-color-scheme: dark) {\n")
	writeHighlightStyle(&b, styles.Get("github-dark"), "\t")
	b.WriteString("}\n")
	return b.Bytes()
})

func writeHighlightStyle(b *bytes.Buffer, style *chroma.Style, indent string) {
	base := style.Get(chroma.Background)
	types := make([]chroma.TokenType, 0, len(chroma.StandardTypes))
	for tt := range chroma.StandardTypes {
		types = append(types, tt)
	}
	slices.Sort(types)
	for _, tt := range types {
		cls := chroma.StandardTypes[tt]
		if tt < 0 || cls == "" || cls == "err" || cls == "w" {
			continue
		}
		// Every class gets a full rule in both themes, so nothing from the
		// light theme leaks into dark mode.
		e := style.Get(tt)
		decl := []string{"color:inherit", "font-weight:inherit", "font-style:inherit"}
		if e.Colour.IsSet() && e.Colour != base.Colour {
			decl[0] = "color:" + e.Colour.String()
		}
		if e.Bold == chroma.Yes {
			decl[1] = "font-weight:600"
		}
		if e.Italic == chroma.Yes {
			decl[2] = "font-style:italic"
		}
		if e.Underline == chroma.Yes {
			decl = append(decl, "text-decoration:underline")
		}
		fmt.Fprintf(b, "%s.hl-%s{%s}\n", indent, cls, strings.Join(decl, ";"))
	}
}
