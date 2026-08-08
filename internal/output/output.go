// Package output renders Graph API responses.
//
// JSON is the default because the tool is built to be scripted against and
// called by language models. The other formats exist for the cases where JSON
// is the wrong shape: ndjson to pipe a paginated sweep line by line, table to
// read a result at a terminal, csv to paste into a spreadsheet.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Format is a rendering mode.
type Format string

const (
	JSON   Format = "json"
	NDJSON Format = "ndjson"
	Table  Format = "table"
	CSV    Format = "csv"
)

// Formats lists every supported format, for help text and completion.
func Formats() []string { return []string{"json", "ndjson", "table", "csv"} }

// Parse validates a format name.
func Parse(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case JSON:
		return JSON, nil
	case NDJSON:
		return NDJSON, nil
	case Table:
		return Table, nil
	case CSV:
		return CSV, nil
	}
	return "", fmt.Errorf("unknown output format %q (want one of: %s)", s, strings.Join(Formats(), ", "))
}

// Options tune rendering.
type Options struct {
	Pretty  bool     // indent JSON
	Columns []string // explicit column order for table/csv
}

// Write renders v in the requested format.
func Write(w io.Writer, f Format, v any, opts Options) error {
	switch f {
	case JSON:
		return writeJSON(w, v, opts.Pretty)
	case NDJSON:
		return writeNDJSON(w, v)
	case Table:
		return writeTable(w, v, opts.Columns)
	case CSV:
		return writeCSV(w, v, opts.Columns)
	}
	return fmt.Errorf("unknown output format %q", f)
}

func writeJSON(w io.Writer, v any, pretty bool) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

func writeNDJSON(w io.Writer, v any) error {
	rows, ok := records(v)
	if !ok {
		return writeJSON(w, v, false)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func writeTable(w io.Writer, v any, columns []string) error {
	rows, ok := records(v)
	if !ok {
		// A single object reads better as aligned key/value pairs than as a
		// one-row table with forty columns.
		obj, isObj := v.(map[string]any)
		if !isObj {
			return writeJSON(w, v, true)
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, k := range sortedKeys(obj) {
			fmt.Fprintf(tw, "%s\t%s\n", k, cell(obj[k]))
		}
		return tw.Flush()
	}
	cols := resolveColumns(rows, columns)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(upperAll(cols), "\t"))
	for _, r := range rows {
		values := make([]string, len(cols))
		for i, c := range cols {
			values[i] = cell(r[c])
		}
		fmt.Fprintln(tw, strings.Join(values, "\t"))
	}
	return tw.Flush()
}

func writeCSV(w io.Writer, v any, columns []string) error {
	rows, ok := records(v)
	if !ok {
		obj, isObj := v.(map[string]any)
		if !isObj {
			return writeJSON(w, v, false)
		}
		rows = []map[string]any{obj}
	}
	cols := resolveColumns(rows, columns)
	cw := csv.NewWriter(w)
	if err := cw.Write(cols); err != nil {
		return err
	}
	for _, r := range rows {
		values := make([]string, len(cols))
		for i, c := range cols {
			values[i] = cell(r[c])
		}
		if err := cw.Write(values); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// records pulls the row set out of a response: either a bare array, or the
// "data" array of a Graph list response.
func records(v any) ([]map[string]any, bool) {
	switch t := v.(type) {
	case []map[string]any:
		return t, true
	case []any:
		return toRows(t), true
	case map[string]any:
		if data, ok := t["data"]; ok {
			if arr, isArr := data.([]any); isArr {
				return toRows(arr), true
			}
		}
	}
	return nil, false
}

func toRows(items []any) []map[string]any {
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			rows = append(rows, m)
			continue
		}
		rows = append(rows, map[string]any{"value": item})
	}
	return rows
}

// resolveColumns honours an explicit order when given, otherwise takes the
// union of keys in order of first appearance so the most common fields --
// which the API returns first -- stay leftmost.
func resolveColumns(rows []map[string]any, explicit []string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	seen := map[string]bool{}
	var cols []string
	for _, r := range rows {
		for _, k := range sortedKeys(r) {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	// "id" and "name" are the columns anybody scanning a table looks for.
	sort.SliceStable(cols, func(i, j int) bool {
		return rank(cols[i]) < rank(cols[j])
	})
	return cols
}

func rank(col string) int {
	switch col {
	case "id":
		return 0
	case "name":
		return 1
	}
	return 2
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func upperAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

// cell flattens a JSON value into something that fits in one table cell.
func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
