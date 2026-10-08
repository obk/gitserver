// Package render turns repository content into safe HTML: Markdown
// (READMEs) and syntax-highlighted code and diffs.
package render

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Markdown renders untrusted Markdown: no raw HTML, and goldmark drops
// javascript: and similar link destinations (WithUnsafe is never set).
//
// For a README, linkBase and imageBase make relative links work like on
// GitHub: [x](auth.go#L48) points into the file view of the repository
// (".../tree/auth.go#L48", which has line anchors) and ![](img.png) to the
// raw file. With empty bases, links are left alone.
func Markdown(src []byte, linkBase, imageBase string) (template.HTML, error) {
	opts := []parser.Option{parser.WithAutoHeadingID()}
	if linkBase != "" {
		opts = append(opts, parser.WithASTTransformers(util.Prioritized(relativeLinks{linkBase, imageBase}, 100)))
	}
	md := goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithParserOptions(opts...))
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

type relativeLinks struct{ linkBase, imageBase string }

func (t relativeLinks) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch n := n.(type) {
			case *ast.Link:
				n.Destination = rewriteRelative(n.Destination, t.linkBase)
			case *ast.Image:
				n.Destination = rewriteRelative(n.Destination, t.imageBase)
			}
		}
		return ast.WalkContinue, nil
	})
}

// rewriteRelative prefixes a repository-relative destination with base.
// Absolute URLs, absolute paths, fragments and paths leaving the
// repository are returned unchanged.
func rewriteRelative(dest []byte, base string) []byte {
	d := string(dest)
	if d == "" || strings.HasPrefix(d, "#") || strings.HasPrefix(d, "/") {
		return dest
	}
	u, err := url.Parse(d)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return dest
	}
	p := path.Clean(u.Path)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return dest
	}
	out := base + EscapePath(p)
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return []byte(out)
}

// RawContentType lets images referenced from READMEs display. Everything
// else is served as plain text. Raw files always get "CSP: sandbox", so
// even an SVG cannot run scripts on this origin.
func RawContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	}
	return "text/plain; charset=utf-8"
}

// EscapePath escapes each segment of a slash-separated path for use in URLs.
func EscapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}
