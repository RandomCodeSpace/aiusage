package views

import (
	"fmt"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/sparkline"
	"github.com/charmbracelet/x/ansi"

	"github.com/RandomCodeSpace/aiusage/store"
)

// Stable hit regions for the four workspace summary cells.
const (
	ZoneWorkspaceInput  = "workspace-summary-input"
	ZoneWorkspaceOutput = "workspace-summary-output"
	ZoneWorkspaceCache  = "workspace-summary-cache"
	ZoneWorkspaceCost   = "workspace-summary-cost"
)

const (
	workspaceWideW = 110
	workspaceWideH = 22
	workspaceGap   = 1
	workspaceFrame = 0 // pane hierarchy uses headings; the app owns the border
)

// WorkspaceLayout is the complete allocation for a workspace body. SummaryH,
// MachineH and BodyH are allocated heights. Pane dimensions include their
// headings; only the containing application frame adds a border.
type WorkspaceLayout struct {
	SummaryH int
	MachineH int
	BodyH    int

	TableW  int
	TableH  int
	DetailW int
	DetailH int

	ChartW      int
	ChartH      int
	SuggestionW int
	SuggestionH int
	Side        bool
}

// WorkspaceGeometry allocates the table from its row count rather than making
// a short comparison fill the screen. Selected detail gets the remaining left
// column. Wide terminals add trend and suggestion panes on the right.
func WorkspaceGeometry(width, height, rowCount int) WorkspaceLayout {
	width = max(width, 1)
	height = max(height, 1)
	rowCount = max(rowCount, 0)

	g := WorkspaceLayout{
		SummaryH: workspaceSummaryHeight(width, height),
		MachineH: 1,
	}
	if g.SummaryH+g.MachineH > height {
		g.SummaryH = min(g.SummaryH, height)
		g.MachineH = min(g.MachineH, height-g.SummaryH)
	}
	g.BodyH = max(0, height-g.SummaryH-g.MachineH)
	g.Side = width >= workspaceWideW && height >= workspaceWideH && g.BodyH >= 13

	leftOuterW := width
	rightOuterW := 0
	if g.Side {
		leftOuterW = (width - workspaceGap) * 3 / 5
		rightOuterW = width - workspaceGap - leftOuterW
	}
	g.TableW = max(0, leftOuterW-workspaceFrame)
	g.DetailW = g.TableW

	// Comparison rows take priority. Details appear only when the complete
	// visible table and at least three useful detail lines fit together.
	if g.BodyH >= 9 {
		tableOuterH := max(rowCount+2, 3)
		tableOuterH = min(tableOuterH, g.BodyH-workspaceGap-3)
		g.TableH = max(0, tableOuterH-workspaceFrame)
		detailOuterH := g.BodyH - workspaceGap - tableOuterH
		g.DetailH = max(0, detailOuterH-workspaceFrame)
	} else if g.BodyH >= 3 {
		g.TableH = g.BodyH - workspaceFrame
	}

	if g.Side {
		g.ChartW = max(0, rightOuterW-workspaceFrame)
		g.SuggestionW = g.ChartW
		chartOuterH := g.BodyH * 2 / 3
		chartOuterH = max(chartOuterH, 7)
		chartOuterH = min(chartOuterH, g.BodyH-workspaceGap-5)
		g.ChartH = max(0, chartOuterH-workspaceFrame)
		suggestionOuterH := g.BodyH - workspaceGap - chartOuterH
		g.SuggestionH = max(0, suggestionOuterH-workspaceFrame)
	}

	return g
}

// WorkspaceData contains already-rendered interactive components. The root
// renders the persistent range, group, sort, chart and filter controls above it.
type WorkspaceData struct {
	Totals   store.Bucket
	Timeline []store.Bucket

	RowCount   int
	Table      string
	Detail     string
	Trend      string
	Machine    string
	Suggestion string
}

// Workspace composes a production usage workspace without owning navigation,
// selection or widget state. Every output frame is bounded to width and height.
func Workspace(c Ctx, d WorkspaceData, width, height int) string {
	g := WorkspaceGeometry(width, height, d.RowCount)
	parts := []string{workspaceFit(workspaceSummary(c, d.Totals, d.Timeline, width, g.SummaryH <= 3), width, g.SummaryH)}
	if g.BodyH > 0 {
		parts = append(parts, workspaceBody(c, d, g, width))
	}
	if g.MachineH > 0 {
		parts = append(parts, workspaceFit(d.Machine, width, g.MachineH))
	}
	return workspaceFit(lipgloss.JoinVertical(lipgloss.Left, parts...), width, height)
}

