// Package row is the ordered key/value record every layer passes around.
//
// The Python build used dicts, whose insertion order leaks into json/yaml
// output and table columns. Row keeps that order so output is reproducible.
//
// Values are restricted to: nil, bool, int64, float64, string, []any,
// []string, *Row, []*Row. Ints from SQLite arrive as int64; use Int() to read.
package row

import "database/sql"

// Row is an insertion-ordered map. The zero value is not usable; use New.
type Row struct {
	keys []string
	vals map[string]any
}

// New returns an empty row.
func New() *Row { return &Row{vals: map[string]any{}} }

// Of builds a row from alternating key, value arguments.
func Of(kv ...any) *Row {
	r := New()
	for i := 0; i+1 < len(kv); i += 2 {
		r.Set(kv[i].(string), kv[i+1])
	}
	return r
}

// Set assigns a value, appending the key if it is new (order is kept on
// overwrite, as a Python dict does).
func (r *Row) Set(k string, v any) *Row {
	if _, ok := r.vals[k]; !ok {
		r.keys = append(r.keys, k)
	}
	r.vals[k] = normalize(v)
	return r
}

// Get returns the value for k (nil if absent).
func (r *Row) Get(k string) any { return r.vals[k] }

// Has reports whether k is present.
func (r *Row) Has(k string) bool { _, ok := r.vals[k]; return ok }

// Delete removes k, like dict.pop.
func (r *Row) Delete(k string) {
	if _, ok := r.vals[k]; !ok {
		return
	}
	delete(r.vals, k)
	for i, key := range r.keys {
		if key == k {
			r.keys = append(r.keys[:i:i], r.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in insertion order. Do not mutate.
func (r *Row) Keys() []string { return r.keys }

// Len is the number of keys.
func (r *Row) Len() int { return len(r.keys) }

// Copy is a shallow copy (dict(row)).
func (r *Row) Copy() *Row {
	c := New()
	for _, k := range r.keys {
		c.Set(k, r.vals[k])
	}
	return c
}

// Str returns the value as a string ("" for nil or non-strings).
func (r *Row) Str(k string) string {
	s, _ := r.vals[k].(string)
	return s
}

// Int returns the value as an int64 (0 for nil; bools map to 0/1).
func (r *Row) Int(k string) int64 {
	switch v := r.vals[k].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

// IsNull reports whether the key is absent or holds nil.
func (r *Row) IsNull(k string) bool { return r.vals[k] == nil }

func normalize(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case uint:
		return int64(x)
	case []byte:
		return string(x)
	case sql.NullString:
		if x.Valid {
			return x.String
		}
		return nil
	case sql.NullInt64:
		if x.Valid {
			return x.Int64
		}
		return nil
	case *string:
		if x == nil {
			return nil
		}
		return *x
	case *int64:
		if x == nil {
			return nil
		}
		return *x
	}
	return v
}

// Scan reads every row of rs into Rows, keyed by column name, in column order.
func Scan(rs *sql.Rows) ([]*Row, error) {
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []*Row
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := New()
		for i, c := range cols {
			r.Set(c, vals[i])
		}
		out = append(out, r)
	}
	return out, rs.Err()
}
