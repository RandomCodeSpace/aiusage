package views

import (
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

// hero.go composes the Overview hero: the detented two-pane trend that is THE
// hero (issue #8), and the leverage-ratio pivot it toggles to. Everything here
// is geometry, series partitioning and plain text — the ntcharts widgets
// themselves are built behind the chartstyle.go seam (issue #13).
//
// Two panes rather than one axis because the spread is real: cache-read runs
// ~100x input/output, so one shared linear axis flattens fresh tokens onto the
// baseline and one shared log axis makes every series look the same size. Each
// pane therefore carries its own quantized scale and prints it as text
// ("SCALE 200M/div"), which is the channel that survives monochrome.

// HeroMode selects which reading of the trend the hero renders.
type HeroMode int

const (
	// HeroTrend is the detented two-pane hero: fresh (input+output) above
	// cache, each pane on its own quantized linear scale. The default.
	HeroTrend HeroMode = iota
	// HeroLeverage is the pivot: cache-read / input per bucket as one ratio
	// line. It answers "is caching working", not "how much".
	HeroLeverage
)

const (
	// minHeroTwoPaneH is the body height below which the two panes cannot both
	// carry a readable plot area (2 header rows + a >=5-row fresh pane + a
	// >=6-row cache pane, 2 of which are the shared x axis).
	minHeroTwoPaneH = 13

	// minHeroPivotH is the body height below which the ratio chart degrades to
	// its sparkline row (1 header + >=4 plot rows + 2 axis rows + 1 footer).
	minHeroPivotH = 8

	// minHeroLogH is the body height at which the small-terminal band still
	// affords a headed, axed chart: 1 header + >=2 plot rows + 2 axis rows.
	// Between it and minHeroTwoPaneH the hero renders the quantized decade-ring
	// log (issue #39), which is a full braille build and therefore a memoized
	// frame rather than a cheap strip.
	minHeroLogH = 5

	// heroCardChromeH is what the hero's card costs vertically: the design's two
	// card padding rows plus the titled rule. heroPanel spends exactly this
	// before handing the rest to the body.
	heroCardChromeH = 2*blockPadY + 1

	// minHeroPanelH is the shortest hero PANEL that can still carry a BUILT
	// chart: the card chrome over the decade band's own floor. Overview reserves
	// it for the hero before the KPI strip is allowed to claim rows (issue #48).
	minHeroPanelH = heroCardChromeH + minHeroLogH

	// minPaneGraphH is the smallest plot area worth handing a pane.
	minPaneGraphH = 4

	// heroXSteps is the x-label pitch on the pane that carries the shared axis.
	heroXSteps = 4

	// leverageFloorPerDay is the DEFAULT per-bucket input below which the ratio
	// is noise rather than leverage: a 9M cache read against a 50K input is not
	// 180x of working leverage, it is a nearly idle day. Such buckets break the
	// line.
	//
	// It is a rate, not a constant, because a bucket's plausible input scales
	// with its width — one number cannot be right for an hour bucket and a
	// month bucket at once (issue #39). 200K per day of bucket span keeps the
	// day figure the pivot shipped with. Ctx.LeverageFloor overrides it
	// outright when the user has an opinion about their own spend.
	leverageFloorPerDay = 200_000
)

// heroPanel renders the hero panel: title chip, then the mode's body. The title
// is built additively — each optional chip is appended only while it still fits
// the panel's inner width — so a long title can never wrap and silently eat a
// row from the chart's budget.
func heroPanel(c Ctx, d OverviewData, w, h int, lay Layout, focus bool) string {
	// The hero's card stays on the ground step: its body is an ntcharts canvas,
	// rendered one cell at a time with per-cell styles, so a panel background
	// cannot reach the cells between the braille without a per-cell repaint pass
	// on every scrub. Charts live on the ground plane by design (surface.go);
	// focus is carried by the titled rule's focus bar, which needs no paint.
	style := c.Block(ElevGround).Width(w)
	inner := w - cardChromeW
	if inner < 4 {
		inner = 4
	}
	chartH := h - heroCardChromeH // title rule + card padding
	if chartH < 1 {
		chartH = 1
	}

	title := c.titleChip("TREND", focus)
	add := func(chip string) {
		if lipgloss.Width(title)+2+lipgloss.Width(chip) <= inner {
			title += "  " + chip
		}
	}
	if d.Mode == HeroLeverage {
		add(c.Subtle.Render("leverage · cache-read / input"))
	}
	if !heroUsesFrame(d.Mode, lay, inner, chartH) && d.Mode == HeroTrend {
		// Fallback body: the degraded chart has no per-series pane headers, so
		// the title carries the legend instead.
		add(c.CompLegend())
	}
	if d.ScrubLabel != "" {
		add(c.now().Render("◷ " + d.ScrubLabel))
	}

	scrubIdx := -1
	if d.Pinned {
		scrubIdx = d.Cursor
	}
	// The rule runs out from whatever chips the title ended up carrying, so the
	// pane's extent is drawn even in the header-less degraded bands.
	return style.Render(c.Rule(title, inner) + "\n" +
		heroBodyMemo(c, d, lay, inner, chartH, scrubIdx))
}

// heroFrameKind is which built body a (mode, layout, geometry) triple resolves
// to. Every kind other than heroFrameNone constructs braille and therefore must
// go through the render memo.
type heroFrameKind int

const (
	// heroFrameNone: no braille fits; the body is a cheap strip or numbers.
	heroFrameNone heroFrameKind = iota
	// heroFrameTwoPane: the detented fresh-over-cache hero.
	heroFrameTwoPane
	// heroFrameLeverage: the ratio pivot's axed chart.
	heroFrameLeverage
	// heroFrameLog: the degraded band — too short for two detented panes, still
	// tall enough for the pre-hero log chart (see the SEAM note below).
	heroFrameLog
)

// heroFrameFor resolves the body kind for a mode at (w, h) under lay. It is the
// single gate: heroPanel asks it (through heroUsesFrame) to decide the title,
// and heroBodyMemo asks it to pick the builder.
//
// w is the pane's POST-CHROME inner width, so the width budget is charged
// exactly once: lay.ChartMode was granted on a column width derived from the
// same minChartInnerW this compares against (issue #48). Testing the outer
// gate's constant here spent the same cells twice and left a band of widths
// (80 columns, the classic terminal, among them) that passed the layout gate
// and failed this one.
func heroFrameFor(mode HeroMode, lay Layout, w, h int) heroFrameKind {
	if lay.ChartMode != ChartFull || w < minChartInnerW {
		return heroFrameNone
	}
	if mode == HeroLeverage {
		if h >= minHeroPivotH {
			return heroFrameLeverage
		}
		return heroFrameNone
	}
	switch {
	case h >= minHeroTwoPaneH:
		return heroFrameTwoPane
	case h >= minHeroLogH:
		return heroFrameLog
	}
	return heroFrameNone
}

// heroUsesFrame reports whether the mode renders a HEADED pane at the given
// body. Every built kind does now that the small-terminal band carries its own
// glyphs + SCALE header (issue #39); only the strip/numbers fallbacks leave the
// legend for the title to carry.
func heroUsesFrame(mode HeroMode, lay Layout, w, h int) bool {
	return heroFrameFor(mode, lay, w, h) != heroFrameNone
}

// heroBodyMemo renders the hero body, routing the expensive braille build
// through the shared render memo when the root model wired one (d.Memo). The
// memo covers EVERY braille body, including the degraded log band — that band
// renders a full ntcharts build, so leaving it direct would rebuild the chart
// on every View and every scrub keypress. Only the strips (sparkline rows,
// numbers) are cheap enough to stay direct.
func heroBodyMemo(c Ctx, d OverviewData, lay Layout, w, h, scrubIdx int) string {
	if len(d.Timeline) == 0 {
		// Empty and error states keep their existing treatments.
		return heroBody(c, d.Timeline, d.TimelineDim, lay, w, h, scrubIdx)
	}
	if kind := heroFrameFor(d.Mode, lay, w, h); kind != heroFrameNone {
		if d.Memo != nil {
			if s, ok := d.Memo.frame(c, d.Gen, d.Timeline, d.TimelineDim, kind, w, h, scrubIdx); ok {
				return s
			}
		} else if f, ok := buildHeroFrame(c, d.Timeline, d.TimelineDim, kind, w, h, nil); ok {
			return f.render(c, scrubIdx)
		}
	}
	if d.Mode == HeroLeverage {
		return leverageFallback(c, d.Timeline, d.TimelineDim, w, h)
	}
	// Below the decade band's own floor there is not even one ring pitch of
	// plot area left, so the hero degrades to the cheap treatments: a per-series
	// sparkline strip, then per-series numbers. Never a total.
	return heroBody(c, d.Timeline, d.TimelineDim, lay, w, h, scrubIdx)
}

// heroPaneSplit divides chartTotal rows between the fresh and cache panes. The
// cache pane pays two rows for the shared x axis, so the remainder is split
// evenly and the fresh pane is then nudged to an ODD height.
//
// The nudge is the "one row of headroom" the prototype called for. ntcharts
// draws the label for graph row i at canvas row origin.Y-i; on the axis-less
// pane origin.Y is height-1 while the graph height is the full height, so the
// top ring (i == graphHeight) lands one row above the canvas and is clipped. An
// odd height moves the highest visible ring onto the top row instead of leaving
// it blank.
func heroPaneSplit(chartTotal int) (freshH, cacheH int) {
	freshH = (chartTotal - 2) / 2
	if freshH%2 == 0 {
		freshH++
	}
	for freshH > 3 && chartTotal-freshH-2 < minPaneGraphH {
		freshH -= 2
	}
	return freshH, chartTotal - freshH
}

// heroPaneSeries splits the component descriptor into the two panes: cache on
// its own, input+output together as "fresh". The locked three-series /
// no-totals decision is what makes this a clean cut.
func heroPaneSeries(c Ctx) (fresh, cache []CompSpec) {
	for _, s := range c.Comp {
		if s.Key == "cache" {
			cache = append(cache, s)
			continue
		}
		fresh = append(fresh, s)
	}
	return fresh, cache
}

// seriesMax is the largest value any of specs takes across buckets — the peak a
// pane's scale has to cover.
func seriesMax(buckets []store.Bucket, specs []CompSpec) int64 {
	var max int64
	for _, b := range buckets {
		comp := Split(b)
		for _, s := range specs {
			if v := s.Pick(comp); v > max {
				max = v
			}
		}
	}
	return max
}

// bucketStep is the nominal spacing between adjacent buckets for a grouping dim.
func bucketStep(dim string) time.Duration {
	switch dim {
	case "hour":
		return time.Hour
	case "week":
		return 7 * 24 * time.Hour
	case "month":
		return 30 * 24 * time.Hour
	default: // day
		return 24 * time.Hour
	}
}

// gapThreshold is the spacing beyond which a series must break rather than
// interpolate a diagonal across missing buckets: one and a half bucket steps,
// which no adjacent pair can reach and no missing bucket can stay under.
func gapThreshold(dim string) time.Duration { return bucketStep(dim) * 3 / 2 }

// gapRuns splits bucket indices into contiguous runs; a new run starts wherever
// the time step exceeds the dim's gap threshold. Each run becomes its own
// dataset, which is what keeps a line from bridging an outage.
func gapRuns(times []time.Time, dim string) [][]int {
	if len(times) == 0 {
		return nil
	}
	thr := gapThreshold(dim)
	var runs [][]int
	run := []int{0}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) > thr {
			runs = append(runs, run)
			run = nil
		}
		run = append(run, i)
	}
	return append(runs, run)
}