func workspaceSummaryHeight(width, height int) int {
	if height < 22 {
		return 3
	}
	switch {
	case width >= 84:
		return 6
	case width >= 38:
		return 11
	default:
		return 21
	}
}

func workspaceSummary(c Ctx, totals store.Bucket, timeline []store.Bucket, width int, compact bool) string {
	cost := workspaceCostLines(c, totals)
	if compact {
		return workspaceCompactSummary(c, totals, cost[0], width)
	}
	inputStyle := workspaceCompStyle(c, "input")
	outputStyle := workspaceCompStyle(c, "output")
	cacheStyle := workspaceCompStyle(c, "cache")
	tiles := []string{
		c.mark(ZoneWorkspaceInput, workspaceSummaryCell(c, "Input", humanizeOr(c, totals.Input), []string{"tokens"}, inputStyle, workspaceTokenTrend(timeline, func(b store.Bucket) int64 { return b.Input }), workspaceSummaryColumns(width))),
		c.mark(ZoneWorkspaceOutput, workspaceSummaryCell(c, "Output", humanizeOr(c, totals.Output), []string{"tokens"}, outputStyle, workspaceTokenTrend(timeline, func(b store.Bucket) int64 { return b.Output }), workspaceSummaryColumns(width))),
		c.mark(ZoneWorkspaceCache, workspaceSummaryCell(c, "Cache", workspaceCacheValue(c, totals.CacheRead, totals.CacheCreation), []string{
			"read " + humanizeOr(c, totals.CacheRead),
			"write " + humanizeOr(c, totals.CacheCreation),
		}, cacheStyle, workspaceCacheTrend(timeline), workspaceSummaryColumns(width))),
		c.mark(ZoneWorkspaceCost, workspaceSummaryCell(c, "Cost", cost[0], cost[1:], c.Stat, workspaceKnownCostTrend(timeline), workspaceSummaryColumns(width))),
	}

	cols := workspaceSummaryColumnCount(width)
	rows := make([]string, 0, (len(tiles)+cols-1)/cols)
	for start := 0; start < len(tiles); start += cols {
		end := min(start+cols, len(tiles))
		segs := make([]string, 0, 2*(end-start)-1)
		for i := start; i < end; i++ {
			if i > start {
				segs = append(segs, workspaceSummaryDivider(c))
			}
			segs = append(segs, tiles[i])
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, segs...))
	}

	head := c.titleRule("AI usage · total "+humanizeOr(c, totals.Total)+" tokens", width, false)
	return lipgloss.JoinVertical(lipgloss.Left, append([]string{head}, rows...)...)
}

func workspaceCompactSummary(c Ctx, totals store.Bucket, cost string, width int) string {
	cellW := max(1, (width-3)/2)
	tiles := []string{
		c.mark(ZoneWorkspaceInput, workspaceCompactSummaryCell(c, "Input", humanizeOr(c, totals.Input), workspaceCompStyle(c, "input"), cellW)),
		c.mark(ZoneWorkspaceOutput, workspaceCompactSummaryCell(c, "Output", humanizeOr(c, totals.Output), workspaceCompStyle(c, "output"), cellW)),
		c.mark(ZoneWorkspaceCache, workspaceCompactSummaryCell(c, "Cache", workspaceCacheValue(c, totals.CacheRead, totals.CacheCreation), workspaceCompStyle(c, "cache"), cellW)),
		c.mark(ZoneWorkspaceCost, workspaceCompactSummaryCell(c, "Cost", cost, c.Stat, cellW)),
	}
	divider := c.Faint.Render(" │ ")
	head := c.titleRule("AI usage · total "+humanizeOr(c, totals.Total)+" tokens", width, false)
	return lipgloss.JoinVertical(lipgloss.Left,
		head,
		lipgloss.JoinHorizontal(lipgloss.Top, tiles[0], divider, tiles[1]),
		lipgloss.JoinHorizontal(lipgloss.Top, tiles[2], divider, tiles[3]),
	)
}

func workspaceCompactSummaryCell(c Ctx, label, value string, valueStyle lipgloss.Style, width int) string {
	labelW := min(lipgloss.Width(label), max(1, width/2))
	metricStyle := workspaceMetricStyle(valueStyle)
	line := metricStyle.Width(labelW).Render(label) + " " + metricStyle.Render(value)
	return workspaceFit(line, width, 1)
}

