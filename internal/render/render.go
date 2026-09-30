// Package render is the output layer: nothing here touches the database.
//
// Five Python-era formats plus jsonl (D12): table (aligned columns, the
// terminal default), plain and tsv (terse, script-friendly), json (pretty
// printed, byte-identical to the Python build), yaml (hand-rolled emitter,
// not byte-compatible but a faithful port), and jsonl (one compact object
// per line, streamable, new in the Go build).
package render

import (
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
)

// Formats lists the accepted -f/--format values, in the order documented.
var Formats = []string{"table", "plain", "tsv", "json", "jsonl", "yaml"}

const (
	boldCode  = "\033[1m"
	resetCode = "\033[0m"
	dimCode   = "\033[90m"
)

// statusColors ports render.py's _COLORS.
var statusColors = map[string]string{
	"todo":        "\033[0m",
	"in-progress": "\033[33m",
	"done":        "\033[32m",
	"cancelled":   "\033[90m",
}

// Options configures Emit.
type Options struct {
	Format   string
	Header   bool
	Color    *bool // nil = auto-detect on w
	Restrict bool  // json/yaml/jsonl carry only `fields` (the caller passed --fields)
	MaxBytes int   // 0 = unbounded
}

// Section is one label/value line for EmitDetail. Value is a string, or a
// []string rendered as an indented block.
type Section struct {
	Label string
	Value any
}

// ColorEnabled ports render.py's color_enabled: false under NO_COLOR or
// AGTRACK_NO_COLOR, else whether w is a terminal.
func ColorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("AGTRACK_NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Cell ports render.py's _cell: the text a single table/plain/tsv cell
// renders as for a given value.
func Cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "yes"
		}
		return ""
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyStr(e)
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(x, ",")
	case []*row.Row:
		parts := make([]string, len(x))
		for i, r := range x {
			parts[i] = pyStr(r)
		}
		return strings.Join(parts, ",")
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return pyFloatRepr(x)
	case string:
		return x
	default:
		return pyStr(x)
	}
}

