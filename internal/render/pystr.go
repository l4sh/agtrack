package render

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/l4sh/agtrack/internal/row"
)

// pyFloatRepr renders f exactly as Python's repr(float)/str(float) would.
// Python and Go both compute the shortest decimal that round-trips to the
// same float64; only the surface formatting (fixed vs. exponential, the
// trailing ".0", the two-digit signed exponent) differs, which is what this
// function bridges.
func pyFloatRepr(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	neg := f < 0
	af := math.Abs(f)
	sci := strconv.FormatFloat(af, 'e', -1, 64)
	mantissa, expPart, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expPart)
	digits := strings.Replace(mantissa, ".", "", 1)
	decpt := exp + 1

	var out string
	switch {
	case decpt <= -4 || decpt > 16:
		d0 := digits[:1]
		rest := digits[1:]
		m := d0
		if rest != "" {
			m += "." + rest
		}
		e := decpt - 1
		sign := "+"
		if e < 0 {
			sign = "-"
			e = -e
		}
		expStr := strconv.Itoa(e)
		if len(expStr) < 2 {
			expStr = "0" + expStr
		}
		out = fmt.Sprintf("%se%s%s", m, sign, expStr)
	case decpt <= 0:
		out = "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	default:
		out = digits[:decpt] + "." + digits[decpt:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

// pyStr mimics Python's str() over the value shapes render deals with: nil,
// bool, int/int64, float64, string, and the row/list container types.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return pyFloatRepr(x)
	case string:
		return x
	case []any:
		return pyListRepr(x)
	case []string:
		arr := make([]any, len(x))
		for i, s := range x {
			arr[i] = s
		}
		return pyListRepr(arr)
	case []*row.Row:
		arr := make([]any, len(x))
		for i, r := range x {
			arr[i] = r
		}
		return pyListRepr(arr)
	case *row.Row:
		return pyDictRepr(x)
	default:
		return fmt.Sprint(x)
	}
}

// pyRepr is pyStr's repr() counterpart, used only to render values nested
// inside a list/dict repr (Python's str(list) reprs its elements).
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyStrRepr(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return pyFloatRepr(x)
	case []any:
		return pyListRepr(x)
	case []string:
		arr := make([]any, len(x))
		for i, s := range x {
			arr[i] = s
		}
		return pyListRepr(arr)
	case []*row.Row:
		arr := make([]any, len(x))
		for i, r := range x {
			arr[i] = r
		}
		return pyListRepr(arr)
	case *row.Row:
		return pyDictRepr(x)
	default:
		return fmt.Sprint(x)
	}
}

func pyListRepr(items []any) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = pyRepr(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func pyDictRepr(r *row.Row) string {
	if r == nil {
		return "{}"
	}
	parts := make([]string, 0, r.Len())
	for _, k := range r.Keys() {
		parts = append(parts, pyStrRepr(k)+": "+pyRepr(r.Get(k)))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// pyStrRepr is a best-effort Python repr() of a string: single-quoted with
// minimal escaping. Only reached for the rare case of a dict/list nested
// inside a table cell or a YAML scalar.
func pyStrRepr(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}
