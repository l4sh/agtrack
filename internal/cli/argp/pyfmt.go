package argp

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Python formatting and conversion helpers, so messages match argparse.

// pyRepr renders a string the way Python's repr() does.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// Python decodes such argv bytes with surrogateescape.
			b.WriteString(`\udc` + strconv.FormatUint(uint64(s[i])|0x100, 16)[1:])
		case r == '\\' || r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case !unicode.IsPrint(r) && r != ' ':
			switch {
			case r < 0x100:
				b.WriteString(`\x` + strconv.FormatUint(uint64(r)|0x100, 16)[1:])
			case r < 0x10000:
				b.WriteString(`\u` + strconv.FormatUint(uint64(r)|0x10000, 16)[1:])
			default:
				b.WriteString(`\U` + strconv.FormatUint(uint64(r)|0x100000000, 16)[1:])
			}
		default:
			b.WriteRune(r)
		}
		i += size
	}
	b.WriteByte(quote)
	return b.String()
}

// pyInt parses a base-10 integer the way Python's int() does: surrounding
// whitespace, a sign, and single underscores between digits are allowed.
// Values that do not fit an int are rejected.
func pyInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 || body == "" {
		return 0, false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '_' {
			if i == 0 || i == len(body)-1 || body[i+1] == '_' {
				return 0, false
			}
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
	if err != nil {
		return 0, false
	}
	return n, true
}

// negNumber is argparse's _negative_number_matcher: '-', optional '.', digit.
func negNumber(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	rest := s[1:]
	if rest[0] == '.' {
		rest = rest[1:]
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest != "" && unicode.IsDigit(r)
}
