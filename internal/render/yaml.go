package render

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/l4sh/agtrack/internal/row"
)

// plainSafe is render.py's _PLAIN_SAFE set: characters that never force a
// YAML scalar to be quoted.
const plainSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 _-./:@#+"

// ToYAML is an exact port of render.py's to_yaml/_yaml_scalar, including its
// quirks (e.g. an empty dict/list nested as a list item renders as a quoted
// "{}" / "[]" string, because Python's _yaml_scalar falls through to
// json.dumps(str(value)) for it). Per the parity contract this output is not
// byte-compatible with Python's YAML in general, but is a faithful port of
// the same emitter.
func ToYAML(obj any) string {
	var b strings.Builder
	writeYAML(&b, obj, 0)
	return b.String()
}

func writeYAML(b *strings.Builder, obj any, indent int) {
	pad := strings.Repeat(" ", indent)
	switch v := obj.(type) {
	case *row.Row:
		writeYAMLRow(b, v, indent, pad)
	case map[string]any:
		writeYAMLMap(b, v, indent, pad)
	case []any:
		writeYAMLList(b, v, indent, pad)
	case []string:
		arr := make([]any, len(v))
		for i, s := range v {
			arr[i] = s
		}
		writeYAMLList(b, arr, indent, pad)
	case []*row.Row:
		arr := make([]any, len(v))
		for i, r := range v {
			arr[i] = r
		}
		writeYAMLList(b, arr, indent, pad)
	default:
		b.WriteString(pad + yamlScalar(v) + "\n")
	}
}

// isEmptyContainer reports whether v is a dict-like or list-like container,
// and whether it is empty.
func isEmptyContainer(v any) (empty, isContainer, isDict bool) {
	switch x := v.(type) {
	case *row.Row:
		return x == nil || x.Len() == 0, true, true
	case map[string]any:
		return len(x) == 0, true, true
	case []any:
		return len(x) == 0, true, false
	case []string:
		return len(x) == 0, true, false
	case []*row.Row:
		return len(x) == 0, true, false
	}
	return false, false, false
}

func writeYAMLRow(b *strings.Builder, r *row.Row, indent int, pad string) {
	if r == nil || r.Len() == 0 {
		b.WriteString(pad + "{}\n")
		return
	}
	for _, k := range r.Keys() {
		writeYAMLEntry(b, k, r.Get(k), indent, pad)
	}
}

func writeYAMLMap(b *strings.Builder, m map[string]any, indent int, pad string) {
	if len(m) == 0 {
		b.WriteString(pad + "{}\n")
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeYAMLEntry(b, k, m[k], indent, pad)
	}
}

func writeYAMLEntry(b *strings.Builder, key string, val any, indent int, pad string) {
	empty, isContainer, isDict := isEmptyContainer(val)
	switch {
	case isContainer && !empty:
		b.WriteString(pad + yamlScalar(key) + ":\n")
		writeYAML(b, val, indent+2)
	case isContainer && empty:
		marker := "[]"
		if isDict {
			marker = "{}"
		}
		b.WriteString(pad + yamlScalar(key) + ": " + marker + "\n")
	default:
		b.WriteString(pad + yamlScalar(key) + ": " + yamlScalar(val) + "\n")
	}
}

func writeYAMLList(b *strings.Builder, items []any, indent int, pad string) {
	if len(items) == 0 {
		b.WriteString(pad + "[]\n")
		return
	}
	for _, item := range items {
		empty, isContainer, _ := isEmptyContainer(item)
		if isContainer && !empty {
			var nested strings.Builder
			writeYAML(&nested, item, indent+2)
			block := nested.String()
			b.WriteString(pad + "- " + block[indent+2:])
		} else {
			b.WriteString(pad + "- " + yamlScalar(item) + "\n")
		}
	}
}

// yamlScalar ports _yaml_scalar. It only ever receives a container value for
// the (empty-container-as-list-item) case described above; any other
// container reaching here is a defensive fallback, not a real Python path.
func yamlScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return pyFloatRepr(x)
	case string:
		return yamlQuoteIfUnsafe(x)
	case *row.Row:
		if x == nil || x.Len() == 0 {
			return yamlQuoteIfUnsafe("{}")
		}
	case []any:
		if len(x) == 0 {
			return yamlQuoteIfUnsafe("[]")
		}
	case []string:
		if len(x) == 0 {
			return yamlQuoteIfUnsafe("[]")
		}
	case []*row.Row:
		if len(x) == 0 {
			return yamlQuoteIfUnsafe("[]")
		}
	}
	return yamlQuoteIfUnsafe(pyStr(v))
}

var yamlRetypes = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)?(?:\.[0-9_]*)?(?:[eE][-+]?[0-9]+)?|0x[0-9a-fA-F_]+|0o?[0-7_]+|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN)|[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}(?:[Tt ].*)?|[0-9]+(?::[0-5]?[0-9])+(?:\.[0-9]*)?)$`)

func yamlQuoteIfUnsafe(text string) string {
	unsafe := text == ""
	if !unsafe {
		for _, c := range text {
			if !strings.ContainsRune(plainSafe, c) {
				unsafe = true
				break
			}
		}
	}
	if !unsafe {
		unsafe = strings.TrimSpace(text) != text
	}
	if !unsafe && len(text) > 0 {
		unsafe = strings.IndexByte("-?:,[]{}#&*!|>'\"%@`", text[0]) >= 0
	}
	if !unsafe {
		unsafe = strings.Contains(text, ": ")
	}
	if !unsafe {
		unsafe = strings.HasSuffix(text, ":")
	}
	if !unsafe {
		switch strings.ToLower(text) {
		case "true", "false", "null", "yes", "no", "on", "off", "~", "y", "n":
			unsafe = true
		}
	}
	// Beyond the 1.x emitter: a plain scalar must read back as the same
	// string. " #" starts a comment; numbers, dates and timestamps re-type.
	if !unsafe {
		unsafe = strings.Contains(text, " #") || yamlRetypes.MatchString(text)
	}
	if unsafe {
		return jsonQuoteString(text)
	}
	return text
}
