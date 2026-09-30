package spec

import (
	"bytes"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// RenderHTML renders a section body for the viewer. Raw HTML is never passed
// through (no html.WithUnsafe), links and images with a javascript:, vbscript:,
// file: or data: destination lose it (images keep data:image/png, gif, jpeg
// and webp), and Mermaid fences become
// <pre class="mermaid-src" data-mermaid="1"><code>…</code></pre> for the
// viewer to pick up; nothing is executed here. GFM tables, strikethrough and
// task lists are enabled.
func RenderHTML(body string) (string, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
		goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(sanitizer{}, 1000))),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(fenceRenderer{}, 100))),
	)
	var buf bytes.Buffer
	if err := md.Convert([]byte(body), &buf); err != nil {
		return "", errs.New("cannot render markdown: %v", err)
	}
	out := strings.ReplaceAll(buf.String(), "<!-- raw HTML omitted -->\n", "")
	return strings.ReplaceAll(out, "<!-- raw HTML omitted -->", ""), nil
}

// unsafeURL reports whether a destination must not reach the page. Browsers
// ignore ASCII tab and newline inside a URL and leading control characters
// and spaces, so those are removed before the scheme is checked.
func unsafeURL(dest []byte, image bool) bool {
	u := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, string(dest))
	u = strings.ToLower(strings.TrimLeft(u, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "))
	if image {
		for _, ok := range []string{"data:image/png", "data:image/gif", "data:image/jpeg", "data:image/webp"} {
			if strings.HasPrefix(u, ok) {
				return false
			}
		}
	}
	for _, bad := range []string{"javascript:", "vbscript:", "file:", "data:"} {
		if strings.HasPrefix(u, bad) {
			return true
		}
	}
	return false
}

type sanitizer struct{}

func (sanitizer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	src := reader.Source()
	var autolinks []*ast.AutoLink
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			if unsafeURL(n.Destination, false) {
				n.Destination = nil
			}
		case *ast.Image:
			if unsafeURL(n.Destination, true) {
				n.Destination = nil
			}
		case *ast.AutoLink:
			if unsafeURL(n.URL(src), false) {
				autolinks = append(autolinks, n)
			}
		}
		return ast.WalkContinue, nil
	})
	for _, a := range autolinks {
		a.Parent().ReplaceChild(a.Parent(), a, ast.NewString(a.Label(src)))
	}
}

type fenceRenderer struct{}

func (fenceRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, renderFence)
}

func renderFence(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.FencedCodeBlock)
	lang := n.Language(src)
	if !entering {
		_, _ = w.WriteString("</code></pre>\n")
		return ast.WalkContinue, nil
	}
	switch {
	case strings.EqualFold(string(lang), "mermaid"):
		_, _ = w.WriteString(`<pre class="mermaid-src" data-mermaid="1"><code>`)
	case lang != nil:
		_, _ = w.WriteString(`<pre><code class="language-`)
		_, _ = w.Write(util.EscapeHTML(lang))
		_, _ = w.WriteString(`">`)
	default:
		_, _ = w.WriteString("<pre><code>")
	}
	for i := 0; i < n.Lines().Len(); i++ {
		seg := n.Lines().At(i)
		_, _ = w.Write(util.EscapeHTML(seg.Value(src)))
	}
	return ast.WalkContinue, nil
}
