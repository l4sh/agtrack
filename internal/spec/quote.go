package spec

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// QuoteState is the result of re-checking a note's quote against a body.
type QuoteState string

// Quote states (design §5.6).
const (
	QuoteOK        QuoteState = "ok"
	QuoteAmbiguous QuoteState = "ambiguous"
	QuoteMissing   QuoteState = "missing"
)

// LocateQuote finds quote in body. Whitespace is compared loosely — any run
// of whitespace matches any other — because prose is rewrapped constantly
// and a quote copied from a rendered view spans the source's line breaks.
// When quote occurs more than once, prefix (text just before) and suffix
// (text just after) narrow it down, compared the same way. It returns the
// byte offset of the match in body, or -1 unless the state is QuoteOK.
func LocateQuote(body, quote, prefix, suffix string) (QuoteState, int) {
	nb, pos := squash(body)
	nq, _ := squash(strings.TrimSpace(quote))
	if nq == "" {
		return QuoteMissing, -1
	}
	np, _ := squash(strings.TrimSpace(prefix))
	ns, _ := squash(strings.TrimSpace(suffix))

	var hits []int
	for from := 0; from <= len(nb); {
		i := strings.Index(nb[from:], nq)
		if i < 0 {
			break
		}
		hits = append(hits, from+i)
		_, w := utf8.DecodeRuneInString(nb[from+i:])
		from += i + max(w, 1)
	}
	switch len(hits) {
	case 0:
		return QuoteMissing, -1
	case 1:
		return QuoteOK, pos[hits[0]]
	}
	if np == "" && ns == "" {
		return QuoteAmbiguous, -1
	}
	var keep []int
	for _, h := range hits {
		before := strings.TrimRight(nb[:h], " ")
		after := strings.TrimLeft(nb[h+len(nq):], " ")
		if strings.HasSuffix(before, np) && strings.HasPrefix(after, ns) {
			keep = append(keep, h)
		}
	}
	if len(keep) == 1 {
		return QuoteOK, pos[keep[0]]
	}
	return QuoteAmbiguous, -1 // none or several fit the context
}

// squash collapses each whitespace run to one space and returns, for every
// byte of the result, the offset of the byte it came from.
func squash(s string) (string, []int) {
	var sb strings.Builder
	pos := make([]int, 0, len(s)+1)
	space := false
	for i, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				sb.WriteByte(' ')
				pos = append(pos, i)
			}
			space = true
			continue
		}
		space = false
		n := sb.Len()
		sb.WriteRune(r)
		for k := n; k < sb.Len(); k++ {
			pos = append(pos, i+k-n)
		}
	}
	pos = append(pos, len(s))
	return sb.String(), pos
}

// EscapeFTS turns free text into a literal-term FTS5 query: every
// whitespace-separated token becomes a double-quoted string (embedded quotes
// doubled), and the tokens are joined by spaces, which FTS5 reads as AND.
// No token can act as an operator, column filter or prefix query.
func EscapeFTS(q string) string {
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}