// SelectFields ports render.py's select_fields: validates a user-supplied
// --fields list against the rows on hand.
func SelectFields(rows []*row.Row, fields []string) ([]string, error) {
	if len(rows) == 0 {
		return append([]string(nil), fields...), nil
	}
	available := make(map[string]bool, rows[0].Len())
	for _, k := range rows[0].Keys() {
		available[k] = true
	}
	var unknown []string
	for _, f := range fields {
		if !available[f] {
			unknown = append(unknown, f)
		}
	}
	if len(unknown) > 0 {
		avail := make([]string, 0, len(available))
		for k := range available {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, errs.New("unknown field(s): %s; available: %s",
			strings.Join(unknown, ", "), strings.Join(avail, ", "))
	}
	return append([]string(nil), fields...), nil
}

// ProjectRows ports render.py's project_rows: with no fields, a full shallow
// copy of every row; with fields, a row holding exactly those keys (missing
// ones read as nil, matching dict.get).
func ProjectRows(rows []*row.Row, fields []string) []*row.Row {
	out := make([]*row.Row, len(rows))
	if len(fields) == 0 {
		for i, r := range rows {
			out[i] = r.Copy()
		}
		return out
	}
	for i, r := range rows {
		nr := row.New()
		for _, f := range fields {
			nr.Set(f, r.Get(f))
		}
		out[i] = nr
	}
	return out
}

// Emit ports render.py's emit, plus the jsonl format and --max-bytes
// bounding. For json/yaml/jsonl, MaxBytes reshapes the payload itself (a
// wrapper object for json/yaml, a trailing manifest line for jsonl) since the
// manifest must live in the same stream as the data. For table/plain/tsv the
// manifest is documented to go to stderr, which Emit — writing to a single
// w — does not own: it only trims the printed rows via Bound. The caller
// owns printing that manifest (call Bound itself to get it).
func Emit(w io.Writer, rows []*row.Row, fields []string, o Options) error {
	switch o.Format {
	case "json", "yaml", "jsonl":
		workRows := rows
		var manifest *row.Row
		if o.MaxBytes > 0 {
			workRows, manifest = Bound(rows, o.MaxBytes, o.Format, fields, o.Restrict)
		}
		var projFields []string
		if o.Restrict {
			projFields = fields
		}
		data := ProjectRows(workRows, projFields)
		return emitProjected(w, data, manifest, o.Format)
	case "table", "plain", "tsv":
		workRows := rows
		if o.MaxBytes > 0 {
			workRows, _ = Bound(rows, o.MaxBytes, o.Format, fields, o.Restrict)
		}
		return emitTabular(w, workRows, fields, o)
	default:
		return errs.Usage("unknown format: %s", o.Format)
	}
}

func emitProjected(w io.Writer, data []*row.Row, manifest *row.Row, format string) error {
	switch format {
	case "jsonl":
		if err := emitJSONL(w, data); err != nil {
			return err
		}
		if manifest != nil {
			wrapper := row.Of("_omitted", manifest)
			if _, err := w.Write(JSONCompact(wrapper)); err != nil {
				return err
			}
			_, err := io.WriteString(w, "\n")
			return err
		}
		return nil
	case "json":
		if manifest != nil {
			wrapper := row.Of("items", data, "omitted", manifest)
			_, err := w.Write(JSON(wrapper))
			return err
		}
		_, err := w.Write(JSON(data))
		return err
	case "yaml":
		if manifest != nil {
			wrapper := row.Of("items", data, "omitted", manifest)
			_, err := io.WriteString(w, ToYAML(wrapper))
			return err
		}
		_, err := io.WriteString(w, ToYAML(data))
		return err
	}
	return nil
}

func emitTabular(w io.Writer, rows []*row.Row, fields []string, o Options) error {
	if len(rows) == 0 {
		return nil
	}
	table := make([][]string, len(rows))
	for i, r := range rows {
		line := make([]string, len(fields))
		for j, f := range fields {
			line[j] = Cell(r.Get(f))
		}
		table[i] = line
	}

	useColor := false
	if o.Color != nil {
		useColor = *o.Color
	} else {
		useColor = ColorEnabled(w)
	}

	switch o.Format {
	case "tsv":
		if o.Header {
			hdr := make([]string, len(fields))
			for i, f := range fields {
				hdr[i] = strings.ToUpper(f)
			}
			if _, err := io.WriteString(w, strings.Join(hdr, "\t")+"\n"); err != nil {
				return err
			}
		}
		for _, line := range table {
			cells := make([]string, len(line))
			for i, c := range line {
				cells[i] = strings.ReplaceAll(c, "\t", " ")
			}
			if _, err := io.WriteString(w, strings.Join(cells, "\t")+"\n"); err != nil {
				return err
			}
		}
		return nil
	case "plain":
		for _, line := range table {
			var parts []string
			for _, c := range line {
				if c != "" {
					parts = append(parts, c)
				}
			}
			if _, err := io.WriteString(w, strings.Join(parts, " ")+"\n"); err != nil {
				return err
			}
		}
		return nil
	}

	// table
	widths := make([]int, len(fields))
	for i, f := range fields {
		widths[i] = utf8.RuneCountInString(f)
	}
	for _, line := range table {
		for i, c := range line {
			if n := utf8.RuneCountInString(c); n > widths[i] {
				widths[i] = n
			}
		}
	}
	if o.Header {
		parts := make([]string, len(fields))
		for i, f := range fields {
			parts[i] = ljust(strings.ToUpper(f), widths[i])
		}
		head := strings.Join(parts, "  ")
		if useColor {
			head = boldCode + head + resetCode
		}
		if _, err := io.WriteString(w, rstrip(head)+"\n"); err != nil {
			return err
		}
	}
	for idx, line := range table {
		parts := make([]string, len(fields))
		for i, c := range line {
			if i < len(fields)-1 {
				parts[i] = ljust(c, widths[i])
			} else {
				parts[i] = c
			}
		}
		text := rstrip(strings.Join(parts, "  "))
		if useColor {
			if code, ok := statusColors[rows[idx].Str("status")]; ok {
				text = code + text + resetCode
			}
		}
		if _, err := io.WriteString(w, text+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// EmitDetail ports render.py's emit_detail: skips nil/empty-string/empty-list
// values, dims the label when colour is on, and renders a []string value as
// an indented block.
func EmitDetail(w io.Writer, sections []Section, color *bool) error {
	useColor := false
	if color != nil {
		useColor = *color
	} else {
		useColor = ColorEnabled(w)
	}
	for _, s := range sections {
		empty := false
		switch v := s.Value.(type) {
		case nil:
			empty = true
		case string:
			empty = v == ""
		case []string:
			empty = len(v) == 0
		}
		if empty {
			continue
		}
		key := s.Label + ":"
		if useColor {
			key = dimCode + s.Label + ":" + resetCode
		}
		if v, ok := s.Value.([]string); ok {
			if _, err := io.WriteString(w, key+"\n"); err != nil {
				return err
			}
			for _, line := range v {
				if _, err := io.WriteString(w, "  "+line+"\n"); err != nil {
					return err
				}
			}
			continue
		}
		valStr, ok := s.Value.(string)
		if !ok {
			valStr = pyStr(s.Value)
		}
		if _, err := io.WriteString(w, key+" "+valStr+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func ljust(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

func rstrip(s string) string {
	return strings.TrimRightFunc(s, unicode.IsSpace)
}
