package spec

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// sectionNumberRe is a leading human section number ("2.1 ", "3. "): it
// is dropped so renumbering a document does not change every address.
var sectionNumberRe = regexp.MustCompile(`^\s*\d+(?:\.\d+)*\.?\s+`)

// HeadingSlug derives a heading's own slug (design §5.4 rules 1–2): lowercase,
// letters and digits kept — Unicode ones included, so `## Überblick` is
// `überblick` rather than an empty slug — and every run of anything else
// (whitespace, punctuation, symbols, `_`) collapsed to a single `_`, trimmed
// at both ends. The result never contains `__`, which is reserved as the
// level separator. Combining marks stay attached to their letter.
func HeadingSlug(heading string) string {
	heading = sectionNumberRe.ReplaceAllString(heading, "")
	var sb strings.Builder
	sep := false
	for _, r := range heading {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsNumber(r):
			if sep && sb.Len() > 0 {
				sb.WriteByte('_')
			}
			sep = false
			sb.WriteRune(unicode.ToLower(r))
		case unicode.IsMark(r) && sb.Len() > 0 && !sep:
			sb.WriteRune(r)
		default:
			sep = true
		}
	}
	return sb.String()
}

// TakenKeys is every name already resolvable in d: pin keys and path slugs.
// Pass it (plus the spec's known anchor keys) to MintKey so a new pin never
// shadows another section's address.
func (d *Doc) TakenKeys() map[string]bool {
	taken := map[string]bool{}
	for i := range d.Sections {
		s := &d.Sections[i]
		taken[s.PathSlug] = true
		if s.PinKey != "" {
			taken[s.PinKey] = true
		}
	}
	return taken
}

// MintKey proposes a readable pin id for sec: its own slug with `_` turned
// into `-` (`idle_timeout` → `idle-timeout`), suffixed `-2`, `-3`, ... until it
// is not in taken. A section that already has a pin keeps it. The caller adds
// the result to taken if it mints several keys in a row.
func MintKey(sec *Section, taken map[string]bool) string {
	if sec.PinKey != "" {
		return sec.PinKey
	}
	own := sec.OwnSlug
	if sec.Kind == KindRoot {
		if sec.Level == 0 {
			return RootSlug // the preamble's identity is structural
		}
		own = HeadingSlug(sec.Heading)
	}
	base := strings.ReplaceAll(own, "_", "-")
	if base == "" || !ValidKey(base) {
		base = "section"
	}
	key := base
	for n := 2; taken[key]; n++ {
		key = fmt.Sprintf("%s-%d", base, n)
	}
	return key
}
