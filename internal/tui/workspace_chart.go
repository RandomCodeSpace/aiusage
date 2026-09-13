package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/runes"
	"github.com/NimbleMarkets/ntcharts/v2/linechart"
	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

var workspaceChartMetrics = [...]UsageMetric{
	UsageMetricInput,
	UsageMetricOutput,
	UsageMetricCache,
	UsageMetricCost,
	UsageMetricTotal,
}

// Retain one rendered plot, following the existing hero memo's applied-data
// identity. Row selection and live machine samples do not change this plot.
// A pointer survives Bubble Tea's model copies; a replacement dataset, metric,
// geometry, or palette replaces the single entry.
type workspaceTrendMemo struct {
	mu     sync.Mutex
	key    workspaceTrendKey
	body   string
	valid  bool
	builds int
}

type workspaceTrendKey struct {
	gen     uint64
	first   *store.Bucket
	n, w, h int
	dim     string
	metric  UsageMetric
	style   string
}

// renderWorkspaceTrend renders the selected metric from the already-applied
// overview timeline. It performs no query: changing the chart metric is a
// presentation-only operation over m.tlData.
func (m Model) renderWorkspaceTrend(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}

	metric := workspaceChartMetric(m.workspace.chart)
	title := m.workspaceChartStyle(metric).Bold(true).Render(
		fmt.Sprintf("%s trend · %s", metric, workspaceChartUnit(metric)),
	)
	chooser := m.renderWorkspaceChartChooser(metric)
	if h == 1 {
		return workspaceChartFit(title, w, h)
	}
	if h == 2 {
		return workspaceChartFit(title+"\n"+chooser, w, h)
	}

	bodyH := h - 2
	body := m.workspaceTrendBody(metric, w, bodyH)
	body = workspaceChartFit(body, w, bodyH)
	return workspaceChartFit(title+"\n"+body+"\n"+chooser, w, h)
}

