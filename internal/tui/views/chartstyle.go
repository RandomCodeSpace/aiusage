package views

import (
	"math"
	"strings"
	"time"

	"github.com/NimbleMarkets/ntcharts/linechart"
	"github.com/NimbleMarkets/ntcharts/linechart/timeserieslinechart"
	"github.com/NimbleMarkets/ntcharts/sparkline"
	"github.com/charmbracelet/lipgloss"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// chartstyle.go centralises construction of ntcharts widgets from
// []store.Bucket, styled with the per-component token colors + tool accents and
// wired to the shared zone manager. Views call these instead of touching
// ntcharts directly. Trend series names, colors and order come from Ctx.Comp
// (the component token model in components.go).

// newColumnSparkline uses solid block columns for a self-scaled magnitude row
// (reads better at h=1 for KPI tiles).
func newColumnSparkline(values []float64, w, h int, style lipgloss.Style) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	sl := sparkline.New(w, h, sparkline.WithStyle(style))
	sl.PushAll(values)
	sl.Draw()
	return sl.View()
}

// heroBody renders the shared token trend used by Overview, Timeline and the
// per-entity preview cards: input, output, cache-read and cache-creation as four
// contrasting lines on a LOG axis. cache-read dwarfs input/output by ~100x, so a
// linear axis would flatten them to the baseline; a log axis keeps all four
// readable at true magnitude. When the pane is too small for an axed chart it
// degrades to a per-series strip (sparklines or numbers) — never a total.
// scrubIdx (>=0 when pinned) marks the scrub column.
func heroBody(c Ctx, buckets []store.Bucket, dim string, lay Layout, w, h, scrubIdx int) string {
	if w < 8 {
		w = 8
	}
	if h < 1 {
		h = 1
	}
	if len(buckets) == 0 {
		return emptyChartFrame(c, w, h)
	}
	if lay.ChartMode == ChartFull && w >= minChartW && h >= 5 {
		// Clamp to exactly h rows: ntcharts can emit one trailing axis/overflow
		// row beyond the requested canvas height for some data, which would push
		// the panel one line past its budget. fitHeight makes the contract exact.
		return fitHeight(trendChart(c, buckets, dim, w, h, scrubIdx), h)
	}
	return trendStrip(c, buckets, w, h)
}

