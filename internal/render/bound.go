package render

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/row"
)

// Bound implements --max-bytes (design doc §4.2, spec §6.1): keep whole rows
// while the rendered output stays within maxBytes, never cut a string. It
// returns the rows that fit and, if anything was omitted, a manifest row
// {"omitted", "returned", "total", "bytes_limit", "retrieve"} so the
// omission is never silent. If even the first row alone exceeds the limit,
// it returns zero rows and a manifest naming that row's size.
//
// The per-row size is computed against the target format's own encoding
// (compact JSON for json/jsonl, a tab-joined cell line otherwise) so the
// count reflects what will actually be written.
func Bound(rows []*row.Row, maxBytes int, format string, fields []string, restrict bool) ([]*row.Row, *row.Row) {
	if maxBytes <= 0 || len(rows) == 0 {
		return rows, nil
	}
	total := len(rows)
	var projFields []string
	if restrict {
		projFields = fields
	}

	sizes := make([]int, total)
	for i, r := range rows {
		sizes[i] = rowBytes(r, fields, projFields, format)
	}

	overhead := 0
	if format == "json" {
		overhead = len("[]")
	}
	used := overhead
	kept := 0
	for i := 0; i < total; i++ {
		add := sizes[i]
		if format == "json" && i > 0 {
			add++ // comma
		}
		if used+add > maxBytes {
			break
		}
		used += add
		kept++
	}

	if kept == total {
		return rows, nil
	}
	if kept == 0 {
		manifest := row.Of(
			"omitted", int64(total),
			"returned", int64(0),
			"total", int64(total),
			"bytes_limit", int64(maxBytes),
			"retrieve", fmt.Sprintf("first row is %d bytes, exceeds --max-bytes %d; raise --max-bytes or narrow --fields", sizes[0], maxBytes),
		)
		return nil, manifest
	}
	manifest := row.Of(
		"omitted", int64(total-kept),
		"returned", int64(kept),
		"total", int64(total),
		"bytes_limit", int64(maxBytes),
		"retrieve", fmt.Sprintf("re-run without --max-bytes, or narrow it (-n, --fields, filters), to see the remaining %d row(s)", total-kept),
	)
	return rows[:kept], manifest
}

func rowBytes(r *row.Row, fields, projFields []string, format string) int {
	switch format {
	case "json":
		obj := ProjectRows([]*row.Row{r}, projFields)[0]
		return len(JSON(obj)) - 1 // JSON() adds a trailing newline the array layout doesn't need per-element
	case "jsonl":
		obj := ProjectRows([]*row.Row{r}, projFields)[0]
		return len(JSONCompact(obj)) + 1 // + newline
	default: // table, plain, tsv, yaml
		cells := make([]string, len(fields))
		for i, f := range fields {
			cells[i] = Cell(r.Get(f))
		}
		return len(strings.Join(cells, "\t")) + 1
	}
}
