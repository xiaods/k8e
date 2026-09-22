package rqlitecompat

import (
	"encoding/json"
	"fmt"
)

// row is a single result row addressed by column name. rqlite returns column
// names alongside values, so the layer never depends on positional ordering.
type row map[string]any

func (r StatementResult) row(i int) (row, error) {
	if i < 0 || i >= len(r.Values) {
		return nil, &Err{Message: fmt.Sprintf("result has %d rows, want row %d", len(r.Values), i)}
	}
	if len(r.Columns) != len(r.Values[i]) {
		return nil, &Err{Message: fmt.Sprintf("row %d has %d values for %d columns", i, len(r.Values[i]), len(r.Columns))}
	}
	out := make(row, len(r.Columns))
	for j, name := range r.Columns {
		out[name] = r.Values[i][j]
	}
	return out, nil
}

func (r StatementResult) rows() ([]row, error) {
	out := make([]row, 0, len(r.Values))
	for i := range r.Values {
		rr, err := r.row(i)
		if err != nil {
			return nil, err
		}
		out = append(out, rr)
	}
	return out, nil
}

// blob decodes a BLOB cell. With blob_array rqlite echoes a BLOB as a JSON
// array of numbers; a TEXT cell arrives as a string and is returned as its
// raw UTF-8 bytes.
func (r row) blob(name string) []byte {
	v, ok := r[name]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]byte, 0, len(t))
		for _, x := range t {
			out = append(out, byte(int64Of(x)))
		}
		return out
	case string:
		return []byte(t)
	case json.Number:
		return []byte(string(t))
	default:
		return []byte(fmt.Sprint(t))
	}
}

// int decodes an INTEGER cell. A missing or NULL cell is 0.
func (r row) int(name string) int64 {
	v, ok := r[name]
	if !ok || v == nil {
		return 0
	}
	return int64Of(v)
}

// str decodes a TEXT cell.
func (r row) str(name string) string {
	v, ok := r[name]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// intOf converts a decoded JSON scalar to int64.
func int64Of(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		n, _ := t.Int64()
		return n
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return 0
	}
}