// fitHeight forces s to exactly h lines: extra lines are dropped, short blocks
// are padded with blanks. Keeps a view's height contract exact regardless of any
// underlying widget's row count.
func fitHeight(s string, h int) string {
	if h < 1 {
		h = 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// trendChart plots the four token series on a logarithmic Y axis: each value v
// is plotted as log10(1+v) and the Y tick labels are mapped back to humanized
// counts (1K/1M/1B). Series colors and draw order come from c.Comp. The latest
// bucket gets an amber "now" column; a pinned scrubIdx gets an accent crosshair.
func trendChart(c Ctx, buckets []store.Bucket, dim string, w, h, scrubIdx int) string {
	if w < 8 {
		w = 8
	}
	if h < 4 {
		h = 4
	}
	times := bucketTimes(buckets, dim)
	if len(times) == 0 {
		return emptyChartFrame(c, w, h)
	}

	var maxV int64
	for _, b := range buckets {
		comp := Split(b)
		for _, s := range c.Comp {
			if v := s.Pick(comp); v > maxV {
				maxV = v
			}
		}
	}
	maxLog := logT(maxV)
	if maxLog < 1 {
		maxLog = 1
	}

	minT, maxT := times[0], times[len(times)-1]
	if !maxT.After(minT) {
		maxT = minT.Add(time.Hour)
	}

	axis := lipgloss.NewStyle().Foreground(c.FaintColor)
	label := lipgloss.NewStyle().Foreground(c.FaintColor)
	tslc := timeserieslinechart.New(w, h,
		timeserieslinechart.WithTimeRange(minT, maxT),
		timeserieslinechart.WithYRange(0, maxLog*1.02),
		timeserieslinechart.WithXYSteps(4, 3),
		timeserieslinechart.WithAxesStyles(axis, label),
		timeserieslinechart.WithXLabelFormatter(xLabelFormatter(dim)),
		timeserieslinechart.WithYLabelFormatter(logYLabelFormatter(c)),
	)
	order := make([]string, 0, len(c.Comp))
	for _, s := range c.Comp {
		tslc.SetDataSetStyle(s.Key, s.Style())
		order = append(order, s.Key)
	}
	for i, b := range buckets {
		comp := Split(b)
		for _, s := range c.Comp {
			tslc.PushDataSet(s.Key, timeserieslinechart.TimePoint{Time: times[i], Value: logT(s.Pick(comp))})
		}
	}
	tslc.DrawBrailleDataSets(order)

	tslc.SetColumnBackgroundStyle(times[len(times)-1], lipgloss.NewStyle().Background(c.NowColor))
	if scrubIdx >= 0 && scrubIdx < len(times) {
		tslc.SetColumnBackgroundStyle(times[scrubIdx], lipgloss.NewStyle().Background(c.AccentColor))
	}
	return c.mark(zoneHero, tslc.View())
}

// trendStrip is the small-pane fallback: one self-scaled sparkline row per series
// when there is vertical room, else a single-line per-series numeric readout. It
// never shows a total.
func trendStrip(c Ctx, buckets []store.Bucket, w, h int) string {
	if h >= len(c.Comp) {
		const lbl = 9 // glyph + space + short(6) + space
		sw := w - lbl
		if sw < 3 {
			sw = 3
		}
		rows := make([]string, 0, len(c.Comp))
		for _, s := range c.Comp {
			vals := make([]float64, len(buckets))
			for i, b := range buckets {
				vals[i] = float64(s.Pick(Split(b)))
			}
			label := s.Glyph + " " + padRightLocal(s.Short, 6) + " "
			rows = append(rows, s.Style().Render(label)+newColumnSparkline(vals, sw, 1, s.Style()))
		}
		return strings.Join(rows, "\n")
	}
	last := Split(buckets[len(buckets)-1])
	parts := make([]string, 0, len(c.Comp))
	for _, s := range c.Comp {
		parts = append(parts, s.Style().Render(s.Glyph+" "+s.Short+" "+humanizeOr(c, s.Pick(last))))
	}
	line := strings.Join(parts, c.Subtle.Render(" · "))
	if c.Truncate != nil {
		line = c.Truncate(line, w)
	}
	return line
}

// logT maps a token count to its log-axis position: log10(1+v). The +1 keeps
// zero at the baseline and avoids -Inf.
func logT(v int64) float64 {
	if v <= 0 {
		return 0
	}
	return math.Log10(1 + float64(v))
}

// logYLabelFormatter inverts logT for axis tick labels: 10^v - 1, humanized.
func logYLabelFormatter(c Ctx) linechart.LabelFormatter {
	return func(_ int, v float64) string {
		if c.Humanize == nil {
			return ""
		}
		raw := math.Pow(10, v) - 1
		if raw < 0 {
			raw = 0
		}
		return c.Humanize(int64(raw))
	}
}

// humanizeOr formats n via the injected Humanize, defending against a nil helper
// in headless tests.
func humanizeOr(c Ctx, n int64) string {
	if c.Humanize == nil {
		return ""
	}
	return c.Humanize(n)
}

// padRightLocal is a small alignment helper used where the Ctx helpers may be
// nil (defensive) and to keep chartstyle self-contained.
func padRightLocal(s string, w int) string {
	if len([]rune(s)) >= w {
		return string([]rune(s)[:w])
	}
	return s + strings.Repeat(" ", w-len([]rune(s)))
}

// bucketTimes parses each bucket's time key (dim "day"/"hour"/"week"/"month")
// into time.Time. Unparseable buckets are dropped.
func bucketTimes(buckets []store.Bucket, dim string) []time.Time {
	out := make([]time.Time, 0, len(buckets))
	for _, b := range buckets {
		if t, ok := parseBucketTime(b.Keys[dim], dim); ok {
			out = append(out, t)
		}
	}
	return out
}

// ParseBucketTime parses a store bucket key into a time.Time using the layout
// implied by the grouping dimension. Exported so package tui can resolve a
// bucket to a [since,until) window for scrub re-pricing.
func ParseBucketTime(v, dim string) (time.Time, bool) { return parseBucketTime(v, dim) }

// BucketTimestamp returns a human-readable label for a bucket's time key.
func BucketTimestamp(b store.Bucket, dim string) string { return bucketLabel(b, dim) }

// parseBucketTime parses a store bucket key into a time.Time using the layout
// implied by the grouping dimension.
func parseBucketTime(v, dim string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	var layouts []string
	switch dim {
	case "hour":
		layouts = []string{"2006-01-02 15", "2006-01-02T15", "2006-01-02 15:04"}
	case "day":
		layouts = []string{"2006-01-02"}
	case "week":
		layouts = []string{"2006-01-02", "2006-W01"}
	case "month":
		layouts = []string{"2006-01", "2006-01-02"}
	default:
		layouts = []string{"2006-01-02 15", "2006-01-02", "2006-01"}
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// xLabelFormatter returns the X axis label formatter for the grouping dim.
func xLabelFormatter(dim string) linechart.LabelFormatter {
	return func(_ int, v float64) string {
		t := time.Unix(int64(v), 0).UTC()
		if dim == "hour" {
			return t.Format("15:04")
		}
		return t.Format("01/02")
	}
}

// emptyChartFrame renders a faint ghosted axis frame with a centered hint for
// the empty state, sized to w×h.
func emptyChartFrame(c Ctx, w, h int) string {
	hint := "No usage in range — press [ ] to widen or t to change range"
	if c.Truncate != nil {
		hint = c.Truncate(hint, w-2)
	}
	box := lipgloss.NewStyle().Width(w).Height(h).
		Align(lipgloss.Center, lipgloss.Center).
		Foreground(c.FaintColor)
	return box.Render(hint)
}