func workspaceSummaryColumnCount(width int) int {
	switch {
	case width >= 84:
		return 4
	case width >= 38:
		return 2
	default:
		return 1
	}
}

func workspaceSummaryColumns(width int) int {
	cols := workspaceSummaryColumnCount(width)
	return max(1, (width-3*(cols-1))/cols)
}

type workspaceTrend struct {
	Values    []float64
	Available bool
	Label     string
}

func workspaceSummaryCell(c Ctx, label, value string, foot []string, valueStyle lipgloss.Style, trend workspaceTrend, width int) string {
	metricStyle := workspaceMetricStyle(valueStyle)
	lines := []string{metricStyle.Render(label), metricStyle.Render(value)}
	for _, line := range foot {
		lines = append(lines, workspaceSecondaryStyle(c).Render(line))
	}
	spare := 5 - len(lines)
	if spare > 0 && (len(trend.Values) > 0 || trend.Label != "") {
		lines = append(lines, workspaceTrendRows(c, trend, valueStyle, width, spare)...)
	}
	return workspaceFit(strings.Join(lines, "\n"), width, 5)
}

func workspaceMetricStyle(style lipgloss.Style) lipgloss.Style {
	return style.Bold(true)
}

func workspaceSecondaryStyle(c Ctx) lipgloss.Style {
	return c.Subtle.Italic(true)
}

func workspaceCacheValue(c Ctx, read, creation int64) string {
	if read > 0 && creation > math.MaxInt64-read {
		return fmt.Sprintf("%.1fT", (float64(read)+float64(creation))/1_000_000_000_000)
	}
	return humanizeOr(c, read+creation)
}

func workspaceTokenTrend(timeline []store.Bucket, pick func(store.Bucket) int64) workspaceTrend {
	if len(timeline) == 0 {
		return workspaceTrend{}
	}
	values := make([]float64, len(timeline))
	for i, bucket := range timeline {
		values[i] = float64(max(int64(0), pick(bucket)))
	}
	return workspaceTrend{Values: values, Available: true, Label: "Trend · range"}
}

func workspaceCacheTrend(timeline []store.Bucket) workspaceTrend {
	if len(timeline) == 0 {
		return workspaceTrend{}
	}
	values := make([]float64, len(timeline))
	for i, bucket := range timeline {
		values[i] = float64(max(int64(0), bucket.CacheRead)) + float64(max(int64(0), bucket.CacheCreation))
	}
	return workspaceTrend{Values: values, Available: true, Label: "Trend · range"}
}

// workspaceKnownCostTrend omits buckets with no priced observation. Plotting
// those as zero would turn unavailable cost into a measured zero.
func workspaceKnownCostTrend(timeline []store.Bucket) workspaceTrend {
	if len(timeline) == 0 {
		return workspaceTrend{}
	}
	values := make([]float64, 0, len(timeline))
	missing := false
	for _, bucket := range timeline {
		priced := max(int64(0), bucket.Events-bucket.UnpricedEvents)
		if priced == 0 && bucket.CostMicroUSD == 0 {
			missing = true
			continue
		}
		values = append(values, float64(max(int64(0), bucket.CostMicroUSD)))
	}
	if len(values) == 0 {
		return workspaceTrend{Label: "Trend · unavailable"}
	}
	label := "Trend · range"
	if missing {
		label = "Trend · priced"
	}
	return workspaceTrend{Values: values, Available: true, Label: label}
}

func workspaceTrendRows(c Ctx, trend workspaceTrend, style lipgloss.Style, width, rows int) []string {
	if !trend.Available {
		return []string{workspaceSecondaryStyle(c).Render(trend.Label)}
	}
	label := trend.Label
	var peak float64
	for _, value := range trend.Values {
		peak = max(peak, value)
	}
	if peak == 0 {
		return []string{workspaceSecondaryStyle(c).Render(label + " · 0")}
	}
	if rows == 1 {
		labelW := lipgloss.Width(label)
		plotW := width - labelW - 1
		if plotW < 2 {
			return []string{workspaceSecondaryStyle(c).Render(label)}
		}
		return []string{workspaceSecondaryStyle(c).Render(label) + c.pad(1) + workspaceSparkline(style, trend.Values, plotW)}
	}
	return []string{workspaceSecondaryStyle(c).Render(label), workspaceSparkline(style, trend.Values, width)}
}