func (m Model) workspaceTrendBody(metric UsageMetric, w, h int) string {
	memo := m.workspace.trendMemo
	if memo == nil {
		return m.buildWorkspaceTrendBody(metric, w, h)
	}
	key := workspaceTrendKey{gen: m.dataGen, n: len(m.tlData.Buckets), w: w, h: h, dim: m.tlData.Dim, metric: metric,
		style: m.workspaceChartStyle(metric).Render("x") + m.th.Subtle.Render("x")}
	if len(m.tlData.Buckets) > 0 {
		key.first = &m.tlData.Buckets[0]
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if !memo.valid || memo.key != key {
		memo.body = m.buildWorkspaceTrendBody(metric, w, h)
		memo.key, memo.valid = key, true
		memo.builds++
	}
	return memo.body
}

func (m Model) buildWorkspaceTrendBody(metric UsageMetric, w, h int) string {
	points, start, end, peak := workspaceTrendPoints(m.tlData, metric)
	body := "No usage in this scope and range"
	switch {
	case len(points) == 0:
	case metric == UsageMetricCost && !workspaceTrendHasKnownCost(m.tlData):
		body = "Unavailable: all usage events unpriced"
	case peak == 0:
		body = "Recorded zero · " + workspaceChartValue(metric, 0)
	case len(points) == 1:
		body = fmt.Sprintf("%s · %s", workspaceChartTimestamp(points[0].Time, m.tlData.Dim), workspaceChartValue(metric, peak))
	default:
		body = m.drawWorkspaceTrend(points, start, end, peak, metric, w, h)
	}
	return body
}

func (m Model) drawWorkspaceTrend(points []timeserieslinechart.TimePoint, start, end time.Time, peak float64, metric UsageMetric, w, h int) string {
	// At this size ntcharts cannot retain both axes and a graph cell. Keep the
	// exact latest and peak readings visible until the pane grows back.
	if w < 20 || h < 5 {
		latest := points[len(points)-1].Value
		return fmt.Sprintf("Latest %s · peak %s", workspaceChartValue(metric, latest), workspaceChartValue(metric, peak))
	}

	yMax := peak
	if yMax < 1 {
		yMax = 1
	}
	plot := timeserieslinechart.New(w, h,
		timeserieslinechart.WithTimeRange(start, end),
		timeserieslinechart.WithYRange(0, yMax),
		timeserieslinechart.WithXYSteps(max(8, w/2), max(2, (h-2)/2)),
		timeserieslinechart.WithLineStyle(runes.ArcLineStyle),
		timeserieslinechart.WithStyle(m.workspaceChartStyle(metric)),
		timeserieslinechart.WithAxesStyles(m.th.Subtle, m.th.Subtle),
		timeserieslinechart.WithXLabelFormatter(workspaceChartXLabel(m.tlData.Dim)),
		timeserieslinechart.WithYLabelFormatter(workspaceChartYLabel(metric)),
	)
	for _, point := range points {
		plot.Push(point)
	}
	// Draw uses ntcharts' continuous arc-line renderer. Braille and dotted
	// renderers are intentionally not used for this workspace plot.
	plot.Draw()
	return plot.View()
}

func (m Model) renderWorkspaceChartChooser(selected UsageMetric) string {
	parts := make([]string, 0, len(workspaceChartMetrics))
	for _, metric := range workspaceChartMetrics {
		label := string(metric)
		if metric == selected {
			label = m.th.CrumbActive.Render("[" + label + "]")
		} else {
			label = m.th.Crumb.Render(label)
		}
		parts = append(parts, m.zoneMark("workspace-chart-"+string(metric), label))
	}
	return m.th.Subtle.Render("Chart: ") + strings.Join(parts, " ")
}

func (m Model) workspaceChartStyle(metric UsageMetric) lipgloss.Style {
	key := strings.ToLower(string(metric))
	for _, spec := range m.vctx.Comp {
		if spec.Key == key {
			return spec.Style()
		}
	}
	if metric == UsageMetricCost {
		return lipgloss.NewStyle().Foreground(m.th.Now)
	}
	return lipgloss.NewStyle().Foreground(m.th.Accent)
}

func workspaceChartMetric(metric UsageMetric) UsageMetric {
	for _, candidate := range workspaceChartMetrics {
		if metric == candidate {
			return metric
		}
	}
	return UsageMetricTotal
}

func workspaceChartUnit(metric UsageMetric) string {
	if metric == UsageMetricCost {
		return "known USD"
	}
	return "tokens"
}

func workspaceTrendPoints(data views.TimelineData, metric UsageMetric) ([]timeserieslinechart.TimePoint, time.Time, time.Time, float64) {
	points := make([]timeserieslinechart.TimePoint, 0, len(data.Buckets))
	var start, end time.Time
	var peak float64
	for _, bucket := range data.Buckets {
		at, ok := views.ParseBucketTime(bucket.Keys[data.Dim], data.Dim)
		if !ok {
			continue
		}
		value := workspaceTrendValue(metric, bucket)
		points = append(points, timeserieslinechart.TimePoint{Time: at, Value: value})
		if len(points) == 1 || at.Before(start) {
			start = at
		}
		if len(points) == 1 || at.After(end) {
			end = at
		}
		if value > peak {
			peak = value
		}
	}
	return points, start, end, peak
}

func workspaceTrendValue(metric UsageMetric, bucket store.Bucket) float64 {
	switch metric {
	case UsageMetricInput:
		return float64(bucket.Input)
	case UsageMetricOutput:
		return float64(bucket.Output)
	case UsageMetricCache:
		return float64(bucket.CacheRead) + float64(bucket.CacheCreation)
	case UsageMetricCost:
		return float64(bucket.CostMicroUSD)
	default:
		return float64(bucket.Total)
	}
}

func workspaceChartValue(metric UsageMetric, value float64) string {
	if metric == UsageMetricCost {
		return model.FormatCost(int64(value), false, true)
	}
	return HumanizeTokens(int64(value)) + " tokens"
}

func workspaceTrendHasKnownCost(data views.TimelineData) bool {
	for _, bucket := range data.Buckets {
		if bucket.CostMicroUSD != 0 || bucket.Events > bucket.UnpricedEvents {
			return true
		}
	}
	return false
}

func workspaceChartTimestamp(at time.Time, dim string) string {
	if dim == "hour" {
		return at.Format("2006-01-02 15:00")
	}
	return at.Format("2006-01-02")
}

func workspaceChartXLabel(dim string) linechart.LabelFormatter {
	return func(_ int, value float64) string {
		at := time.Unix(int64(value), 0).In(time.Local)
		if dim == "hour" {
			return at.Format("15:04")
		}
		return at.Format("01/02")
	}
}

func workspaceChartYLabel(metric UsageMetric) linechart.LabelFormatter {
	return func(_ int, value float64) string {
		if metric == UsageMetricCost {
			if value >= 1_000_000 {
				return "$" + HumanizeTokens(int64(value)/1_000_000)
			}
			return model.FormatCost(int64(value), false, true)
		}
		return HumanizeTokens(int64(value))
	}
}

func workspaceChartFit(s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h).Render(s)
}
