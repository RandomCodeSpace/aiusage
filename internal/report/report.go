// Package report renders store.Summary results as an aligned text table and
// provides machine-readable exports (JSON/CSV). It is read-only over the data:
// it only formats values already produced by the store.
package report

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/model"
)

// Opt controls how RenderTable renders a summary.
type Opt struct {
	// Breakdown replaces the combined Cache column with the stored components
	// (Reasoning, CacheW, CacheR), and appends the reasoning legend explaining
	// which accounting rule each row was rendered under.
	Breakdown bool
	// Color enables lipgloss styling (headers/totals). When false the output is
	// plain ASCII suitable for pipes and tests.
	Color bool
	// Costs adds a trailing Cost column, aligned by index with Summary.Buckets
	// (see ResolveCosts). Nil renders the table exactly as before.
	Costs *Costs
}

// Fixed metric column headers appended after the grouping-key columns.
const (
	colEvents = "Events"
	colInput  = "Input"
	colOutput = "Output"
	colCache  = "Cache"
	colTotal  = "Total"
	colCost   = "Cost"
)

// Component columns rendered in place of the combined Cache column when
// Opt.Breakdown is set.
const (
	colReasoning = "Reasoning"
	colCacheW    = "CacheW"
	colCacheR    = "CacheR"
)

const totalsLabel = "TOTAL"

// RenderTable renders a summary as an aligned text table. Columns are the
// grouping-key dimensions (in OrderedKeys order) followed by Events, Input,
// Output, Cache (= CacheCreation + CacheRead) and Total — or, with
// Opt.Breakdown, the stored components in place of Cache. Numeric columns are
// right-aligned and humanised (e.g. 2.0M, 912.3K). A TOTAL row is always
// appended.
func RenderTable(sum *store.Summary, opt Opt) string {
	if sum == nil {
		return ""
	}

	keyCols := keyColumns(sum)
	metrics := metricHeaders(opt.Breakdown)
	headers := append(append([]string{}, keyCols...), metrics...)
	numeric := len(metrics)
	if opt.Costs != nil {
		headers = append(headers, colCost)
		numeric++
	}

	// Reasoning markers are collected while rendering so the legend lists only
	// the conventions the table actually used. Without a breakdown there is no
	// Reasoning column to mark, and therefore nothing to explain.
	marks := map[string]bool{}
	markOf := func(mark string) string {
		if !opt.Breakdown || mark == "" {
			return ""
		}
		marks[mark] = true
		return mark
	}

	// Build the data rows as raw strings (already humanised for metrics).
	rows := make([][]string, 0, len(sum.Buckets)+1)
	for i, b := range sum.Buckets {
		row := bucketRow(b, keyCols, opt.Breakdown, markOf(bucketMark(b)))
		if opt.Costs != nil {
			row = append(row, costCell(opt.Costs.Buckets, i))
		}
		rows = append(rows, row)
	}
	totalsRow := bucketRow(sum.Totals, keyCols, opt.Breakdown, markOf(totalsMark(sum)))
	if opt.Costs != nil {
		totalsRow = append(totalsRow, opt.Costs.Totals.String())
	}
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
	// Numeric columns are always the trailing metric ones (Events, Input,
	// Output, Cache, Total, plus Cost when present). Everything before them is
	// a label column (left-aligned).
	numericFrom := len(headers) - numeric

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

	if legend := breakdownLegend(marks); legend != "" {
		sb.WriteString("\n\n")
		sb.WriteString(legend)
	}

	return sb.String()
}

