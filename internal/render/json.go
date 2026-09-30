package render

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
)

// jsonEncoder is a byte-for-byte port of Python's json.dump(obj, indent=2,
// default=str) (indent > 0) and json.dumps(obj, separators=(",", ":")) for
// JSONCompact (indent == 0): ensure_ascii=True escaping, Python float repr,
// and the same object/array layout.
type jsonEncoder struct {
	buf    bytes.Buffer
	indent int
}

func (e *jsonEncoder) writeIndent(level int) {
	if e.indent > 0 {
		e.buf.WriteByte('\n')
		e.buf.WriteString(strings.Repeat(" ", level*e.indent))
	}
}

func (e *jsonEncoder) keySep() string {
	if e.indent > 0 {
		return ": "
	}
	return ":"
}

func (e *jsonEncoder) encode(v any, level int) {
	switch x := v.(type) {
	case nil:
		e.buf.WriteString("null")
	case bool:
		if x {
			e.buf.WriteString("true")
		} else {
			e.buf.WriteString("false")
		}
	case int:
		e.buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		e.buf.WriteString(strconv.FormatInt(x, 10))
	case int32:
		e.buf.WriteString(strconv.FormatInt(int64(x), 10))
	case float64:
		e.buf.WriteString(pyFloatRepr(x))
	case float32:
		e.buf.WriteString(pyFloatRepr(float64(x)))
	case string:
		e.buf.WriteString(jsonQuoteString(x))
	case []string:
		arr := make([]any, len(x))
		for i, s := range x {
			arr[i] = s
		}
		e.encodeArray(arr, level)
	case []any:
		e.encodeArray(x, level)
	case *row.Row:
		e.encodeRow(x, level)
	case []*row.Row:
		arr := make([]any, len(x))
		for i, r := range x {
			arr[i] = r
		}
		e.encodeArray(arr, level)
	case map[string]any:
		e.encodeMap(x, level)
	default:
		// Python's default=str: anything else is stringified.
		e.buf.WriteString(jsonQuoteString(fmt.Sprint(x)))
	}
}

func (e *jsonEncoder) encodeArray(arr []any, level int) {
	if len(arr) == 0 {
		e.buf.WriteString("[]")
		return
	}
	e.buf.WriteByte('[')
	for i, item := range arr {
		e.writeIndent(level + 1)
		e.encode(item, level+1)
		if i != len(arr)-1 {
			e.buf.WriteByte(',')
		}
	}
	e.writeIndent(level)
	e.buf.WriteByte(']')
}

func (e *jsonEncoder) encodeRow(r *row.Row, level int) {
	if r == nil || r.Len() == 0 {
		e.buf.WriteString("{}")
		return
	}
	keys := r.Keys()
	e.buf.WriteByte('{')
	for i, k := range keys {
		e.writeIndent(level + 1)
		e.buf.WriteString(jsonQuoteString(k))
		e.buf.WriteString(e.keySep())
		e.encode(r.Get(k), level+1)
		if i != len(keys)-1 {
			e.buf.WriteByte(',')
		}
	}
	e.writeIndent(level)
	e.buf.WriteByte('}')
}

func (e *jsonEncoder) encodeMap(m map[string]any, level int) {
	if len(m) == 0 {
		e.buf.WriteString("{}")
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e.buf.WriteByte('{')
	for i, k := range keys {
		e.writeIndent(level + 1)
		e.buf.WriteString(jsonQuoteString(k))
		e.buf.WriteString(e.keySep())
		e.encode(m[k], level+1)
		if i != len(keys)-1 {
			e.buf.WriteByte(',')
		}
	}
	e.writeIndent(level)
	e.buf.WriteByte('}')
}

// jsonQuoteString ports Python's ensure_ascii=True string escaping: quote and
// backslash, the standard \b\f\n\r\t shorthands, every other control or
// non-ASCII code point as \u00XX, and astral code points as a UTF-16
// surrogate pair. '/' and '<>&' are left unescaped, matching Python.
func jsonQuoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7E:
				b.WriteRune(r)
			case r <= 0xFFFF:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				r -= 0x10000
				hi := 0xD800 + (r >> 10)
				lo := 0xDC00 + (r & 0x3FF)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// JSON is a byte-identical port of Python's json.dumps(obj, indent=2, default=str).
func JSON(obj any) []byte {
	e := &jsonEncoder{indent: 2}
	e.encode(obj, 0)
	e.buf.WriteByte('\n')
	return e.buf.Bytes()
}

// JSONCompact is a byte-identical port of
// json.dumps(obj, separators=(",", ":"), default=str): one line, no spaces.
func JSONCompact(obj any) []byte {
	e := &jsonEncoder{indent: 0}
	e.encode(obj, 0)
	return e.buf.Bytes()
}

// EmitObject dumps a single structured object (a `show` result, a dump, a
// summary) in json, yaml, or jsonl.
func EmitObject(w io.Writer, obj any, format string) error {
	switch format {
	case "yaml":
		_, err := io.WriteString(w, ToYAML(obj))
		return err
	case "jsonl":
		return emitJSONL(w, obj)
	case "json":
		_, err := w.Write(JSON(obj))
		return err
	default:
		return errs.Usage("unknown format: %s", format)
	}
}

// emitJSONL writes one compact JSON object per line for a list, or a single
// compact line for anything else.
func emitJSONL(w io.Writer, obj any) error {
	writeLine := func(v any) error {
		if _, err := w.Write(JSONCompact(v)); err != nil {
			return err
		}
		_, err := io.WriteString(w, "\n")
		return err
	}
	switch v := obj.(type) {
	case []any:
		for _, item := range v {
			if err := writeLine(item); err != nil {
				return err
			}
		}
		return nil
	case []*row.Row:
		for _, r := range v {
			if err := writeLine(r); err != nil {
				return err
			}
		}
		return nil
	case []string:
		for _, s := range v {
			if err := writeLine(s); err != nil {
				return err
			}
		}
		return nil
	default:
		return writeLine(obj)
	}
}
