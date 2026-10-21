// Package format renders query results as table, CSV, JSON or vertical text.
package format

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"sqlcli/internal/query"
)

// Write renders the result in the chosen format: auto | table | csv | tsv | json | vertical.
func Write(w io.Writer, r *query.Result, format string, showMeta bool) error {
	return WriteWidth(w, r, format, showMeta, 0)
}

// WriteWidth is Write with a terminal width: in "auto" format a table wider
// than maxWidth is rendered vertically instead (maxWidth <= 0 = unlimited).
func WriteWidth(w io.Writer, r *query.Result, format string, showMeta bool, maxWidth int) error {
	if r.Err != nil {
		fmt.Fprintf(w, "ERROR: %v\n", r.Err)
		return r.Err
	}
	if !r.IsQuery {
		if showMeta {
			fmt.Fprintf(w, "OK, %d row(s) affected (%s)\n", r.RowsAffected, r.Duration.Round(1e6))
		}
		return nil
	}
	switch strings.ToLower(format) {
	case "csv":
		return writeDelimited(w, r, ',')
	case "tsv":
		return writeDelimited(w, r, '\t')
	case "json":
		return writeJSON(w, r)
	case "vertical", "v":
		writeVertical(w, r)
	case "table", "t":
		writeTable(w, r)
	default:
		cells, widths := tableCells(r)
		if maxWidth > 0 && tableWidth(widths) > maxWidth {
			writeVertical(w, r)
		} else {
			printTable(w, r.Columns, cells, widths)
		}
	}
	if showMeta {
		fmt.Fprintf(w, "%d row(s) (%s)\n", len(r.Rows), r.Duration.Round(1e6))
	}
	return nil
}

func writeDelimited(w io.Writer, r *query.Result, sep rune) error {
	cw := csv.NewWriter(w)
	cw.Comma = sep
	if err := cw.Write(r.Columns); err != nil {
		return err
	}
	for _, row := range r.Rows {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func writeJSON(w io.Writer, r *query.Result) error {
	out := make([]map[string]any, 0, len(r.Rows))
	for _, row := range r.Rows {
		m := make(map[string]any, len(r.Columns))
		for i, c := range r.Columns {
			if row[i] == "NULL" {
				m[c] = nil
			} else {
				m[c] = row[i]
			}
		}
		out = append(out, m)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writeVertical(w io.Writer, r *query.Result) {
	width := 0
	for _, c := range r.Columns {
		if l := utf8.RuneCountInString(c); l > width {
			width = l
		}
	}
	for i, row := range r.Rows {
		fmt.Fprintf(w, "*************************** %d. row ***************************\n", i+1)
		for j, c := range r.Columns {
			fmt.Fprintf(w, "%*s: %s\n", width, c, row[j])
		}
	}
}

// writeTable prints a psql/mysql-style box table.
func writeTable(w io.Writer, r *query.Result) {
	cells, widths := tableCells(r)
	printTable(w, r.Columns, cells, widths)
}

// tableWidth is the rendered line width of a table with the given column widths.
func tableWidth(widths []int) int {
	n := 1
	for _, wd := range widths {
		n += wd + 3
	}
	return n
}

// tableCells sanitizes cell values and computes column widths.
func tableCells(r *query.Result) ([][]string, []int) {
	const maxCell = 200
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = displayWidth(c)
	}
	cells := make([][]string, len(r.Rows))
	for ri, row := range r.Rows {
		cells[ri] = make([]string, len(row))
		for i, v := range row {
			v = strings.ReplaceAll(strings.ReplaceAll(v, "\r\n", "\\n"), "\n", "\\n")
			if utf8.RuneCountInString(v) > maxCell {
				v = string([]rune(v)[:maxCell-1]) + "…"
			}
			cells[ri][i] = v
			if dw := displayWidth(v); dw > widths[i] {
				widths[i] = dw
			}
		}
	}
	return cells, widths
}

func printTable(w io.Writer, columns []string, cells [][]string, widths []int) {
	sep := func() {
		var b strings.Builder
		b.WriteString("+")
		for _, wd := range widths {
			b.WriteString(strings.Repeat("-", wd+2))
			b.WriteString("+")
		}
		fmt.Fprintln(w, b.String())
	}
	line := func(cols []string) {
		var b strings.Builder
		b.WriteString("|")
		for i, c := range cols {
			b.WriteString(" ")
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(c)))
			b.WriteString(" |")
		}
		fmt.Fprintln(w, b.String())
	}
	sep()
	line(columns)
	sep()
	for _, row := range cells {
		line(row)
	}
	sep()
}

// DisplayWidth approximates terminal cell width (CJK counted as 2).
func DisplayWidth(s string) int { return displayWidth(s) }

// displayWidth approximates terminal cell width (CJK counted as 2).
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
			(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) ||
			(r >= 0xfe30 && r <= 0xfe6f) || (r >= 0xff00 && r <= 0xff60) ||
			(r >= 0xffe0 && r <= 0xffe6) || (r >= 0x20000 && r <= 0x3fffd)):
			n += 2
		default:
			n++
		}
	}
	return n
}
