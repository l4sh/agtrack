package spec

import (
	"regexp"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
)

var numberRe = regexp.MustCompile(`^\d+(?:\.\d+)*$`)

// Lookup resolves the part of a section reference after `#`. Tiers, first hit
// wins:
//
//  1. a pin key (pin comment, {#key} or block key);
//  2. an exact path slug;
//  3. a glob (`*`, `?`) over path slugs — `tokens__*` is the subtree below
//     tokens, excluding tokens itself — returning every match in order;
//  4. a positional number such as `2.1`; positional is then true and the
//     caller must warn that the address is unstable;
//  5. a path-slug suffix on a `__` boundary (`revocation` matches
//     `tokens__revocation`); several matches are an ambiguous-reference error
//     listing the candidates.
//
// A miss is a not-found error.
func (d *Doc) Lookup(ref string) (secs []*Section, positional bool, err error) {
	if ref == "" {
		return nil, false, errs.Usage("empty section reference")
	}
	for i := range d.Sections {
		if d.Sections[i].PinKey == ref {
			return []*Section{&d.Sections[i]}, false, nil
		}
	}
	for i := range d.Sections {
		if d.Sections[i].PathSlug == ref {
			return []*Section{&d.Sections[i]}, false, nil
		}
	}
	if strings.ContainsAny(ref, "*?") {
		for i := range d.Sections {
			if s := &d.Sections[i]; s.Kind != KindRoot && globMatch(ref, s.PathSlug) {
				secs = append(secs, s)
			}
		}
		if len(secs) == 0 {
			return nil, false, notFound(ref)
		}
		return secs, false, nil
	}
	if numberRe.MatchString(ref) {
		for i := range d.Sections {
			if d.Sections[i].Number == ref {
				return []*Section{&d.Sections[i]}, true, nil
			}
		}
		return nil, true, notFound(ref)
	}
	var cands []string
	for i := range d.Sections {
		if s := &d.Sections[i]; strings.HasSuffix(s.PathSlug, "__"+ref) {
			secs = append(secs, s)
			cands = append(cands, s.PathSlug)
		}
	}
	switch len(secs) {
	case 0:
		return nil, false, notFound(ref)
	case 1:
		return secs, false, nil
	}
	return nil, false, &errs.Error{Code: errs.ExitError, Token: errs.TokAmbiguous, Entity: "section", Ref: ref,
		Message: "ambiguous section reference " + ref + ": " + strings.Join(cands, ", "),
		Extra:   map[string]any{"candidates": cands}}
}

func notFound(ref string) *errs.Error {
	return errs.NotFound("section", ref, "no such section: %s", ref)
}

// globMatch matches s against a pattern where `*` is any run of bytes
// (including `__`) and `?` is one character.
func globMatch(pat, s string) bool {
	pr, sr := []rune(pat), []rune(s)
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(sr) {
		switch {
		case pi < len(pr) && (pr[pi] == '?' || pr[pi] == sr[si]):
			pi++
			si++
		case pi < len(pr) && pr[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pr) && pr[pi] == '*' {
		pi++
	}
	return pi == len(pr)
}
