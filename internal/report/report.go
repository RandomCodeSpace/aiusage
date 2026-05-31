// Package report renders store.Summary results as an aligned text table and
// provides machine-readable exports (JSON/CSV). It is read-only over the data:
// it only formats values already produced by the store.
package report

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"aiusage/internal/store"
)

// Opt controls how RenderTable renders a summary.
type Opt struct {
	// Breakdown is a presentation hint reserved for the CLI (which controls the
	// grouping dimensions via Filter.GroupBy). RenderTable adds no extra columns
	// for it; the flag is kept so callers can thread it through uniformly.
	Breakdown bool
	// Color enables lipgloss styling (headers/totals). When false the output is
	// plain ASCII suitable for pipes and tests.
	Color bool
	// Width is an optional target terminal width. Zero means unconstrained.
	Width int
}

// Fixed metric column headers appended after the grouping-key columns.
const (
	colEvents = "Events"
	colInput  = "Input"
	colOutput = "Output"
	colCache  = "Cache"
	colTotal  = "Total"
)

const totalsLabel = "TOTAL"

// RenderTable renders a summary as an aligned text table. Columns are the
// grouping-key dimensions (in OrderedKeys order) followed by Events, Input,
// Output, Cache (= CacheCreation + CacheRead) and Total. Numeric columns are
// right-aligned and humanised (e.g. 2.0M, 912.3K). A TOTAL row is always
// appended.
func RenderTable(sum *store.Summary, opt Opt) string {
	if sum == nil {
		return ""
	}

	keyCols := keyColumns(sum)
	headers := append(append([]string{}, keyCols...), colEvents, colInput, colOutput, colCache, colTotal)

	// Build the data rows as raw strings (already humanised for metrics).
	rows := make([][]string, 0, len(sum.Buckets)+1)
	for _, b := range sum.Buckets {
		rows = append(rows, bucketRow(b, keyCols))
	}
	totalsRow := bucketRow(sum.Totals, keyCols)
	// The totals row has no key values; label its first cell.
	if len(keyCols) > 0 {
		totalsRow[0] = totalsLabel
	} else {
		// No grouping columns: prepend a label column so the TOTAL row is
		// distinguishable and the header still aligns.
		headers = append([]string{""}, headers...)
		for i, r := range rows {
			rows[i] = append([]string{""}, r...)
		}
		totalsRow = append([]string{totalsLabel}, totalsRow...)
	}

	// Determine the index after which columns are numeric (right-aligned).
	// Numeric columns are always the final 5 (Events, Input, Output, Cache,
	// Total). Everything before them is a label column (left-aligned).
	numericFrom := len(headers) - 5

	widths := columnWidths(headers, rows, totalsRow)

	var sb strings.Builder
	sb.WriteString(renderRow(headers, widths, numericFrom, opt, styleHeader))
	sb.WriteByte('\n')
	sb.WriteString(separator(widths))
	sb.WriteByte('\n')
	for _, r := range rows {
		sb.WriteString(renderRow(r, widths, numericFrom, opt, styleNone))
		sb.WriteByte('\n')
	}
	sb.WriteString(separator(widths))
	sb.WriteByte('\n')
	sb.WriteString(renderRow(totalsRow, widths, numericFrom, opt, styleTotal))

	return sb.String()
}

// keyColumns returns the grouping dimension column names, preferring the order
// declared on the summary, then on the first bucket.
func keyColumns(sum *store.Summary) []string {
	if len(sum.GroupBy) > 0 {
		return append([]string{}, sum.GroupBy...)
	}
	if len(sum.Buckets) > 0 && len(sum.Buckets[0].OrderedKeys) > 0 {
		return append([]string{}, sum.Buckets[0].OrderedKeys...)
	}
	return nil
}

// bucketRow renders one bucket into ordered string cells: key values then the
// humanised metric columns.
func bucketRow(b store.Bucket, keyCols []string) []string {
	row := make([]string, 0, len(keyCols)+5)
	for _, k := range keyCols {
		row = append(row, b.Keys[k])
	}
	cache := b.CacheCreation + b.CacheRead
	row = append(row,
		humanize(b.Events),
		humanize(b.Input),
		humanize(b.Output),
		humanize(cache),
		humanize(b.Total),
	)
	return row
}

// columnWidths computes the max display width per column across header and all
// rows.
func columnWidths(headers []string, rows [][]string, totals []string) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	consider := func(r []string) {
		for i := 0; i < len(r) && i < len(widths); i++ {
			if l := len(r[i]); l > widths[i] {
				widths[i] = l
			}
		}
	}
	for _, r := range rows {
		consider(r)
	}
	consider(totals)
	return widths
}

type cellStyle int

const (
	styleNone cellStyle = iota
	styleHeader
	styleTotal
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	totalStyle  = lipgloss.NewStyle().Bold(true)
)

// renderRow formats a single row with padded, aligned cells joined by two
// spaces. Columns at index >= numericFrom are right-aligned; the rest are
// left-aligned.
func renderRow(cells []string, widths []int, numericFrom int, opt Opt, style cellStyle) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		w := widths[i]
		var padded string
		if i >= numericFrom {
			padded = padLeft(c, w)
		} else {
			padded = padRight(c, w)
		}
		parts[i] = padded
	}
	line := strings.Join(parts, "  ")
	if !opt.Color {
		return line
	}
	switch style {
	case styleHeader:
		return headerStyle.Render(line)
	case styleTotal:
		return totalStyle.Render(line)
	default:
		return line
	}
}

// separator builds a dashed rule sized to the column widths.
func separator(widths []int) string {
	parts := make([]string, len(widths))
	for i, w := range widths {
		parts[i] = strings.Repeat("-", w)
	}
	return strings.Join(parts, "  ")
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func padLeft(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

// humanize formats a token/event count compactly: values below 1000 are shown
// raw; larger values use a single-decimal SI-style suffix (K, M, G, T). The
// raw integer is the fallback for anything that does not fit a known suffix.
func humanize(n int64) string {
	if n < 0 {
		return "-" + humanize(-n)
	}
	const (
		k = 1000
		m = k * 1000
		g = m * 1000
		t = g * 1000
	)
	switch {
	case n < k:
		return strconv.FormatInt(n, 10)
	case n < m:
		return fmtUnit(float64(n)/k, "K")
	case n < g:
		return fmtUnit(float64(n)/m, "M")
	case n < t:
		return fmtUnit(float64(n)/g, "G")
	default:
		return fmtUnit(float64(n)/t, "T")
	}
}

func fmtUnit(v float64, suffix string) string {
	return fmt.Sprintf("%.1f%s", v, suffix)
}