// metricHeaders returns the fixed metric columns: the combined Cache column by
// default, the stored components with Opt.Breakdown.
func metricHeaders(breakdown bool) []string {
	if breakdown {
		return []string{colEvents, colInput, colOutput, colReasoning, colCacheW, colCacheR, colTotal}
	}
	return []string{colEvents, colInput, colOutput, colCache, colTotal}
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

// unknownLabel is what the rendered table prints for a grouping value the
// ledger stores as the empty string. Provider is the only such dimension: an
// adapter whose source never names the billing provider leaves it empty, and a
// blank cell mid-table reads as a rendering fault rather than as the honest
// "not known". The stored value is never rewritten, and no machine surface
// substitutes this word for it: the CSV and JSON event exports emit the raw
// empty field (see eventRecord), and the JSON summary emits the raw value plus
// a separate provider_label carrying this string (see bucketJSON). A consumer
// that saw only the label could not tell a provider literally named "unknown"
// from a missing one.
const unknownLabel = "unknown"

// displayKey renders one grouping value for the rendered table, and feeds the
// JSON summary's provider_label. Every other dimension passes through
// untouched.
func displayKey(dim, val string) string {
	if dim == "provider" && val == "" {
		return unknownLabel
	}
	return val
}

// bucketRow renders one bucket into ordered string cells: key values then the
// humanised metric columns. mark is the reasoning marker for the row (empty
// outside a breakdown, or when the row reports no reasoning tokens).
func bucketRow(b store.Bucket, keyCols []string, breakdown bool, mark string) []string {
	row := make([]string, 0, len(keyCols)+7)
	for _, k := range keyCols {
		row = append(row, displayKey(k, b.Keys[k]))
	}
	if breakdown {
		return append(row,
			humanize(b.Events),
			humanize(b.Input),
			humanize(b.Output),
			humanize(b.Reasoning)+mark,
			humanize(b.CacheCreation),
			humanize(b.CacheRead),
			humanize(b.Total),
		)
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

// Reasoning markers. Schema v3 records reasoning per the accounting rule of the
// tool that produced the row (model.ReasoningModeFor): for some tools the count
// sits INSIDE Output, for others it is reported alongside it. A breakdown row
// therefore reconciles to its Total only under the rule that applies to it, so
// every non-zero Reasoning cell carries the marker for its rule and the legend
// spells the sum out. ASCII markers survive pipes and dumb terminals.
const (
	markSubset   = "*" // reasoning already counted inside Output
	markAdditive = "+" // reasoning counted alongside Output
	markUnknown  = "?" // rules differ (or the grouping cannot resolve them)
)

// bucketMark returns the reasoning marker for one bucket. The rule is a
// property of the tool, so it is resolvable only when the summary groups by
// tool; otherwise the bucket may mix both conventions and says so. A bucket
// with no reasoning tokens carries no marker — there is nothing to reconcile.
func bucketMark(b store.Bucket) string {
	if b.Reasoning == 0 {
		return ""
	}
	tool := b.Keys["tool"]
	if tool == "" {
		return markUnknown
	}
	return markForTool(tool)
}

// markForTool maps a tool id to its marker through the same rule the pricing
// engine bills by, so the column and the cost never disagree about a row.
func markForTool(tool string) string {
	if model.ReasoningModeFor(tool) == model.ReasoningAdditive {
		return markAdditive
	}
	return markSubset
}

// totalsMark reduces the per-bucket markers to one for the TOTAL row: a single
// convention shared by every contributing bucket carries through; anything else
// (a genuine mix, or a grouping that cannot resolve the rule) is unknown.
func totalsMark(sum *store.Summary) string {
	if sum.Totals.Reasoning == 0 {
		return ""
	}
	seen := ""
	for _, b := range sum.Buckets {
		mk := bucketMark(b)
		if mk == "" {
			continue
		}
		if seen == "" {
			seen = mk
			continue
		}
		if seen != mk {
			return markUnknown
		}
	}
	if seen == "" {
		return markUnknown
	}
	return seen
}

// breakdownLegend states, for each marker present, how that row's components
// reconcile to its Total.
func breakdownLegend(used map[string]bool) string {
	lines := make([]string, 0, 3)
	if used[markSubset] {
		lines = append(lines, markSubset+" reasoning is inside Output: Total = Input + Output + CacheW + CacheR")
	}
	if used[markAdditive] {
		lines = append(lines, markAdditive+" reasoning is alongside Output: Total = Input + Output + Reasoning + CacheW + CacheR")
	}
	if used[markUnknown] {
		lines = append(lines, markUnknown+" reasoning rules differ across the rows summed here: reconcile row by row (group by tool)")
	}
	return strings.Join(lines, "\n")
}

// costCell renders the cost for bucket i, or the unpriced marker when the
// resolved slice is shorter than the bucket list (a caller mismatch must not
// panic mid-render).
func costCell(costs []Cost, i int) string {
	if i < 0 || i >= len(costs) {
		return model.UnpricedMark
	}
	return costs[i].String()
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
