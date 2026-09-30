package spec

import (
	"bytes"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// PlainText is the text a reader sees when body is rendered: inline markup,
// link targets, list markers, table pipes and raw HTML removed, entities
// decoded, blocks separated by newlines. offs maps every byte of text (and
// one past the end) to a byte offset in body; bytes produced by decoding an
// entity or escape map to the start of their source span.
func PlainText(body string) (string, []int) {
	src := []byte(body)
	md := goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList))
	doc := md.Parser().Parse(text.NewReader(src))
	var out []byte
	var offs []int
	emit := func(b []byte, at int, exact bool) {
		for i, c := range b {
			out = append(out, c)
			o := at
			if exact {
				o = at + i
			}
			offs = append(offs, min(o, len(src))) // tab padding can overshoot
		}
	}
	sep := func(at int) {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			emit([]byte{'\n'}, at, false)
		}
	}
	segText := func(seg text.Segment) {
		raw := seg.Value(src)
		dec := []byte(html.UnescapeString(string(util.UnescapePunctuations(raw))))
		emit(dec, seg.Start, seg.Padding == 0 && bytes.Equal(dec, raw))
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Text:
			segText(n.Segment)
			if n.SoftLineBreak() || n.HardLineBreak() {
				emit([]byte{'\n'}, n.Segment.Stop, false)
			}
		case *ast.String:
			emit(n.Value, lastOff(offs), false)
		case *ast.AutoLink:
			label := n.Label(src)
			at := lastOff(offs)
			if i := bytes.Index(src[at:], label); i >= 0 {
				at += i
			}
			emit(label, at, true)
			return ast.WalkSkipChildren, nil
		case *ast.Image, *ast.RawHTML, *ast.HTMLBlock:
			return ast.WalkSkipChildren, nil
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			l := n.Lines()
			if l.Len() > 0 {
				sep(l.At(0).Start)
			}
			for i := 0; i < l.Len(); i++ {
				seg := l.At(i)
				emit(seg.Value(src), seg.Start, true)
			}
			return ast.WalkSkipChildren, nil
		case *extast.TableCell:
			if len(out) > 0 && out[len(out)-1] != '\n' {
				emit([]byte{'\t'}, lastOff(offs), false)
			}
		default:
			if n.Type() == ast.TypeBlock {
				at := lastOff(offs)
				if l := n.Lines(); l != nil && l.Len() > 0 {
					at = l.At(0).Start
				}
				sep(at)
			}
		}
		return ast.WalkContinue, nil
	})
	s := strings.TrimRight(string(out), "\n")
	offs = append(offs[:len(s)], len(src))
	return s, offs
}

func lastOff(offs []int) int {
	if len(offs) == 0 {
		return 0
	}
	return offs[len(offs)-1] + 1
}
