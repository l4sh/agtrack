// Package spec is the markdown engine behind the `spec` entity: it splits a
// document into addressable sections, reconciles a new revision against the
// previous one, and splices pin comments back into the source.
//
// It is pure: no database access. The store layer calls it.
//
// Offsets. Every byte offset a Section reports (ByteStart, ByteEnd,
// HeadingByteStart, SubtreeEnd) is a UTF-8 byte offset into Doc.Source, the
// bytes exactly as read from disk. Text fields (Body, Heading) are cut from
// Doc.Text, the normalised copy (no BOM, LF line endings), so hashes do not
// depend on the platform that last saved the file.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// ParserVersion is recorded on every imported revision. Bump it whenever a
// change here could produce different sections for the same bytes.
const ParserVersion = "agtrack-spec/2"

// Section kinds.
const (
	KindRoot    = "root"
	KindHeading = "heading"
	KindBlock   = "block"
)

// RootSlug is the reserved path slug of the preamble.
const RootSlug = "root"

// FrontMatter is a leading `---` block. Only scalar `key: value` lines are
// parsed; nested YAML is kept in Raw and otherwise ignored.
type FrontMatter struct {
	Raw    string            // lines between the fences, verbatim (normalised)
	Fields map[string]string // scalar keys; surrounding quotes removed
	Keys   []string          // Fields keys in document order
}

// Section is one addressable unit of a document.
type Section struct {
	Ordinal       int
	Kind          string // KindRoot | KindHeading | KindBlock
	Level         int    // 1..6; 0 for root and blocks
	Heading       string // plain text, inline markup and {#key} stripped
	Number        string // "2.1", derived, humans only; "" for root and blocks
	Path          string // "Tokens / Rotation"
	PathSlug      string // "tokens__rotation"
	OwnSlug       string // "rotation"
	PinKey        string // from a pin comment, a {#key} suffix, or the block key
	PinLine       int    // 1-based line of the pin comment or block anchor; 0 if none
	ParentOrdinal int    // -1 for top-level headings and root
	Body          string // own content, descendants excluded, blank edge lines trimmed
	BodySHA       string
	ContextSHA    string
	LineStart     int // 1-based, inclusive: heading (or anchor) line
	LineEnd       int // 1-based, inclusive: last non-blank line of own content
	ByteStart     int // offset in Source of the heading (or anchor) line
	ByteEnd       int // offset in Source just past LineEnd's newline
	// HeadingByteStart is where a pin comment is inserted: the start of the
	// heading line (the first text line for Setext).
	HeadingByteStart int
	SubtreeEnd       int // ByteEnd of the last descendant, or ByteEnd
	HasMermaid       bool
	Images           []string // relative image references, in order, deduplicated
	PinFromSuffix    bool     // PinKey came from a {#key} suffix, not a comment

	headStart    int // normalised offsets used while building
	contentStart int
	contentEnd   int
}

// Doc is a parsed document.
type Doc struct {
	Source      []byte // as given to Parse
	Text        []byte // Normalize(Source)
	SHA         string // sha256 hex of Text
	FrontMatter *FrontMatter
	Title       string // front matter title, else the first H1, else ""
	Sections    []Section
	Warnings    []string

	norm2orig []int // offset in Text -> offset in Source
	lines     []int // start offset in Text of each line
}

var bom = []byte{0xEF, 0xBB, 0xBF}

// Normalize strips a UTF-8 byte order mark and converts CRLF and lone CR line
// endings to LF.
func Normalize(src []byte) []byte {
	out, _ := normalize(src)
	return out
}

// normalize also returns, for every offset in the output (and one past the
// end), the corresponding offset in src.
func normalize(src []byte) ([]byte, []int) {
	start := 0
	if bytes.HasPrefix(src, bom) {
		start = len(bom)
	}
	out := make([]byte, 0, len(src)-start)
	m := make([]int, 0, len(src)-start+1)
	for i := start; i < len(src); i++ {
		c := src[i]
		if c == '\r' {
			m = append(m, i)
			out = append(out, '\n')
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			continue
		}
		m = append(m, i)
		out = append(out, c)
	}
	m = append(m, len(src))
	return out, m
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SHA returns the sha256 hex digest of Normalize(src): the value compared
// against spec_versions.sha256 to detect drift.
func SHA(src []byte) string { return sha(Normalize(src)) }

var (
	commentRe   = regexp.MustCompile(`^<!--\s*agtrack:([A-Za-z]+)\s*(.*?)\s*-->$`)
	keyRe       = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}_.-]*$`)
	suffixKeyRe = regexp.MustCompile(`\s*\{#([^\s{}]+)\}\s*$`)
	emptyATXRe  = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+#*)?[ \t]*$`)
)

