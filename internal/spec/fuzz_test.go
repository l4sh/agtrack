package spec

import (
	"os"
	"strings"
	"testing"
)

// FuzzParse checks the invariants every document must satisfy: no panic,
// offsets inside the source, bodies cut from the text, and a pin splice that
// only ever adds one line.
func FuzzParse(f *testing.F) {
	for _, p := range []string{"testdata/example-spec.md"} {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(string(b))
		}
	}
	for _, s := range []string{"", "#", "---\n---\n", "a\n===\n", "```\n# x\n", "<!-- agtrack:id a -->\n# A {#b}\n",
		"\r\n## A\r\n<!-- agtrack:block k -->\r\n", "- # x\n> ## y\n", "## Ü\n\n### ü\n", "#\n##\n###\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		d, err := Parse([]byte(src))
		if err != nil {
			return
		}
		norm := string(d.Text)
		seen := map[string]bool{}
		for i := range d.Sections {
			s := &d.Sections[i]
			if seen[s.PathSlug] {
				t.Fatalf("duplicate path slug %q", s.PathSlug)
			}
			seen[s.PathSlug] = true
			if s.ByteStart < 0 || s.ByteEnd < s.ByteStart || s.SubtreeEnd < s.ByteEnd || s.SubtreeEnd > len(src) {
				t.Fatalf("%s offsets %d %d %d (len %d)", s.PathSlug, s.ByteStart, s.ByteEnd, s.SubtreeEnd, len(src))
			}
			if !strings.Contains(norm, s.Body) || s.LineEnd < s.LineStart {
				t.Fatalf("%s body/lines", s.PathSlug)
			}
			if s.Kind == KindHeading && s.PinKey == "" {
				out, err := InsertPin([]byte(src), s, "zz-fuzz-pin")
				if err != nil {
					continue // a heading a comment line cannot attach to is refused, not corrupted
				}
				if len(out) <= len(src) || !strings.Contains(string(out), PinComment("zz-fuzz-pin")) {
					t.Fatalf("bad splice")
				}
			}
		}
	})
}
