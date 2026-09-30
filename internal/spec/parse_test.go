package spec

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/errs"
)

func mustParse(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return d
}

func slugs(d *Doc) []string {
	var out []string
	for _, s := range d.Sections {
		out = append(out, s.PathSlug)
	}
	return out
}

func bySlug(t *testing.T, d *Doc, slug string) *Section {
	t.Helper()
	for i := range d.Sections {
		if d.Sections[i].PathSlug == slug {
			return &d.Sections[i]
		}
	}
	t.Fatalf("no section %s in %v", slug, slugs(d))
	return nil
}

func hasWarning(d *Doc, sub string) bool {
	for _, w := range d.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func exampleDoc(t *testing.T) *Doc {
	t.Helper()
	b, err := os.ReadFile("testdata/example-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseExample(t *testing.T) {
	d := exampleDoc(t)
	want := []string{"root", "overview", "tokens", "tokens__rotation", "tokens__rotation__lifetimes",
		"tokens__revocation", "sessions", "sessions__revocation", "sessions__idle_timeout"}
	if got := slugs(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("slugs\n got %v\nwant %v", got, want)
	}
	if len(d.Warnings) != 0 {
		t.Errorf("warnings: %v", d.Warnings)
	}
	if d.Title != "Authentication v2" {
		t.Errorf("title %q", d.Title)
	}
	if fm := d.FrontMatter; fm == nil || !reflect.DeepEqual(fm.Keys, []string{"title", "status", "owner"}) || fm.Fields["owner"] != "alex" {
		t.Errorf("front matter %+v", fm)
	}

	root := bySlug(t, d, "root")
	if root.Kind != KindRoot || !strings.HasPrefix(root.Body, "This document defines") || root.LineStart != 7 {
		t.Errorf("root %+v", root)
	}
	if !bySlug(t, d, "overview").HasMermaid || bySlug(t, d, "tokens").HasMermaid {
		t.Error("mermaid flag")
	}

	rot := bySlug(t, d, "tokens__rotation")
	if rot.PinKey != "grace-policy" || rot.PinLine != 27 || rot.LineStart != 28 || rot.LineEnd != 31 {
		t.Errorf("rotation pin/lines: %+v", rot)
	}
	if rot.Number != "2.1" || rot.Path != "Tokens / Rotation" || rot.Level != 3 || rot.OwnSlug != "rotation" {
		t.Errorf("rotation meta: %+v", rot)
	}
	if rot.Body != "Refresh tokens rotate on every use. The previous token stays valid for a grace\nwindow of 60s to absorb in-flight requests." {
		t.Errorf("rotation body %q", rot.Body)
	}
	if !strings.HasPrefix(string(d.Source[rot.HeadingByteStart:]), "### Rotation\n") {
		t.Errorf("HeadingByteStart points at %q", d.Source[rot.HeadingByteStart:rot.HeadingByteStart+20])
	}

	blk := bySlug(t, d, "tokens__rotation__lifetimes")
	if blk.Kind != KindBlock || blk.PinKey != "lifetimes" || blk.ParentOrdinal != rot.Ordinal || blk.Level != 0 || blk.Number != "" {
		t.Errorf("block %+v", blk)
	}
	if !strings.HasPrefix(blk.Body, "| Token kind |") || !strings.HasSuffix(blk.Body, "TOKEN_GRACE_SECONDS\n```") {
		t.Errorf("block body %q", blk.Body)
	}
	// The block is carved out of Rotation, and a `#` in the bash fence is not a heading.
	if rot.SubtreeEnd != blk.ByteEnd {
		t.Errorf("rotation subtree ends %d, block %d", rot.SubtreeEnd, blk.ByteEnd)
	}
	tokens := bySlug(t, d, "tokens")
	if tokens.SubtreeEnd != bySlug(t, d, "tokens__revocation").ByteEnd {
		t.Error("tokens subtree end")
	}
	for _, s := range []string{"tokens__revocation", "sessions__revocation"} {
		if bySlug(t, d, s).OwnSlug != "revocation" {
			t.Errorf("%s own slug", s)
		}
	}
	if n := bySlug(t, d, "sessions__idle_timeout").Number; n != "3.2" {
		t.Errorf("idle number %s", n)
	}
	// Every section's source span starts with its first line.
	for _, s := range d.Sections[1:] {
		if s.ByteStart != s.HeadingByteStart || s.ByteEnd <= s.ByteStart || s.SubtreeEnd < s.ByteEnd {
			t.Errorf("%s offsets %d %d %d", s.PathSlug, s.ByteStart, s.ByteEnd, s.SubtreeEnd)
		}
	}
}

func TestParseFences(t *testing.T) {
	src := "# T\n\n## A\n\n```bash\n# not a heading\n## nor this\n```\n\n~~~\n# tilde\n~~~\n\n````md\n```\n# inside a longer fence\n```\n````\n\n## B\n\n```\n# unclosed fence swallows the rest\n## C\n"
	d := mustParse(t, src)
	if got, want := slugs(d), []string{"root", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if !strings.Contains(bySlug(t, d, "a").Body, "# inside a longer fence") {
		t.Error("fence body lost")
	}
}

func TestParseATX(t *testing.T) {
	src := "   ## Three spaces\n\n    ## four spaces is code\n\n#not a heading\n\n## Closed ##\n\n##\n\ntext\n\n## `code` *em* [link](x) \\*esc\\*\n"
	d := mustParse(t, src)
	got := []string{}
	for _, s := range d.Sections[1:] {
		got = append(got, s.Heading+"|"+s.PathSlug)
	}
	want := []string{"Three spaces|three_spaces", "Closed|closed", "|section", "code em link *esc*|code_em_link_esc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if !strings.Contains(bySlug(t, d, "three_spaces").Body, "    ## four spaces is code") {
		t.Error("indented code should be body")
	}
	if b := bySlug(t, d, "section").Body; b != "text" {
		t.Errorf("empty heading body %q", b)
	}
	if s := bySlug(t, d, "section"); s.LineStart != 9 {
		t.Errorf("empty heading line %d", s.LineStart)
	}
}

func TestParseSetextAndFrontMatter(t *testing.T) {
	src := "---\ntitle: \"Quoted: title\"\nnested:\n  a: 1\nlist: [x]\n---\nIntro line.\n\nTitle\n=====\n\nPara.\n\nSub\nheading\n---\n\nBody.\n"
	d := mustParse(t, src)
	if got, want := slugs(d), []string{"root", "title", "title__sub_heading"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v (warnings %v)", got, want, d.Warnings)
	}
	// Not elided: there is text before the H1, so it is a normal section.
	d2 := mustParse(t, strings.Replace(src, "Intro line.\n", "", 1))
	if d2.Sections[0].Heading != "Title" || d2.Sections[0].Level != 1 {
		t.Errorf("elided root %+v", d2.Sections[0])
	}
	if got, want := slugs(d2), []string{"root", "sub_heading"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("elided got %v want %v", got, want)
	}
	if d.FrontMatter.Fields["title"] != "Quoted: title" || d.Title != "Quoted: title" {
		t.Errorf("title %q", d.FrontMatter.Fields["title"])
	}
	if _, ok := d.FrontMatter.Fields["nested"]; ok {
		t.Error("nested YAML must not be parsed")
	}
	if !strings.HasPrefix(d.FrontMatter.Raw, "title:") {
		t.Errorf("raw %q", d.FrontMatter.Raw)
	}
	sub := bySlug(t, d2, "sub_heading")
	if sub.Heading != "Sub heading" || sub.Level != 2 || sub.Body != "Body." {
		t.Errorf("setext %+v", sub)
	}
	if !strings.HasPrefix(string(d2.Source[sub.HeadingByteStart:]), "Sub\nheading\n---") {
		t.Error("setext HeadingByteStart")
	}

	// Without the text line, a lone `---` after front matter is not Setext.
	d3 := mustParse(t, "---\na: b\n---\n\n## Real\n")
	if got := slugs(d3); !reflect.DeepEqual(got, []string{"root", "real"}) || d3.Sections[1].LineStart != 5 {
		t.Errorf("fm-only %v %d", got, d3.Sections[1].LineStart)
	}
	// An unclosed fence is not front matter.
	if d4 := mustParse(t, "---\nno close\n\n## X\n"); d4.FrontMatter != nil {
		t.Error("unclosed front matter accepted")
	}
}

func TestParseTitleElision(t *testing.T) {
	d := mustParse(t, "# Auth spec\n\nLead paragraph.\n\n## Tokens\n\n### Rotation\n")
	if d.Title != "Auth spec" {
		t.Errorf("title %q", d.Title)
	}
	r := d.Sections[0]
	if r.Kind != KindRoot || r.Heading != "Auth spec" || r.Body != "Lead paragraph." || r.LineStart != 1 {
		t.Errorf("root %+v", r)
	}
	if got := slugs(d); !reflect.DeepEqual(got, []string{"root", "tokens", "tokens__rotation"}) {
		t.Errorf("slugs %v", got)
	}
	if d.Sections[2].Number != "1.1" {
		t.Errorf("number %s", d.Sections[2].Number)
	}
	// Two H1s: no elision.
	d2 := mustParse(t, "# A\n\n## x\n\n# B\n")
	if got := slugs(d2); !reflect.DeepEqual(got, []string{"root", "a", "a__x", "b"}) {
		t.Errorf("two h1 %v", got)
	}
}

func TestParsePins(t *testing.T) {
	src := "## A\n\n<!-- agtrack:id alpha -->\n\n## B\n\nb body\n<!-- agtrack:id stray -->\n\nmore\n\n## C {#gamma}\n\n<!-- agtrack:idx nope -->\n<!-- agtrack:id bad key -->\n## D\n"
	d := mustParse(t, src)
	b := bySlug(t, d, "b")
	if b.PinKey != "alpha" || b.PinLine != 3 {
		t.Errorf("pin with blank line: %+v", b)
	}
	if a := bySlug(t, d, "a"); a.Body != "" {
		t.Errorf("pin comment leaked into previous body: %q", a.Body)
	}
	if !strings.Contains(b.Body, "<!-- agtrack:id stray -->") || !hasWarning(d, `"stray" is not followed by a heading`) {
		t.Errorf("stray pin: body %q warnings %v", b.Body, d.Warnings)
	}
	c := bySlug(t, d, "c")
	if c.Heading != "C" || c.PinKey != "gamma" || c.PinLine != 0 {
		t.Errorf("{#key}: %+v", c)
	}
	if !hasWarning(d, "malformed agtrack comment <!-- agtrack:idx nope -->") || !hasWarning(d, "agtrack:id bad key") {
		t.Errorf("malformed warnings %v", d.Warnings)
	}
	if bySlug(t, d, "d").PinKey != "" {
		t.Error("malformed pin attached")
	}
}

func TestParseDuplicatePins(t *testing.T) {
	for name, src := range map[string]string{
		"comments":    "<!-- agtrack:id k -->\n## A\n\n<!-- agtrack:id k -->\n## B\n",
		"suffix":      "<!-- agtrack:id k -->\n## A\n\n## B {#k}\n",
		"block+pin":   "## A\n\n<!-- agtrack:id k -->\n## B\n\n<!-- agtrack:block k -->\n\ntext\n",
		"block+block": "## A\n\n<!-- agtrack:block k -->\n\nx\n\n<!-- agtrack:block k -->\n\ny\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(src))
			e, ok := errs.As(err)
			if !ok {
				t.Fatalf("want *errs.Error, got %v", err)
			}
			lines := e.Extra["lines"].([]int)
			if len(lines) != 2 || lines[0] >= lines[1] || !strings.Contains(e.Message, `duplicate pin id "k"`) {
				t.Errorf("error %q %v", e.Message, lines)
			}
		})
	}
	_, err := Parse([]byte("<!-- agtrack:id k -->\n## A\n\n<!-- agtrack:id k -->\n## B\n"))
	if err == nil || !strings.Contains(err.Error(), "lines 1 and 4") {
		t.Errorf("both lines: %v", err)
	}
}

func TestParseBlocks(t *testing.T) {
	src := "Preamble.\n\n<!-- agtrack:block intro -->\nIntro table.\n\n## A\n\nA text.\n\n<!-- agtrack:block one -->\n\nOne.\n\n<!-- agtrack:block two -->\nTwo.\n<!-- agtrack:id next -->\n## B\n"
	d := mustParse(t, src)
	if got, want := slugs(d), []string{"root", "intro", "a", "a__one", "a__two", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if d.Sections[0].Body != "Preamble." || d.Sections[1].ParentOrdinal != 0 || d.Sections[1].Body != "Intro table." {
		t.Errorf("preamble block: %+v %+v", d.Sections[0], d.Sections[1])
	}
	a := bySlug(t, d, "a")
	if a.Body != "A text." || bySlug(t, d, "a__one").Body != "One." || bySlug(t, d, "a__two").Body != "Two." {
		t.Errorf("block bodies %q %q %q", a.Body, bySlug(t, d, "a__one").Body, bySlug(t, d, "a__two").Body)
	}
	if two := bySlug(t, d, "a__two"); two.ParentOrdinal != a.Ordinal || two.Path != "A / two" || two.LineStart != 14 || two.LineEnd != 15 {
		t.Errorf("two %+v", two)
	}
	if bySlug(t, d, "b").PinKey != "next" {
		t.Error("pin after block")
	}
	if a.SubtreeEnd != bySlug(t, d, "a__two").ByteEnd {
		t.Error("subtree end includes blocks")
	}
}

func TestParseVerbatimBodies(t *testing.T) {
	src := "## Mixed\n\n- item\n  - nested *list*\n    1. deep\n\n| a | b |\n|---|--:|\n| 1 | 2 |\n\n<div align=\"center\">\n  <b>html</b>\n</div>\n\nFootnote[^1].\n\n[^1]: the note\n\n> ## quoted heading\n\n- ## list heading\n\n![diagram](img/flow.png) ![abs](https://x/y.png) ![root](/r.png) ![dup](img/flow.png) ![two](./b.svg)\n"
	d := mustParse(t, src)
	if got := slugs(d); !reflect.DeepEqual(got, []string{"root", "mixed"}) {
		t.Fatalf("nested headings became sections: %v", got)
	}
	m := bySlug(t, d, "mixed")
	want := strings.TrimSuffix(strings.TrimPrefix(src, "## Mixed\n\n"), "\n")
	if m.Body != want {
		t.Errorf("body not verbatim:\n%q\n%q", m.Body, want)
	}
	if !reflect.DeepEqual(m.Images, []string{"img/flow.png", "./b.svg"}) {
		t.Errorf("images %v", m.Images)
	}
}

func TestParseDuplicatesAndSkippedLevels(t *testing.T) {
	src := "## Tokens\n\n### Revocation\n\none\n\n### Revocation\n\ntwo\n\n### Revocation!\n\n## Sessions\n\n#### Deep\n\n### Revocation\n"
	d := mustParse(t, src)
	want := []string{"root", "tokens", "tokens__revocation", "tokens__revocation_2", "tokens__revocation_3", "sessions", "sessions__deep", "sessions__revocation"}
	if got := slugs(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if !hasWarning(d, "addressed as tokens__revocation_2") || !hasWarning(d, "addressed as tokens__revocation_3") {
		t.Errorf("duplicate warnings %v", d.Warnings)
	}
	deep := bySlug(t, d, "sessions__deep")
	if deep.ParentOrdinal != bySlug(t, d, "sessions").Ordinal || deep.Number != "2.1" || !hasWarning(d, "level 4 skips from level 2") {
		t.Errorf("skipped level %+v %v", deep, d.Warnings)
	}
	if bySlug(t, d, "sessions__revocation").ParentOrdinal != bySlug(t, d, "sessions").Ordinal {
		t.Error("sibling after skipped level")
	}
}

func TestParseUnicodeAndLineEndings(t *testing.T) {
	src := "\xEF\xBB\xBF---\r\ntitle: Ünïcode\r\n---\r\nPréambule — ok.\r\n\r\n## Überblick & Zusammenfassung\r\n\r\nText mit ümlauten.\r\n\r\n## 日本語 見出し\r\n\r\n本文。\r\n\r\n## Cáfé\r\n"
	d := mustParse(t, src)
	if got, want := slugs(d), []string{"root", "überblick_zusammenfassung", "日本語_見出し", "cáfé"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, s := range d.Sections[1:] {
		if !strings.HasPrefix(string(d.Source[s.HeadingByteStart:]), "## ") {
			t.Errorf("%s: HeadingByteStart %d not at heading in original bytes", s.PathSlug, s.HeadingByteStart)
		}
		if strings.Contains(s.Body, "\r") {
			t.Errorf("%s body not normalised", s.PathSlug)
		}
	}
	jp := bySlug(t, d, "日本語_見出し")
	if jp.Body != "本文。" || jp.LineStart != 10 || string(d.Source[jp.ByteEnd-2:jp.ByteEnd]) != "\r\n" {
		t.Errorf("jp %+v end %q", jp, d.Source[jp.ByteEnd-2:jp.ByteEnd])
	}
	if d.SHA != SHA([]byte(strings.ReplaceAll(strings.TrimPrefix(src, "\xEF\xBB\xBF"), "\r\n", "\n"))) {
		t.Error("SHA must be over normalised text")
	}
	if d.Title != "Ünïcode" || d.Sections[0].Body != "Préambule — ok." {
		t.Errorf("title %q root %q", d.Title, d.Sections[0].Body)
	}
}

func TestParseEmptyAndHashes(t *testing.T) {
	d := mustParse(t, "")
	if len(d.Sections) != 1 || d.Sections[0].Body != "" || d.Sections[0].PathSlug != "root" {
		t.Errorf("empty doc %+v", d.Sections)
	}
	a := mustParse(t, "## A\n\nsame\n\n## B\n\nsame\n")
	if a.Sections[1].BodySHA != a.Sections[2].BodySHA || a.Sections[1].ContextSHA == a.Sections[2].ContextSHA {
		t.Error("body sha equal, context sha must differ")
	}
	// Reordering siblings changes neither hash; a new parent changes context only.
	b := mustParse(t, "## B\n\nsame\n\n## A\n\nsame\n")
	if bySlug(t, a, "a").ContextSHA != bySlug(t, b, "a").ContextSHA {
		t.Error("sibling reorder changed context sha")
	}
	c := mustParse(t, "## P\n\n### A\n\nsame\n")
	if bySlug(t, c, "p__a").BodySHA != bySlug(t, a, "a").BodySHA || bySlug(t, c, "p__a").ContextSHA == bySlug(t, a, "a").ContextSHA {
		t.Error("move under parent: body same, context differs")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"a\r\nb\rc\n":   "a\nb\nc\n",
		"\xEF\xBB\xBFx": "x",
		"\r\r\n":        "\n\n",
		"plain":         "plain",
	} {
		if got := string(Normalize([]byte(in))); got != want {
			t.Errorf("Normalize(%q) = %q want %q", in, got, want)
		}
	}
}

func TestHeadingSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Idle timeout (v2)":    "idle_timeout_v2",
		"Tokens":               "tokens",
		"  snake__case  name ": "snake_case_name",
		"C++ / Go -- notes":    "c_go_notes",
		"Don't panic!":         "don_t_panic",
		"Überblick":            "überblick",
		"🚀":                    "",
		"2.1 Rotation":         "2_1_rotation",
		"ΑΒΓ":                  "αβγ",
	} {
		if got := HeadingSlug(in); got != want {
			t.Errorf("HeadingSlug(%q) = %q want %q", in, got, want)
		}
		if strings.Contains(HeadingSlug(in), "__") {
			t.Errorf("%q: slug contains separator", in)
		}
	}
}