// paneHeader renders one pane's plain-text header: the series glyphs, the pane
// name, and the declared scale. scale is the readout's magnitude only ("5M",
// "10^2"); the SCALE/div framing is added here so every detented pane — linear
// or decade-ring — declares itself in the same words. The readout is text, so
// it survives monochrome and screen readers alike.
func paneHeader(c Ctx, specs []CompSpec, name, scale string, w int) string {
	var glyphs strings.Builder
	for _, s := range specs {
		glyphs.WriteString(c.compStyle(s).Render(s.Glyph))
	}
	head := glyphs.String() + c.pad(1) + c.StatLabel.Render(name)
	tail := c.Subtle.Render("SCALE " + scale + "/div")
	if lipgloss.Width(head)+3+lipgloss.Width(tail) <= w {
		return c.RuleBetween(head, tail, w)
	}
	return head
}

// defaultLeverageFloor is the per-bucket input floor derived from the bucket
// span: leverageFloorPerDay scaled by how much of a day one bucket covers.
// Hour buckets therefore ask for ~8K rather than the 200K a day bucket does,
// which is the difference between the pivot working on the "today" range and
// suppressing all of it.
func defaultLeverageFloor(dim string) int64 {
	hours := int64(bucketStep(dim) / time.Hour)
	if hours < 1 {
		hours = 1
	}
	if n := hours * leverageFloorPerDay / 24; n > 1 {
		return n
	}
	return 1
}