func workspaceSparkline(style lipgloss.Style, values []float64, width int) string {
	if width <= 0 || len(values) == 0 {
		return ""
	}
	plot := sparkline.New(width, 1,
		sparkline.WithStyle(style),
		sparkline.WithData(values),
	)
	plot.Draw()
	return workspaceFit(plot.View(), width, 1)
}

func workspaceSummaryDivider(c Ctx) string {
	return c.Faint.Render(strings.TrimSuffix(strings.Repeat(" │ \n", 5), "\n"))
}

func workspaceCompStyle(c Ctx, key string) lipgloss.Style {
	for _, spec := range c.Comp {
		if spec.Key == key {
			return spec.Style()
		}
	}
	return c.Stat
}

func workspaceCostLines(c Ctx, totals store.Bucket) []string {
	if totals.Events == 0 && totals.UnpricedEvents == 0 && totals.CostMicroUSD == 0 {
		return []string{"No usage", "0 events"}
	}
	if c.Money == nil {
		return []string{"Unavailable", "cost formatter unavailable"}
	}

	priced := max(int64(0), totals.Events-totals.UnpricedEvents)
	computed := min(max(int64(0), totals.ComputedCostEvents), priced)
	reported := priced - computed
	value := costValue(c, totals.CostMicroUSD, totals.UnpricedEvents, computed)
	if priced == 0 && totals.CostMicroUSD == 0 {
		value = c.Money(0, false, false)
	}
	lines := []string{value}
	if reported > 0 {
		lines = append(lines, workspaceEventCount(c, reported, "reported"))
	}
	if computed > 0 {
		lines = append(lines, workspaceEventCount(c, computed, "computed"))
	}
	if totals.UnpricedEvents > 0 {
		lines = append(lines, workspaceEventCount(c, totals.UnpricedEvents, "unpriced"))
	}
	if totals.Events == 0 && totals.CostMicroUSD > 0 {
		lines = append(lines, "event count unavailable")
	}
	return lines
}

func workspaceEventCount(c Ctx, count int64, provenance string) string {
	noun := "events"
	if count == 1 {
		noun = "event"
	}
	return fmt.Sprintf("%s %s %s", humanizeOr(c, count), provenance, noun)
}

func workspaceBody(c Ctx, d WorkspaceData, g WorkspaceLayout, width int) string {
	left := workspacePane(c, workspaceFallback(d.Table, "No models in this scope."), g.TableW, g.TableH)
	if g.DetailH > 0 {
		detail := workspacePane(c, workspaceFallback(d.Detail, "Select a model to inspect its usage."), g.DetailW, g.DetailH)
		left = lipgloss.JoinVertical(lipgloss.Left, left, strings.Repeat(" ", g.TableW+workspaceFrame), detail)
	}
	if !g.Side {
		return workspaceFit(left, width, g.BodyH)
	}

	right := workspacePane(c, workspaceFallback(d.Trend, "No trend data for this scope."), g.ChartW, g.ChartH)
	if g.SuggestionH > 0 {
		suggestion := workspacePane(c, workspaceFallback(d.Suggestion, "No suggestions for this selection."), g.SuggestionW, g.SuggestionH)
		right = lipgloss.JoinVertical(lipgloss.Left, right, strings.Repeat(" ", g.ChartW+workspaceFrame), suggestion)
	}
	return workspaceFit(lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right), width, g.BodyH)
}

func workspacePane(c Ctx, content string, innerW, innerH int) string {
	if innerW <= 0 || innerH <= 0 {
		return ""
	}
	return workspaceFit(content, innerW, innerH)
}

func workspaceFallback(content, fallback string) string {
	if strings.TrimSpace(content) == "" {
		return fallback
	}
	return content
}

func workspaceFit(content string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.SplitN(content, "\n", height+1)
	var out strings.Builder
	out.Grow(width*height + height - 1)
	for row := range height {
		if row > 0 {
			out.WriteByte('\n')
		}
		line := ""
		if row < len(lines) {
			line = ansi.Truncate(lines[row], width, "")
		}
		out.WriteString(line)
		if pad := width - ansi.StringWidth(line); pad > 0 {
			out.WriteString(strings.Repeat(" ", pad))
		}
	}
	return out.String()
}
