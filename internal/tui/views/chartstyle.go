package views

import (
	"math"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/linechart"
	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"
	"github.com/NimbleMarkets/ntcharts/v2/sparkline"

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
	// w is the pane's post-chrome inner width, so it is measured against the
	// inner gate - the same one heroFrameFor uses (issue #48).
	if lay.ChartMode == ChartFull && w >= minChartInnerW && h >= minHeroLogH {
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
	tslc, times, ok := buildTrendChart(c, buckets, dim, w, h)
	if !ok {
		return emptyChartFrame(c, w, h)
	}
	paintTrendHighlights(c, tslc, times, scrubIdx)
	return c.mark(zoneHero, tslc.View())
}

// buildTrendChart constructs the braille trend chart WITHOUT the now/scrub
// column highlights. The split from trendChart exists for the render memo
// (HeroMemo): the expensive braille build happens once per dataset+geometry and
// the highlights are applied afterwards as a canvas post-pass, so a scrub step
// never redraws braille. ok=false when no bucket carries a parseable time key.
func buildTrendChart(c Ctx, buckets []store.Bucket, dim string, w, h int) (*timeserieslinechart.Model, []time.Time, bool) {
	times := bucketTimes(buckets, dim)
	// bucketTimes DROPS a bucket whose key does not parse, so a single bad key
	// leaves times shorter than buckets — and the push loop below indexes
	// times[i] over buckets, i.e. out of range inside View(). Fall back on the
	// same length guard buildHeroFrame applies rather than plot a series whose
	// points are silently shifted onto the wrong timestamps.
	if len(times) == 0 || len(times) != len(buckets) {
		return nil, nil, false
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
	return &tslc, times, true
}

// paintTrendHighlights applies the amber "now" column on the latest bucket and
// (when scrubIdx is a valid index) the accent scrub crosshair. It only touches
// cell backgrounds on the built canvas — the braille content is untouched.
func paintTrendHighlights(c Ctx, t *timeserieslinechart.Model, times []time.Time, scrubIdx int) {
	t.SetColumnBackgroundStyle(times[len(times)-1], lipgloss.NewStyle().Background(c.NowColor))
	if scrubIdx >= 0 && scrubIdx < len(times) {
		t.SetColumnBackgroundStyle(times[scrubIdx], lipgloss.NewStyle().Background(c.AccentColor))
	}
}

// clearTrendHighlights undoes paintTrendHighlights on a retained chart so the
// memo can re-highlight it for a different scrub position later. An empty
// style's background is lipgloss's "no color" sentinel, which the renderer
// skips, so a repainted column renders exactly as if never highlighted.
func clearTrendHighlights(t *timeserieslinechart.Model, times []time.Time, scrubIdx int) {
	t.SetColumnBackgroundStyle(times[len(times)-1], lipgloss.NewStyle())
	if scrubIdx >= 0 && scrubIdx < len(times) {
		t.SetColumnBackgroundStyle(times[scrubIdx], lipgloss.NewStyle())
	}
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
			st := c.compStyle(s)
			rows = append(rows, st.Render(label)+newColumnSparkline(vals, sw, 1, st))
		}
		return strings.Join(rows, "\n")
	}
	last := Split(buckets[len(buckets)-1])
	parts := make([]string, 0, len(c.Comp))
	for _, s := range c.Comp {
		parts = append(parts, c.compStyle(s).Render(s.Glyph+" "+s.Short+" "+humanizeOr(c, s.Pick(last))))
	}
	return c.fitParts(parts, c.Subtle.Render(" · "), w)
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
// implied by the grouping dimension. Store keys are wall-clock strings formatted
// by SQLite with the 'localtime' modifier (store/query.go groupExpr), so they
// must be parsed back in time.Local — parsing as UTC shifts every derived
// [since,until) window by the UTC offset.
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
		return parseWeekKey(v)
	case "month":
		layouts = []string{"2006-01"}
	default:
		layouts = []string{"2006-01-02 15", "2006-01-02", "2006-01"}
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, v, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseWeekKey decodes a store %Y-W%W week key ("2026-W32") into local midnight
// of the week's first day. Go's time package has no layout verb for %W (week 01
// starts at the year's first Monday; earlier days are week 00), so the key is
// parsed by hand.
func parseWeekKey(v string) (time.Time, bool) {
	i := strings.Index(v, "-W")
	if i < 1 {
		return time.Time{}, false
	}
	year, err := strconv.Atoi(v[:i])
	if err != nil {
		return time.Time{}, false
	}
	week, err := strconv.Atoi(v[i+2:])
	if err != nil || week < 0 || week > 53 {
		return time.Time{}, false
	}
	jan1 := time.Date(year, time.January, 1, 0, 0, 0, 0, time.Local)
	if week == 0 {
		return jan1, true
	}
	firstMonday := (8 - int(jan1.Weekday())) % 7
	return jan1.AddDate(0, 0, firstMonday+(week-1)*7), true
}

// xLabelFormatter returns the X axis label formatter for the grouping dim.
// Labels render in local time so the axis matches the localtime bucket keys.
func xLabelFormatter(dim string) linechart.LabelFormatter {
	return func(_ int, v float64) string {
		t := time.Unix(int64(v), 0)
		if dim == "hour" {
			return t.Format("15:04")
		}
		return t.Format("01/02")
	}
}

// --- detented hero (issue #8) ------------------------------------------------
//
// Everything below builds the hero's ntcharts widgets. The geometry, series
// partitioning and text that frame them live in hero.go; the detent arithmetic
// in scale.go. Keeping widget construction here is the chartstyle seam
// (issue #13): views never touch ntcharts directly.

// heroPane is one built pane of the hero body: the fixed header line above it,
// the ntcharts model, and the exact row budget its canvas must fill. An empty
// header claims no row (the degraded log band has no pane header).
type heroPane struct {
	header string
	model  *timeserieslinechart.Model
	h      int
}

// heroFrame is a built hero body — its panes in render order, an optional
// footer strip, and the bucket times every pane shares. HeroMemo retains it
// across frames and re-renders it per scrub position: the braille is drawn
// once, the crosshair is a canvas post-pass.
type heroFrame struct {
	panes  []heroPane
	footer string
	times  []time.Time
	h      int
}

// render paints the now/scrub highlights onto every retained pane, renders the
// body, then clears them again so the retained canvases stay clean for the next
// scrub position.
func (f *heroFrame) render(c Ctx, scrubIdx int) string {
	rows := make([]string, 0, len(f.panes)*2+1)
	for _, p := range f.panes {
		paintTrendHighlights(c, p.model, f.times, scrubIdx)
		if p.header != "" {
			rows = append(rows, p.header)
		}
		rows = append(rows, fitHeight(p.model.View(), p.h))
		clearTrendHighlights(p.model, f.times, scrubIdx)
	}
	if f.footer != "" {
		rows = append(rows, f.footer)
	}
	return c.mark(zoneHero, fitHeight(strings.Join(rows, "\n"), f.h))
}

// buildHeroFrame builds the hero body of one kind at (w, h) WITHOUT the
// now/scrub highlights, so the memo can apply those as a post-pass. lock may be
// nil (an un-memoized render): then the detents are computed fresh.
// ok=false when the body cannot be built and the caller must fall back.
func buildHeroFrame(c Ctx, buckets []store.Bucket, dim string, kind heroFrameKind, w, h int, lock *detentLock) (*heroFrame, bool) {
	times := bucketTimes(buckets, dim)
	// Unparseable time keys would desynchronise times from buckets and with it
	// every index the scrub crosshair resolves; fall back instead of guessing.
	if len(times) == 0 || len(times) != len(buckets) {
		return nil, false
	}
	switch kind {
	case heroFrameLeverage:
		return buildLeverageFrame(c, buckets, times, dim, w, h, lock)
	case heroFrameTwoPane:
		return buildTwoPaneFrame(c, buckets, times, dim, w, h, lock)
	case heroFrameLog:
		return buildDecadeLogFrame(c, buckets, times, dim, w, h, lock)
	}
	return nil, false
}

// buildDecadeLogFrame builds the small-terminal hero (issue #39): every series
// on ONE quantized decade-ring log axis. Below the two-pane floor there is no
// room for two detented panes, but there is no reason for the band to drop out
// of the family — it keeps the exact detent rings, the text SCALE readout, the
// hysteresis lock and the per-run datasets, with a whole-decade pitch standing
// in for the token step. Labeled rows are therefore exact powers of ten, which
// is what a log axis can declare and a linear step cannot.
//
// ok=false when the pane cannot carry even one ring pitch, which leaves the
// caller on the sub-floor fallback (strip, then numbers).
func buildDecadeLogFrame(c Ctx, buckets []store.Bucket, times []time.Time, dim string, w, h int, lock *detentLock) (*heroFrame, bool) {
	if len(c.Comp) == 0 {
		return nil, false
	}
	chartH := h - 1      // the pane header
	graphH := chartH - 2 // the pane carries the x axis
	if graphH < detentYStep {
		return nil, false
	}

	pitch := lock.pickDecade("log", graphH, detentYStep, seriesMax(buckets, c.Comp))
	labelW := decadeLabelWidth(c, pitch, graphH, detentYStep)

	minT, maxT := times[0], times[len(times)-1]
	if !maxT.After(minT) {
		maxT = minT.Add(bucketStep(dim))
	}
	axis := lipgloss.NewStyle().Foreground(c.FaintColor)
	label := lipgloss.NewStyle().Foreground(c.FaintColor)
	tslc := timeserieslinechart.New(w, chartH,
		timeserieslinechart.WithTimeRange(minT, maxT),
		timeserieslinechart.WithYRange(0, detentViewMax(pitch, graphH, detentYStep)),
		timeserieslinechart.WithXYSteps(heroXSteps, detentYStep),
		timeserieslinechart.WithAxesStyles(axis, label),
		timeserieslinechart.WithXLabelFormatter(xLabelFormatter(dim)),
		timeserieslinechart.WithYLabelFormatter(decadeYLabel(c, pitch, detentYStep, labelW)),
	)
	// One dataset per contiguous run per series, exactly as the detented panes
	// do it: a gap must break the line, not draw a diagonal across the outage.
	runs := gapRuns(times, dim)
	order := make([]string, 0, len(c.Comp)*len(runs))
	for _, s := range c.Comp {
		for ri, run := range runs {
			name := s.Key + ":" + strconv.Itoa(ri)
			tslc.SetDataSetStyle(name, s.Style())
			for _, i := range run {
				tslc.PushDataSet(name, timeserieslinechart.TimePoint{
					Time: times[i], Value: decadeLogValue(s.Pick(Split(buckets[i])))})
			}
			order = append(order, name)
		}
	}
	tslc.DrawBrailleDataSets(order)
	return &heroFrame{
		panes: []heroPane{{
			header: paneHeader(c, c.Comp, "tokens", decadePitchLabel(pitch), w),
			model:  &tslc,
			h:      chartH,
		}},
		times: times,
		h:     h,
	}, true
}

// buildTwoPaneFrame builds the hero proper: fresh (input+output) over cache,
// each pane linearly scaled to its own detent and labelled with it.
//
// c.Trend selects the LINE TREATMENT only (ticket #65): the pane split, the
// detent ladder, the SCALE readouts, the shared gutter and the gap runs are the
// same whichever candidate is drawing. Candidate C is the one exception that
// touches arithmetic rather than ink - a mirrored fresh pane measures from a
// centerline, so its detent is picked against HALF the pane and its ring labels
// read as distance from that centerline.
func buildTwoPaneFrame(c Ctx, buckets []store.Bucket, times []time.Time, dim string, w, h int, lock *detentLock) (*heroFrame, bool) {
	// Candidate D is the one treatment that replaces the pane layout instead of
	// only its ink, so it is resolved before the split is computed. A pane too
	// short for three lanes falls through to the layout below, where D draws as
	// the shipped renderer rather than not at all.
	if c.Trend == TrendHeat {
		if f, ok := buildHeatFrame(c, buckets, times, dim, w, h); ok {
			return f, true
		}
	}
	freshSpecs, cacheSpecs := heroPaneSeries(c)
	if len(freshSpecs) == 0 || len(cacheSpecs) == 0 {
		return nil, false
	}
	freshH, cacheH := heroPaneSplit(h - 2) // two header rows
	freshGH, cacheGH := freshH, cacheH-2   // the cache pane carries the x axis
	if freshGH < detentYStep || cacheGH < detentYStep {
		return nil, false
	}

	cacheStep := lock.pick("cache", cacheGH, detentYStep, seriesMax(buckets, cacheSpecs))
	freshMax := seriesMax(buckets, freshSpecs)

	// The ridge keeps its own lock key: it quantizes against half a pane, so
	// sharing "fresh" would make the two treatments fight over the hysteresis.
	ridge, isRidge := ridgeGeom{}, false
	if c.Trend == TrendRidge {
		ridge, isRidge = newRidgeGeom(freshGH)
	}
	var freshStep int64
	var freshLabelW int
	if isRidge {
		freshStep = lock.pick("ridge", ridge.half, detentYStep, freshMax)
		freshLabelW = ridgeLabelWidth(c, freshStep, ridge)
	} else {
		freshStep = lock.pick("fresh", freshGH, detentYStep, freshMax)
		freshLabelW = detentLabelWidth(c, []detentAxis{{step: freshStep, graphH: freshGH}})
	}
	// The shared gutter width is settled before either widget exists: ntcharts
	// reserves the Y gutter from its formatter's widest output, and that width
	// is what column-aligns the two plot areas.
	labelW := detentLabelWidth(c, []detentAxis{{step: cacheStep, graphH: cacheGH}})
	if freshLabelW > labelW {
		labelW = freshLabelW
	}

	fresh := detentPane(c, buckets, times, dim, freshSpecs, w, freshH, freshGH, freshStep, 0, labelW)
	if isRidge {
		fresh = ridgePane(c, buckets, times, dim, freshSpecs, w, freshH, freshStep, ridge, labelW)
	}
	cache := detentPane(c, buckets, times, dim, cacheSpecs, w, cacheH, cacheGH, cacheStep, heroXSteps, labelW)
	return &heroFrame{
		panes: []heroPane{
			{header: paneHeader(c, freshSpecs, "fresh", detentHuman(c, freshStep), w), model: fresh, h: freshH},
			{header: paneHeader(c, cacheSpecs, "cache", detentHuman(c, cacheStep), w), model: cache, h: cacheH},
		},
		times: times,
		h:     h,
	}, true
}

// detentPane builds one linearly-scaled braille pane on a detented Y axis.
// xStep 0 hides the time axis (the fresh pane); the cache pane carries it for
// both. graphH must be the plot height ntcharts will derive for this canvas —
// the canvas height, minus two rows when the x axis is drawn — because it is
// the divisor that makes the ring values exact.
func detentPane(c Ctx, buckets []store.Bucket, times []time.Time, dim string,
	specs []CompSpec, w, h, graphH int, step int64, xStep, labelW int) *timeserieslinechart.Model {
	minT, maxT := times[0], times[len(times)-1]
	if !maxT.After(minT) {
		maxT = minT.Add(bucketStep(dim))
	}
	axis := lipgloss.NewStyle().Foreground(c.FaintColor)
	label := lipgloss.NewStyle().Foreground(c.FaintColor)
	tslc := timeserieslinechart.New(w, h,
		timeserieslinechart.WithTimeRange(minT, maxT),
		timeserieslinechart.WithYRange(0, detentViewMax(step, graphH, detentYStep)),
		timeserieslinechart.WithXYSteps(xStep, detentYStep),
		timeserieslinechart.WithAxesStyles(axis, label),
		timeserieslinechart.WithXLabelFormatter(xLabelFormatter(dim)),
		timeserieslinechart.WithYLabelFormatter(detentYLabel(c, step, detentYStep, labelW)),
	)
	drawPane(c, &tslc, specs, buckets, times, dim)
	return &tslc
}

// drawPane fills a built pane's plot rectangle with the active treatment's ink
// (ticket #65). Everything above it - the canvas, the ranges, the axes, the ring
// labels, the shared gutter - is identical whichever treatment runs, so a flip
// changes the line and nothing else.
func drawPane(c Ctx, tslc *timeserieslinechart.Model, specs []CompSpec,
	buckets []store.Bucket, times []time.Time, dim string) {
	switch c.Trend {
	case TrendSmooth, TrendRidge:
		// The ridge only mirrors the FRESH pane; every other pane it touches
		// renders as candidate A, which is the treatment it is a variation of.
		drawSmoothBraille(tslc, paneSeriesFor(specs, buckets, times, dim))
	case TrendColumns:
		drawBlockColumns(tslc, paneSeriesFor(specs, buckets, times, dim), bucketStep(dim))
	default:
		drawCurrentBraille(tslc, specs, buckets, times, dim)
	}
}

// drawCurrentBraille is the shipped renderer, kept reachable as the prototype's
// baseline: one dataset per contiguous run per series, handed to ntcharts. A
// braille line only joins points inside a single dataset, so a missing bucket
// breaks the line instead of drawing an interpolated diagonal across the outage.
func drawCurrentBraille(tslc *timeserieslinechart.Model, specs []CompSpec,
	buckets []store.Bucket, times []time.Time, dim string) {
	runs := gapRuns(times, dim)
	order := make([]string, 0, len(specs)*len(runs))
	for _, s := range specs {
		for ri, run := range runs {
			name := s.Key + ":" + strconv.Itoa(ri)
			tslc.SetDataSetStyle(name, s.Style())
			for _, i := range run {
				tslc.PushDataSet(name, timeserieslinechart.TimePoint{
					Time: times[i], Value: float64(s.Pick(Split(buckets[i])))})
			}
			order = append(order, name)
		}
	}
	tslc.DrawBrailleDataSets(order)
}

// buildHeatFrame builds candidate D: ONE widget carrying the shared time axis,
// with three self-scaled heat lanes written into its plot rectangle.
//
// It asks for no Y gutter (yStep 0), so the lanes run the full width and the
// pane owns every row above the axis. Everything the other treatments get from
// ntcharts still applies: the x axis and its labels are drawn by the widget, the
// scrub crosshair and the now column stay a canvas post-pass, and the render
// memo retains it exactly as it retains a braille pane.
func buildHeatFrame(c Ctx, buckets []store.Bucket, times []time.Time, dim string, w, h int) (*heroFrame, bool) {
	if len(c.Comp) == 0 {
		return nil, false
	}
	minT, maxT := times[0], times[len(times)-1]
	if !maxT.After(minT) {
		maxT = minT.Add(bucketStep(dim))
	}
	axis := lipgloss.NewStyle().Foreground(c.FaintColor)
	label := lipgloss.NewStyle().Foreground(c.FaintColor)
	tslc := timeserieslinechart.New(w, h,
		timeserieslinechart.WithTimeRange(minT, maxT),
		timeserieslinechart.WithYRange(0, 1),
		timeserieslinechart.WithXYSteps(heroXSteps, 0),
		timeserieslinechart.WithAxesStyles(axis, label),
		timeserieslinechart.WithXLabelFormatter(xLabelFormatter(dim)),
	)
	if !drawHeatLanes(c, &tslc, c.Comp, buckets, times, dim) {
		return nil, false
	}
	// The lane rules carry their own glyphs, names and peaks, so this frame needs
	// no pane header of its own; an empty header claims no row.
	return &heroFrame{
		panes: []heroPane{{model: &tslc, h: h}},
		times: times,
		h:     h,
	}, true
}

// ridgePane builds candidate C's fresh pane: the same detented widget, with the
// ring labels read as distance from a centerline and the two series drawn away
// from it in opposite directions. It carries no x axis, exactly as the flat
// fresh pane does not.
func ridgePane(c Ctx, buckets []store.Bucket, times []time.Time, dim string,
	specs []CompSpec, w, h int, step int64, r ridgeGeom, labelW int) *timeserieslinechart.Model {
	minT, maxT := times[0], times[len(times)-1]
	if !maxT.After(minT) {
		maxT = minT.Add(bucketStep(dim))
	}
	axis := lipgloss.NewStyle().Foreground(c.FaintColor)
	label := lipgloss.NewStyle().Foreground(c.FaintColor)
	tslc := timeserieslinechart.New(w, h,
		timeserieslinechart.WithTimeRange(minT, maxT),
		timeserieslinechart.WithYRange(0, detentViewMax(step, h, detentYStep)),
		// A Y step of 1 hands the formatter every row: the rings are measured out
		// from the centerline rather than from the baseline, so the ridge is free
		// to sit on the pane's true middle row (see newRidgeGeom).
		timeserieslinechart.WithXYSteps(0, 1),
		timeserieslinechart.WithAxesStyles(axis, label),
		timeserieslinechart.WithXLabelFormatter(xLabelFormatter(dim)),
		timeserieslinechart.WithYLabelFormatter(ridgeYLabel(c, step, r, labelW)),
	)
	drawSplitRidge(&tslc, paneSeriesFor(specs, buckets, times, dim), r, step)
	return &tslc
}

// buildLeverageFrame builds the pivot: cache-read / input as one ratio line on
// a detented linear axis, over the magnitude footer. There is no 1x break-even
// reference — at the 40-310x ratios this data actually shows, a line at 1 is
// geometrically indistinguishable from the axis.
func buildLeverageFrame(c Ctx, buckets []store.Bucket, times []time.Time, dim string, w, h int, lock *detentLock) (*heroFrame, bool) {
	segs, maxRatio := leverageSegments(c, buckets, times, dim)
	if len(segs) == 0 {
		return nil, false
	}
	chartH := h - 2 // header + footer
	graphH := chartH - 2
	if graphH < minPaneGraphH {
		return nil, false
	}

	step := lock.pick("leverage", graphH, detentYStep, int64(math.Ceil(maxRatio)))
	labelW := leverageLabelWidth(step, graphH)

	minT, maxT := times[0], times[len(times)-1]
	if !maxT.After(minT) {
		maxT = minT.Add(bucketStep(dim))
	}
	axis := lipgloss.NewStyle().Foreground(c.FaintColor)
	label := lipgloss.NewStyle().Foreground(c.FaintColor)
	line := leverageLineStyle(c)
	tslc := timeserieslinechart.New(w, chartH,
		timeserieslinechart.WithTimeRange(minT, maxT),
		timeserieslinechart.WithYRange(0, detentViewMax(step, graphH, detentYStep)),
		timeserieslinechart.WithXYSteps(heroXSteps, detentYStep),
		timeserieslinechart.WithAxesStyles(axis, label),
		timeserieslinechart.WithXLabelFormatter(xLabelFormatter(dim)),
		timeserieslinechart.WithYLabelFormatter(leverageYLabel(step, detentYStep, labelW)),
	)
	order := make([]string, 0, len(segs))
	for i, seg := range segs {
		name := "lev:" + strconv.Itoa(i)
		tslc.SetDataSetStyle(name, line)
		for _, p := range seg {
			tslc.PushDataSet(name, timeserieslinechart.TimePoint{Time: p.t, Value: p.v})
		}
		order = append(order, name)
	}
	tslc.DrawBrailleDataSets(order)
	return &heroFrame{
		panes:  []heroPane{{header: leverageHeader(c, step, w), model: &tslc, h: chartH}},
		footer: leverageFooter(c, buckets, w),
		times:  times,
		h:      h,
	}, true
}

// leverageYLabel is detentYLabel for the ratio axis: same exact-multiple
// derivation, rendered as a multiple instead of a token count.
func leverageYLabel(step int64, yStep, labelW int) linechart.LabelFormatter {
	return func(i int, _ float64) string {
		if yStep < 1 || i%yStep != 0 {
			return ""
		}
		return padLeftLocal(leverageRatioLabel(int64(i/yStep)*step), labelW)
	}
}

// emptyChartFrame renders the centered no-rows treatment for an empty chart
// pane, sized to w×h, with a range-change hint underneath.
func emptyChartFrame(c Ctx, w, h int) string {
	block := EmptyState(c, EmptyNoRows, w-2)
	if h >= 2 {
		sub := "press t to change range"
		if c.Truncate != nil {
			sub = c.Truncate(sub, w-2)
		}
		block = lipgloss.JoinVertical(lipgloss.Center, block, c.Faint.Render(sub))
	}
	box := lipgloss.NewStyle().Width(w).Height(h).
		Align(lipgloss.Center, lipgloss.Center)
	return box.Render(block)
}