// leverageFloor resolves the per-bucket input floor for a grouping: the
// configured value when the user set one, otherwise the bucket span's default.
func (c Ctx) leverageFloor(dim string) int64 {
	if c.LeverageFloor > 0 {
		return c.LeverageFloor
	}
	return defaultLeverageFloor(dim)
}

// leveragePoint is one bucket's cache-read / input ratio.
type leveragePoint struct {
	t time.Time
	v float64
}

// leverageSegments splits the ratio series into drawable segments and reports
// the peak ratio. A segment breaks on a bucket below the resolved input floor
// and on a time gap wider than the dim's threshold, so neither an idle day nor
// an outage renders as a slope.
func leverageSegments(c Ctx, buckets []store.Bucket, times []time.Time, dim string) ([][]leveragePoint, float64) {
	thr := gapThreshold(dim)
	floor := c.leverageFloor(dim)
	var (
		segs [][]leveragePoint
		cur  []leveragePoint
		max  float64
	)
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, cur)
			cur = nil
		}
	}
	for i, b := range buckets {
		if b.Input < floor {
			flush()
			continue
		}
		r := float64(b.CacheRead) / float64(b.Input)
		if len(cur) > 0 && times[i].Sub(cur[len(cur)-1].t) > thr {
			flush()
		}
		cur = append(cur, leveragePoint{t: times[i], v: r})
		if r > max {
			max = r
		}
	}
	flush()
	return segs, max
}