// ValidKey reports whether key is usable as a pin id: it starts with a letter
// or digit and continues with letters, digits, `_`, `.` or `-`.
func ValidKey(key string) bool { return keyRe.MatchString(key) }

// event is one structural marker found at the top level of the document.
type event struct {
	kind      string // "heading", "pin", "block", "bad"
	lineStart int    // normalised offset of the marker's first line
	end       int    // normalised offset just past the marker's last line
	level     int
	heading   string
	key       string
	raw       string // for "bad": the comment text
}

// Parse splits src into sections. It fails only when the document is
// unusable as a spec: a pin id used twice (both lines are named).
func Parse(src []byte) (*Doc, error) {
	norm, m := normalize(src)
	d := &Doc{Source: src, Text: norm, SHA: sha(norm), norm2orig: m}
	d.lines = lineStarts(norm)

	// Blank the front matter out (same length, spaces) so goldmark neither
	// reads its closing fence as a Setext underline nor shifts any offset.
	parseText := norm
	bodyStart := 0
	if fm, end := frontMatter(norm); fm != nil {
		d.FrontMatter = fm
		parseText = append([]byte(nil), norm...)
		for i := 0; i < end; i++ {
			if parseText[i] != '\n' {
				parseText[i] = ' '
			}
		}
		bodyStart = end
	}

	root := goldmark.New().Parser().Parse(text.NewReader(parseText))
	events, blocks := d.collect(root, parseText, bodyStart)
	if err := d.build(events, bodyStart); err != nil {
		return nil, err
	}
	d.attach(root, parseText, blocks)
	d.finish()

	if d.FrontMatter != nil && d.FrontMatter.Fields["title"] != "" {
		d.Title = d.FrontMatter.Fields["title"]
	} else {
		for i := range d.Sections {
			if s := &d.Sections[i]; s.Level == 1 {
				d.Title = s.Heading
				break
			}
		}
	}
	return d, nil
}

func lineStarts(b []byte) []int {
	ls := []int{0}
	for i, c := range b {
		if c == '\n' && i+1 < len(b) {
			ls = append(ls, i+1)
		}
	}
	return ls
}

// line returns the 1-based line number of normalised offset off.
func (d *Doc) line(off int) int {
	return sort.Search(len(d.lines), func(i int) bool { return d.lines[i] > off })
}

func (d *Doc) orig(off int) int { return d.norm2orig[off] }

func lineStartOf(b []byte, off int) int {
	for off > 0 && b[off-1] != '\n' {
		off--
	}
	return off
}

func lineEndOf(b []byte, off int) int {
	for off < len(b) && b[off] != '\n' {
		off++
	}
	if off < len(b) {
		off++
	}
	return off
}

// frontMatter recognises a `---` fence on the very first line closed by a
// `---` or `...` line. It returns the offset just past the closing fence.
func frontMatter(b []byte) (*FrontMatter, int) {
	first := lineEndOf(b, 0)
	if strings.TrimRight(string(b[:first]), " \t\n") != "---" {
		return nil, 0
	}
	for off := first; off < len(b); {
		end := lineEndOf(b, off)
		l := strings.TrimRight(string(b[off:end]), " \t\n")
		if l == "---" || l == "..." {
			fm := &FrontMatter{Raw: string(b[first:off]), Fields: map[string]string{}}
			for _, ln := range strings.Split(fm.Raw, "\n") {
				k, v, ok := scalarField(ln)
				if !ok {
					continue
				}
				if _, dup := fm.Fields[k]; !dup {
					fm.Keys = append(fm.Keys, k)
				}
				fm.Fields[k] = v
			}
			return fm, end
		}
		off = end
	}
	return nil, 0
}

