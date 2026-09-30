package render

import "github.com/l4sh/agtrack/internal/row"

// Lean deep-copies obj, dropping the named keys at every *row.Row level
// (D12): the derived duplicates (created, updated, mark, path, context, ...)
// that json/yaml output should not carry.
func Lean(obj any, drop ...string) any {
	dropSet := make(map[string]bool, len(drop))
	for _, k := range drop {
		dropSet[k] = true
	}
	return leanValue(obj, dropSet)
}

func leanValue(v any, drop map[string]bool) any {
	switch x := v.(type) {
	case *row.Row:
		if x == nil {
			return x
		}
		nr := row.New()
		for _, k := range x.Keys() {
			if drop[k] {
				continue
			}
			nr.Set(k, leanValue(x.Get(k), drop))
		}
		return nr
	case []*row.Row:
		out := make([]*row.Row, len(x))
		for i, r := range x {
			out[i], _ = leanValue(r, drop).(*row.Row)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = leanValue(e, drop)
		}
		return out
	case []string:
		out := make([]string, len(x))
		copy(out, x)
		return out
	default:
		return v
	}
}