// leverageLineStyle colors the ratio line with the cache series color (the
// ratio is about cache doing input's work); the accent stays reserved for
// interaction.
func leverageLineStyle(c Ctx) lipgloss.Style {
	for _, s := range c.Comp {
		if s.Key == "cache" {
			return s.Style()
		}
	}
	return lipgloss.NewStyle().Foreground(c.AccentColor)
}

// leverageRatioLabel renders a ratio tick as a multiple ("160x"). Ratios are
// plain integers, never humanized: "1.0Kx" reads as a token count, not a rate.
func leverageRatioLabel(n int64) string { return strconv.FormatInt(n, 10) + "x" }

// leverageLabelWidth is the Y-gutter width for the ratio axis.
func leverageLabelWidth(step int64, graphH int) int {
	w := 0
	for k := 0; k <= graphH/detentYStep; k++ {
		if n := len(leverageRatioLabel(int64(k) * step)); n > w {
			w = n
		}
	}
	return w
}

// leverageHeader is the pivot's pane header: the line's glyph, what it plots,
// and the declared scale.
func leverageHeader(c Ctx, step int64, w int) string {
	head := leverageLineStyle(c).Render("⠒") + c.pad(1) + c.StatLabel.Render("leverage")
	scale := c.Subtle.Render("SCALE " + leverageRatioLabel(step) + "/div")
	if lipgloss.Width(head)+3+lipgloss.Width(scale) <= w {
		return c.RuleBetween(head, scale, w)
	}
	return head
}