var fieldRe = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]*)[ \t]*:(?:[ \t]+(.*))?$`)

func scalarField(ln string) (string, string, bool) {
	sm := fieldRe.FindStringSubmatch(strings.TrimRight(ln, " \t"))
	if sm == nil {
		return "", "", false
	}
	v := strings.TrimSpace(sm[2])
	if v == "" || v == "|" || v == ">" || strings.HasPrefix(v, "#") {
		return "", "", false // a nested mapping, block scalar or comment: not ours
	}
	if n := len(v); n >= 2 && (v[0] == '"' && v[n-1] == '"' || v[0] == '\'' && v[n-1] == '\'') {
		v = v[1 : n-1]
	} else if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return sm[1], v, true
}

// collect walks the top-level blocks for headings and agtrack comments.
// Headings nested in lists or block quotes are content, not sections. It also
// returns the start offset of every top-level block, for attach.
func (d *Doc) collect(root ast.Node, b []byte, cursor int) ([]event, []topBlock) {
	var evs []event
	var blocks []topBlock
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		start, end, ok := span(n, b)
		switch n := n.(type) {
		case *ast.Heading:
			if !ok {
				// Empty ATX heading: goldmark keeps no segment. Find its line.
				start, end = findEmptyATX(b, cursor, n.Level)
			} else {
				start = lineStartOf(b, start)
				end = lineEndOf(b, max(start, end-1))
				if !isATX(b[start:]) {
					end = lineEndOf(b, end) // Setext: include the underline
				}
			}
			evs = append(evs, event{kind: "heading", lineStart: start, end: end,
				level: n.Level, heading: plainText(n, b)})
		case *ast.HTMLBlock:
			if !ok {
				break
			}
			start = lineStartOf(b, start)
			raw := strings.TrimSpace(string(b[start:end]))
			sm := commentRe.FindStringSubmatch(raw)
			if sm == nil {
				break
			}
			end = lineEndOf(b, start)
			ev := event{lineStart: start, end: end, key: sm[2], raw: raw}
			switch {
			case sm[1] == "id" && ValidKey(sm[2]):
				ev.kind = "pin"
			case sm[1] == "block" && ValidKey(sm[2]):
				ev.kind = "block"
			default:
				ev.kind = "bad"
			}
			evs = append(evs, ev)
		}
		if ok {
			blocks = append(blocks, topBlock{n, start})
			cursor = end
		} else if len(evs) > 0 && evs[len(evs)-1].end > cursor {
			cursor = evs[len(evs)-1].end
		}
	}
	return evs, blocks
}

type topBlock struct {
	n     ast.Node
	start int
}

// span returns the byte range covered by a block's own lines, descending to
// the first/last child when the block has none (lists, quotes).
func span(n ast.Node, b []byte) (int, int, bool) {
	if f, ok := n.(*ast.FencedCodeBlock); ok && f.Info != nil {
		s := lineStartOf(b, f.Info.Segment.Start)
		e := lineEndOf(b, f.Info.Segment.Stop)
		if l := f.Lines(); l.Len() > 0 {
			e = l.At(l.Len() - 1).Stop
		}
		return s, e, true
	}
	if l := n.Lines(); l != nil && l.Len() > 0 {
		return l.At(0).Start, l.At(l.Len() - 1).Stop, true
	}
	if n.Type() == ast.TypeBlock && n.FirstChild() != nil {
		s, _, ok1 := span(n.FirstChild(), b)
		_, e, ok2 := span(n.LastChild(), b)
		if ok1 && ok2 {
			return s, e, true
		}
	}
	return 0, 0, false
}

func isATX(line []byte) bool {
	t := bytes.TrimLeft(line, " ")
	return len(line)-len(t) <= 3 && len(t) > 0 && t[0] == '#'
}

func findEmptyATX(b []byte, from, level int) (int, int) {
	for off := from; off < len(b); {
		end := lineEndOf(b, off)
		if sm := emptyATXRe.FindSubmatch(bytes.TrimRight(b[off:end], "\n")); sm != nil && len(sm[1]) == level {
			return off, end
		}
		off = end
	}
	return from, from
}

// plainText is the heading's text with inline markup removed.
func plainText(n ast.Node, src []byte) string {
	var sb strings.Builder
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			switch c := c.(type) {
			case *ast.Text:
				sb.Write(util.UnescapePunctuations(c.Segment.Value(src)))
				if c.SoftLineBreak() || c.HardLineBreak() {
					sb.WriteByte(' ')
				}
			case *ast.String:
				sb.Write(c.Value)
			case *ast.RawHTML:
				// dropped
			case *ast.AutoLink:
				sb.Write(c.Label(src))
			default:
				walk(c)
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

// build turns events into sections with their structure and bodies.
func (d *Doc) build(evs []event, bodyStart int) error {
	b := d.Text
	rootSec := Section{Ordinal: 0, Kind: KindRoot, ParentOrdinal: -1, PathSlug: RootSlug,
		OwnSlug: RootSlug, headStart: bodyStart, contentStart: bodyStart, contentEnd: -1}
	secs := []Section{rootSec}

	// Title elision: a lone H1 opening the document (nothing but blank lines
	// before it) is the document title. It becomes the root section, so its
	// children are top-level and addresses are not all prefixed by the title.
	h1s, firstHeading := 0, -1
	for i, e := range evs {
		if e.kind == "heading" {
			if firstHeading < 0 {
				firstHeading = i
			}
			if e.level == 1 {
				h1s++
			}
		}
	}
	elide := -1
	if firstHeading >= 0 && h1s == 1 && evs[firstHeading].level == 1 {
		lead := evs[firstHeading].lineStart
		if firstHeading > 0 && evs[firstHeading-1].kind == "pin" {
			lead = evs[firstHeading-1].lineStart
		}
		if len(bytes.TrimSpace(b[bodyStart:lead])) == 0 {
			elide = firstHeading
		}
	}

	pinSeen := map[string]int{} // key -> line
	claim := func(key string, line int) error {
		if prev, ok := pinSeen[key]; ok {
			return &errs.Error{Code: errs.ExitError, Token: errs.TokInvalidArgument,
				Message: fmt.Sprintf("duplicate pin id %q at lines %d and %d; ids must be unique, rename one", key, prev, line),
				Extra:   map[string]any{"key": key, "lines": []int{prev, line}}}
		}
		pinSeen[key] = line
		return nil
	}

	type frame struct{ ord, level int }
	var stack []frame
	var pending *event
	current := 0 // section whose own content is running
	closeAt := func(off int) {
		if secs[current].contentEnd < 0 {
			secs[current].contentEnd = off
		}
	}

	for i := range evs {
		e := evs[i]
		if pending != nil && e.kind != "heading" {
			d.warnf("line %d: pin comment %q is not followed by a heading; ignored", d.line(pending.lineStart), pending.key)
			pending = nil
		}
		switch e.kind {
		case "bad":
			d.warnf("line %d: malformed agtrack comment %s; ignored", d.line(e.lineStart), e.raw)
		case "pin":
			ev := e
			pending = &ev
		case "heading":
			if pending != nil && len(bytes.TrimSpace(b[pending.end:e.lineStart])) != 0 {
				d.warnf("line %d: pin comment %q is not followed by a heading; ignored", d.line(pending.lineStart), pending.key)
				pending = nil
			}
			lead := e.lineStart
			if pending != nil {
				lead = pending.lineStart
			}
			closeAt(lead)
			heading, suffixKey := e.heading, ""
			if sm := suffixKeyRe.FindStringSubmatchIndex(heading); sm != nil {
				suffixKey = heading[sm[2]:sm[3]]
				heading = heading[:sm[0]]
				if !ValidKey(suffixKey) {
					d.warnf("line %d: {#%s} is not a valid id; ignored", d.line(e.lineStart), suffixKey)
					suffixKey = ""
				}
			}
			s := Section{Kind: KindHeading, Level: e.level, Heading: heading,
				headStart: e.lineStart, contentStart: e.end, contentEnd: -1, ParentOrdinal: -1}
			switch {
			case pending != nil:
				s.PinKey, s.PinLine = pending.key, d.line(pending.lineStart)
				if suffixKey != "" && suffixKey != s.PinKey {
					d.warnf("line %d: heading has both pin %q and {#%s}; the pin comment wins", d.line(e.lineStart), s.PinKey, suffixKey)
				}
			case suffixKey != "":
				s.PinKey, s.PinFromSuffix = suffixKey, true
			}
			pending = nil
			if s.PinKey != "" {
				line := s.PinLine
				if line == 0 {
					line = d.line(e.lineStart)
				}
				if err := claim(s.PinKey, line); err != nil {
					return err
				}
			}
			if i == elide {
				r := &secs[0]
				r.Level, r.Heading = 1, s.Heading
				r.PinKey, r.PinLine, r.PinFromSuffix = s.PinKey, s.PinLine, s.PinFromSuffix
				r.headStart, r.contentStart, r.contentEnd = s.headStart, s.contentStart, -1
				current = 0
				continue
			}
			for len(stack) > 0 && stack[len(stack)-1].level >= e.level {
				stack = stack[:len(stack)-1]
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				s.ParentOrdinal = p.ord
				if e.level > p.level+1 {
					d.warnf("line %d: heading level %d skips from level %d; attached to %q", d.line(e.lineStart), e.level, p.level, secs[p.ord].Heading)
				}
			}
			s.Ordinal = len(secs)
			secs = append(secs, s)
			stack = append(stack, frame{s.Ordinal, e.level})
			current = s.Ordinal
		case "block":
			closeAt(e.lineStart)
			if err := claim(e.key, d.line(e.lineStart)); err != nil {
				return err
			}
			parent := 0
			if len(stack) > 0 {
				parent = stack[len(stack)-1].ord
			}
			s := Section{Ordinal: len(secs), Kind: KindBlock, PinKey: e.key, PinLine: d.line(e.lineStart),
				ParentOrdinal: parent, headStart: e.lineStart, contentStart: e.end, contentEnd: -1}
			secs = append(secs, s)
			current = s.Ordinal
		}
	}
	if pending != nil {
		d.warnf("line %d: pin comment %q is not followed by a heading; ignored", d.line(pending.lineStart), pending.key)
	}
	closeAt(len(b))
	d.Sections = secs
	d.derive()
	return nil
}

func (d *Doc) warnf(format string, a ...any) {
	d.Warnings = append(d.Warnings, fmt.Sprintf(format, a...))
}

// derive fills slugs, paths and numbers, which depend on the whole tree.
func (d *Doc) derive() {
	used := map[string]bool{}
	counts := map[int]int{} // parent ordinal -> children numbered so far
	for i := range d.Sections {
		s := &d.Sections[i]
		var parent *Section
		if s.ParentOrdinal >= 0 {
			parent = &d.Sections[s.ParentOrdinal]
		}
		switch s.Kind {
		case KindRoot:
			s.Path = s.Heading
			used[RootSlug] = true
			continue
		case KindHeading:
			s.OwnSlug = HeadingSlug(s.Heading)
			if s.OwnSlug == "" {
				s.OwnSlug = "section"
			}
			counts[s.ParentOrdinal]++
			n := fmt.Sprint(counts[s.ParentOrdinal])
			if parent != nil {
				n = parent.Number + "." + n
			}
			s.Number = n
		case KindBlock:
			s.OwnSlug = s.PinKey
			s.Heading = ""
		}
		prefix, pathPrefix := "", ""
		if parent != nil && parent.Kind != KindRoot {
			prefix, pathPrefix = parent.PathSlug+"__", parent.Path+" / "
		}
		label := s.Heading
		if s.Kind == KindBlock {
			label = s.PinKey
		}
		s.Path = pathPrefix + label
		base := s.OwnSlug
		slug := prefix + base
		for n := 2; used[slug]; n++ {
			s.OwnSlug = fmt.Sprintf("%s_%d", base, n)
			slug = prefix + s.OwnSlug
		}
		if s.OwnSlug != base {
			d.warnf("line %d: duplicate heading %q under the same parent; addressed as %s", d.line(s.headStart), label, slug)
		}
		used[slug] = true
		s.PathSlug = slug
	}
}

// attach assigns Mermaid fences and relative images to the section whose own
// content holds them.
func (d *Doc) attach(root ast.Node, b []byte, blocks []topBlock) {
	owner := func(off int) *Section {
		var best *Section
		for i := range d.Sections {
			s := &d.Sections[i]
			if off >= s.contentStart && off < s.contentEnd {
				best = s
			}
		}
		return best
	}
	for _, bn := range blocks {
		s := owner(bn.start)
		if s == nil {
			continue
		}
		_ = ast.Walk(bn.n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			switch n := n.(type) {
			case *ast.FencedCodeBlock:
				if strings.EqualFold(string(n.Language(b)), "mermaid") {
					s.HasMermaid = true
				}
			case *ast.Image:
				if dst := string(n.Destination); isRelativeRef(dst) && !contains(s.Images, dst) {
					s.Images = append(s.Images, dst)
				}
			}
			return ast.WalkContinue, nil
		})
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func isRelativeRef(u string) bool {
	return u != "" && !schemeRe.MatchString(u) && !strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "#")
}

// finish computes bodies, hashes and source offsets.
func (d *Doc) finish() {
	b := d.Text
	for i := range d.Sections {
		s := &d.Sections[i]
		bs, be := trimBlankLines(b, s.contentStart, s.contentEnd)
		s.Body = string(b[bs:be])
		s.BodySHA = sha([]byte(s.Body))

		start, end := s.headStart, s.contentStart // empty body: heading line only
		switch {
		case be > bs:
			end = lineEndOf(b, be-1)
			if s.Kind == KindRoot && s.Level == 0 {
				start = bs // a preamble starts at its first line of text
			}
		case s.Kind == KindRoot && s.Level == 0:
			end = start // an empty preamble covers nothing
		}
		s.LineStart = d.line(start)
		s.LineEnd = s.LineStart
		if end > start {
			s.LineEnd = d.line(end - 1)
		}
		s.ByteStart, s.ByteEnd = d.orig(start), d.orig(end)
		s.HeadingByteStart = s.ByteStart
	}
	for i := range d.Sections {
		s := &d.Sections[i]
		s.SubtreeEnd = s.ByteEnd
		for j := i + 1; j < len(d.Sections) && d.isDescendant(j, i); j++ {
			if e := d.Sections[j].ByteEnd; e > s.SubtreeEnd {
				s.SubtreeEnd = e
			}
		}
		s.ContextSHA = d.contextSHA(i)
	}
}

func (d *Doc) isDescendant(j, i int) bool {
	for p := d.Sections[j].ParentOrdinal; p >= 0; p = d.Sections[p].ParentOrdinal {
		if p == i {
			return true
		}
	}
	return false
}

// contextSHA covers ancestor headings, the own heading and the body (design
// §5.3), so a move under a different parent needs review even when the body
// is unchanged, while reordering siblings changes nothing.
func (d *Doc) contextSHA(i int) string {
	var chain []string
	for p := i; p >= 0; p = d.Sections[p].ParentOrdinal {
		s := &d.Sections[p]
		label := s.Heading
		if s.Kind == KindBlock {
			label = "#" + s.PinKey
		}
		chain = append(chain, label)
	}
	var sb strings.Builder
	for k := len(chain) - 1; k >= 0; k-- {
		sb.WriteString(chain[k])
		sb.WriteByte('\n')
	}
	sb.WriteByte(0)
	sb.WriteString(d.Sections[i].Body)
	return sha([]byte(sb.String()))
}

// trimBlankLines narrows [s, e) to whole lines, dropping blank lines at both
// edges and the final newline.
func trimBlankLines(b []byte, s, e int) (int, int) {
	for s < e {
		le := lineEndOf(b, s)
		if le > e {
			le = e
		}
		if len(bytes.TrimSpace(b[s:le])) != 0 {
			break
		}
		s = le
	}
	for e > s {
		x := e
		if b[x-1] == '\n' {
			x--
		}
		ls := max(lineStartOf(b, x), s)
		if len(bytes.TrimSpace(b[ls:e])) != 0 {
			break
		}
		e = ls
	}
	if e > s && b[e-1] == '\n' {
		e--
	}
	return s, e
}

// Section returns the section with the given ordinal, or nil.
func (d *Doc) Section(ordinal int) *Section {
	if ordinal < 0 || ordinal >= len(d.Sections) {
		return nil
	}
	return &d.Sections[ordinal]
}

// Children returns the direct children of the section with ordinal i.
func (d *Doc) Children(i int) []*Section {
	var out []*Section
	for j := range d.Sections {
		if d.Sections[j].ParentOrdinal == i && j != i {
			out = append(out, &d.Sections[j])
		}
	}
	return out
}

// Subtree returns the section and all its descendants, in document order.
func (d *Doc) Subtree(i int) []*Section {
	out := []*Section{&d.Sections[i]}
	for j := i + 1; j < len(d.Sections) && d.isDescendant(j, i); j++ {
		out = append(out, &d.Sections[j])
	}
	return out
}