// leverageFooter is the raw-magnitude strip under the ratio chart: the range's
// overall leverage plus each series' range total. The chart deliberately
// carries no magnitudes, so the footer is where they live. Parts drop from the
// right until the line fits.
func leverageFooter(c Ctx, buckets []store.Bucket, w int) string {
	var tot Components
	for _, b := range buckets {
		s := Split(b)
		tot.Input += s.Input
		tot.Output += s.Output
		tot.CacheRead += s.CacheRead
		tot.CacheCreation += s.CacheCreation
	}
	parts := make([]string, 0, len(c.Comp)+1)
	if tot.Input > 0 {
		overall := tot.CacheRead / tot.Input
		parts = append(parts, c.Subtle.Render("range ")+
			leverageLineStyle(c).Render(leverageRatioLabel(overall)))
	}
	for _, s := range c.Comp {
		parts = append(parts, s.Style().Render(s.Glyph+" "+s.Short+" ")+
			c.Number.Render(humanizeOr(c, s.Pick(tot))))
	}
	sep := c.Subtle.Render("  ·  ")
	line := strings.Join(parts, sep)
	for lipgloss.Width(line) > w && len(parts) > 1 {
		parts = parts[:len(parts)-1]
		line = strings.Join(parts, sep)
	}
	return line
}

// leverageFloorLabel names the RESOLVED per-bucket input floor for display, so
// the message can never quote a number the segments were not filtered against.
// It prefers the injected humanizer and falls back to a plain K form, because
// headless renders construct partial Ctx values with no humanizer.
func leverageFloorLabel(c Ctx, dim string) string {
	floor := c.leverageFloor(dim)
	if s := humanizeOr(c, floor); s != "" {
		return s
	}
	return strconv.FormatInt(floor/1000, 10) + "K"
}

// leverageBelowFloor is the pivot's treatment when the range HAS buckets but
// none clears the input floor: every ratio was suppressed as noise, which is a
// different fact from an empty range. The no-rows panel would be wrong twice
// over here — the rows exist, and widening the range is not the fix — so the
// panel states what happened and keeps the magnitude footer, which needs no
// floor to be true.
func leverageBelowFloor(c Ctx, buckets []store.Bucket, dim string, w, h int) string {
	note := c.Faint.Render(truncTo(c, "⊘ leverage skipped: no bucket over "+leverageFloorLabel(c, dim)+" input", w))
	rows := make([]string, 0, h)
	if h >= 3 {
		// Sit the note in the middle of the rows the footer does not claim.
		for i := 0; i < (h-2)/2; i++ {
			rows = append(rows, "")
		}
	}
	rows = append(rows, note)
	if h >= 2 {
		for len(rows) < h-1 {
			rows = append(rows, "")
		}
		rows = append(rows, leverageFooter(c, buckets, w))
	}
	return fitHeight(strings.Join(rows, "\n"), h)
}

// leverageFallback is the pivot below its chart floor: the ratio series as one
// self-scaled heat row (order only, no axis) over the magnitude footer, so the
// toggle stays meaningful at sizes an axed chart cannot fill.
func leverageFallback(c Ctx, buckets []store.Bucket, dim string, w, h int) string {
	if len(buckets) == 0 {
		return emptyChartFrame(c, w, h)
	}
	floor := c.leverageFloor(dim)
	vals := make([]float64, 0, len(buckets))
	for _, b := range buckets {
		if b.Input >= floor {
			vals = append(vals, float64(b.CacheRead)/float64(b.Input))
		}
	}
	if len(vals) == 0 {
		return leverageBelowFloor(c, buckets, dim, w, h)
	}
	rows := []string{heatStrip(c, vals, w, heatPeak(vals), heatConstInk(leverageLineStyle(c)))}
	if h >= 2 {
		rows = append(rows, leverageFooter(c, buckets, w))
	}
	return fitHeight(strings.Join(rows, "\n"), h)
}
