package views

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// OverviewData is everything the Overview view needs to render one frame. The
// hero/sparkline series are derived from the day/hour timeline; the side bars
// from the by-tool grouping. When the scrub crosshair is pinned, ScrubBucket /
// ScrubByTool carry that single instant's values and the KPI tiles re-price.
type OverviewData struct {
	Totals      store.Bucket   // grand total for the active range (or scrubbed bucket)
	Prev        store.Bucket   // prior equal-length period, for deltas
	ByTool      []store.Bucket // grouped by tool (sorted desc) — full range or scrubbed
	Timeline    []store.Bucket // day/hour buckets ascending (hero + sparklines)
	TimelineDim string         // "day" or "hour"
	RangeLbl    string
	ActivePane  int        // which pane wears the focus ring (see PaneOverview*)
	ScrubLabel  string     // non-empty when scrub is pinned: the bucket timestamp
	Cursor      int        // highlighted timeline bucket index (scrub crosshair)
	Pinned      bool       // whether the scrub is pinned (crosshair renders only then)
	Sys         []SysGauge // container CPU/mem/disk gauges (empty → strip omitted)
	Mode        HeroMode   // which reading the hero renders (trend / leverage pivot)

	// Render memoization (issue #4, 1f): Gen identifies the applied dataset and
	// Memo caches the built hero chart + KPI sparklines across frames. A nil
	// Memo renders everything directly (headless views tests).
	Gen  uint64
	Memo *HeroMemo
}

// Overview view panes (focus order; pane 0 = rail is owned by the root frame).
const (
	PaneOverviewKPIs = iota
	PaneOverviewHero
	PaneOverviewTools
)

// heroReserveH is the one body row Overview holds back from the hero: the
// breathing line under the panels. It is counted in the strip's budget so the
// reserve cannot be spent twice.
const heroReserveH = 1

// minHeroBodyH is the smallest Overview BODY that can host a BUILT hero chart
// once the KPI strip has yielded everything it can: one tile row (the strip's
// floor - an Overview with no numbers at all would be a worse trade than a
// short hero), the reserve row, and minHeroPanelH for the panel itself. A body
// shorter than this renders the hero's strip fallback whatever the width is;
// that is a documented floor, not an emergent accident, and
// TestOverviewHeroFloorIsPinned holds it in place. The sys gauge strip and (on
// narrow bodies) the compact by-tool card cost their own rows on top of it.
const minHeroBodyH = kpiTileH + heroReserveH + minHeroPanelH

// Overview renders the calm landing hub: a KPI strip, a hero time-series, and a
// side by-tool stacked bar panel over a fresh/cache split gauge. The layout is
// fully driven by lay: KPI columns reflow to the body width, the side panel
// appears only when lay grants it, and the hero degrades to a sparkline when
// there isn't room for a full chart.
//
// The hero outranks the KPI strip (issue #48). Rows are handed out in that
// order: the sys strip and the narrow-body tool card first (both fixed), then
// minHeroPanelH is RESERVED for the hero, and the KPI strip fills what is left,
// dropping trailing tile rows rather than taking rows off the chart. This is
// the same rule that already forbids the cost tile from costing the hero a row,
// applied to the whole strip.
func Overview(c Ctx, d OverviewData, lay Layout) string {
	width := lay.BodyW
	if width < 8 {
		width = 8
	}

	// Container resource gauges sit at the very top as a thin one-row strip; it
	// costs one body line and is omitted when no gauges are supplied.
	strip := SysStrip(c, d.Sys, width)
	stripH := 0
	if strip != "" {
		stripH = lipgloss.Height(strip)
	}

	// On a body with no side panel the by-tool card sits UNDER the hero, so its
	// rows are part of the hero's budget. Measuring it here (instead of letting
	// it overflow and be clamped) is what keeps the card whole now that the hero
	// can grow into a chart at these widths.
	tools, toolsH := "", 0
	if !lay.SidePanel {
		tools = compactToolStrip(c, d, width)
		toolsH = lipgloss.Height(tools)
	}

	avail := lay.BodyH - stripH - toolsH - heroReserveH
	kpis := overviewKPIs(c, d, lay, avail-minHeroPanelH)
	kpisH := lipgloss.Height(kpis)
	bodyH := avail - kpisH
	if bodyH < 3 {
		bodyH = 3
	}

	head := func(rest ...string) string {
		parts := make([]string, 0, len(rest)+2)
		if strip != "" {
			parts = append(parts, strip)
		}
		parts = append(parts, kpis)
		parts = append(parts, rest...)
		return lipgloss.JoinVertical(lipgloss.Left, parts...)
	}

	if !lay.SidePanel {
		hero := heroPanel(c, d, width, bodyH, lay, d.ActivePane == PaneOverviewHero)
		return head(hero, tools)
	}

	hero := heroPanel(c, d, lay.MainW, bodyH, lay, d.ActivePane == PaneOverviewHero)
	side := sidePanel(c, d, lay.SideW, bodyH, d.ActivePane == PaneOverviewTools)
	main := lipgloss.JoinHorizontal(lipgloss.Top, hero, " ", side)
	return head(main)
}

// kpiSpec describes one read-only KPI tile.
type kpiSpec struct {
	label, foot        string
	value, prev        int64
	spark              string // pre-rendered heat-strip row; "" → none (total/events are never graphed)
	style              lipgloss.Style
	shareVal, shareTot int64 // shareTot>0 shows a share %
	// fmtVal overrides the default token humanizer. Cost is not a token count,
	// so it carries its own renderer rather than being humanized into a bare
	// number with no currency.
	fmtVal func(int64) string
}

// kpiTileH is the tallest a KPI tile renders: the card's two padding rows over
// the titled rule, the number row, a sparkline row and the foot. It is what one
// tile row of the strip costs, and therefore the strip's floor - the strip
// yields rows to the hero but never vanishes. TestKPITileHeightMatchesTheBudget
// keeps the constant honest against what kpiTile actually renders.
const kpiTileH = 2*blockPadY + 4

// overviewKPIs renders the read-only KPI strip: one tile per token component
// (input, output, cache-read, cache-creation) with a self-scaled heat strip and
// its share of the component sum, then total and events as bare numbers (total
// is never graphed). Tiles reflow to fit the body width.
//
// maxRows is the strip's row budget - what the body has left once the hero's
// floor is reserved. Tile rows are added while they fit it; the rest of the
// tiles are dropped, trailing ones first, which is why the order (components,
// then total/events, then cost) puts the numbers the hero is about at the
// front. The first row is always kept even when it does not fit: that floor is
// minHeroBodyH's kpiTileH term.
func overviewKPIs(c Ctx, d OverviewData, lay Layout, maxRows int) string {
	width := lay.BodyW
	tot := Split(d.Totals)
	prev := Split(d.Prev)
	sum := tot.Sum()

	// Tile geometry first: the heat strip's width is needed while building specs
	// (memoized strips key on their rendered width). How many tiles fit
	// across, given a minimum useful tile width + 1-col gutters.
	nspec := len(c.Comp) + 2
	// The cost tile is counted here even though it may not survive the fit check
	// below: the per-row cap is what decides whether a sixth tile can share the
	// existing row or has to wrap into a new one, so excluding it would wrap it
	// at every width and it would never appear at all.
	if c.Money != nil {
		nspec++
	}
	const minTileW = 16
	per := (width + 1) / (minTileW + 1)
	if per > nspec {
		per = nspec
	}
	if per < 1 {
		per = 1
	}
	tileW := (width - (per - 1)) / per // total tile width (lipgloss v2 sizes are border-inclusive)
	if tileW < 16 {
		tileW = 16
	}
	// Content width inside a tile: border (2) + Padding(0,1) (2). Must mirror
	// kpiTile's cw clamp so the sparkline fills the tile exactly.
	cw := tileW - 4
	if cw < 6 {
		cw = 6
	}

	specs := make([]kpiSpec, 0, nspec)
	for _, s := range c.Comp {
		spark := ""
		if lay.Sparklines {
			spark = kpiHeatStrip(c, d, s, cw)
		}
		specs = append(specs, kpiSpec{
			label:    s.Short,
			value:    s.Pick(tot),
			prev:     s.Pick(prev),
			spark:    spark,
			style:    s.Style(),
			shareVal: s.Pick(tot),
			shareTot: sum,
		})
	}
	specs = append(specs,
		kpiSpec{label: "total", foot: "tokens", value: d.Totals.Total, prev: d.Prev.Total, style: c.Subtle},
		kpiSpec{label: "events", foot: "requests", value: d.Totals.Events, prev: d.Prev.Events, style: c.Subtle},
	)
	// The cost tile rides along only when it fits in the rows the strip already
	// occupies. It is the newest tile and the hero is the reason the screen
	// exists: at 90 columns a sixth tile wraps to a second row and takes five
	// rows off the chart, which is too much to pay for a number `summary`
	// prints. Where there is slack — 120 columns and up, the common case — it
	// costs nothing.
	if c.Money != nil && (len(specs)+1+per-1)/per == (len(specs)+per-1)/per {
		// A range holding rows nothing could price understates the bill, so the
		// figure is marked approximate rather than presented as exact. The foot
		// says which of the two the tile is showing.
		approx := d.Totals.UnpricedEvents > 0
		// Nothing in range could be priced: show the unpriced mark, not a zero.
		known := d.Totals.CostMicroUSD > 0 || d.Totals.UnpricedEvents == 0
		foot := "spend"
		if approx {
			foot = "spend (partial)"
		}
		specs = append(specs, kpiSpec{
			label:  "cost",
			foot:   foot,
			value:  d.Totals.CostMicroUSD,
			prev:   d.Prev.CostMicroUSD,
			style:  c.Subtle,
			fmtVal: func(v int64) string { return c.Money(v, approx, known) },
		})
	}

	tiles := make([]string, len(specs))
	for i, s := range specs {
		tiles[i] = kpiTile(c, s, tileW)
	}

	// Arrange the tiles into rows of `per`, keeping only the rows the budget can
	// pay for.
	var rows []string
	used := 0
	for i := 0; i < len(tiles); i += per {
		end := i + per
		if end > len(tiles) {
			end = len(tiles)
		}
		segs := make([]string, 0, (end-i)*2)
		for j := i; j < end; j++ {
			if j > i {
				segs = append(segs, " ")
			}
			segs = append(segs, tiles[j])
		}
		row := lipgloss.JoinHorizontal(lipgloss.Top, segs...)
		rh := lipgloss.Height(row)
		if len(rows) > 0 && used+rh > maxRows {
			break
		}
		rows = append(rows, row)
		used += rh
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// kpiHeatStrip renders one KPI tile's self-scaled heat row, through the render
// memo when one is wired (the series derives only from the timeline, so it is
// stable for the lifetime of an applied dataset).
//
// Self-scaling is right here: a token series has no ceiling to read against, so
// the question a tile answers is shape over time - the same reason the hero's
// lanes scale per lane. The memo key stays (series, width): everything else the
// row is built from - the ramp, the series color, the card's elevation - is
// static for the life of the process.
func kpiHeatStrip(c Ctx, d OverviewData, s CompSpec, w int) string {
	build := func() string {
		vals := SeriesFor(d.Timeline, func(b store.Bucket) int64 { return s.Pick(Split(b)) })
		// The row is painted on the tile's card step, the same one kpiTile renders
		// at: the ramp's lighter rungs and the track mark do not cover their cell,
		// so an unpainted strip would punch holes in the card.
		cc := c.On(ElevCard)
		return heatStrip(cc, vals, w, heatPeak(vals), heatConstInk(cc.compStyle(s)))
	}
	if d.Memo == nil {
		return build()
	}
	return d.Memo.spark(d.Gen, d.Timeline, s.Key, w, build)
}

// kpiTile renders one read-only KPI bento card: title on the border, big
// right-aligned number + delta chip, an optional self-scaled heat strip, then a
// share % or unit. KPI tiles are not interactive (the trend is the only
// interactive surface on Overview).
func kpiTile(c Ctx, s kpiSpec, w int) string {
	// The card's uniform padding leaves w-4 usable content columns (the same
	// budget the rounded border used to cost). Build every row to exactly cw
	// cells so nothing wraps inside the card.
	c = c.On(ElevCard)
	cw := w - 4
	if cw < 6 {
		cw = 6
	}

	num := c.Humanize(s.value)
	if s.fmtVal != nil {
		num = s.fmtVal(s.value)
	}
	deltaTxt, dir := "·", 0
	if c.Delta != nil {
		deltaTxt, dir = c.Delta(s.value, s.prev)
	}
	deltaStyle := deltaChipStyle(c, dir)

	// Number left, delta right, filling exactly cw so the row never wraps.
	gap := cw - lipgloss.Width(num) - lipgloss.Width(deltaTxt)
	if gap < 1 {
		deltaTxt = ""
		gap = cw - lipgloss.Width(num)
		if gap < 0 {
			num = c.Truncate(num, cw)
			gap = 0
		}
	}
	numberRow := c.Stat.Render(num) + c.pad(gap) + deltaStyle.Render(deltaTxt)

	footRow := c.StatLabel.Render(c.Truncate(s.foot, cw))
	if s.shareTot > 0 {
		footRow = c.fg(s.style.GetForeground()).Render(c.Percent(s.shareVal, s.shareTot)) + c.pad(1) +
			c.StatLabel.Render(c.Truncate(s.foot, cw-5))
	}

	body := numberRow
	if s.spark != "" {
		body += "\n" + s.spark
	}
	body += "\n" + footRow

	// A KPI tile is read-only, so its titled rule never carries a focus bar; the
	// rule still draws the card's extent now that the box is gone.
	return c.Block(ElevCard).Width(w).Render(c.titleRule(s.label, cw, false) + "\n" + body)
}

// deltaChipStyle maps a delta direction to its chip style per the Delta
// contract (format.go): rose = warm (more spend), fell = GoodColor (falling
// spend is good), flat/no-prior = subtle.
func deltaChipStyle(c Ctx, dir int) lipgloss.Style {
	switch {
	case dir > 0:
		return c.now()
	case dir < 0:
		return c.good()
	default:
		return c.Subtle
	}
}

// sidePanel renders the read-only by-tool composition bars over a four-component
// split gauge.
func sidePanel(c Ctx, d OverviewData, w, h int, focus bool) string {
	// Fill the card to the hero's height so the right column matches the trend
	// panel instead of floating short above empty terminal.
	elev := paneElev(focus)
	c = c.On(elev)
	style := c.Block(elev).Width(w).Height(maxInt(h, 3))
	inner := w - 4
	if inner < 4 {
		inner = 4
	}
	title := c.titleRule("BY TOOL · "+d.RangeLbl, inner, focus)
	body := toolRows(c, d.ByTool, inner)
	gauge := splitGauge(c, d.Totals, inner)
	content := title + "\n" + body + "\n" + gauge
	return style.Render(content)
}

// toolRows renders one per-tool row: glyph + colored name + a four-component
// composition bar + humanized total.
func toolRows(c Ctx, buckets []store.Bucket, inner int) string {
	if len(buckets) == 0 {
		return EmptyState(c, EmptyNoRows, inner)
	}
	if zeroTotals(buckets) {
		return EmptyState(c, EmptyZeroTokens, inner)
	}
	var max int64
	for _, b := range buckets {
		if b.Total > max {
			max = b.Total
		}
	}
	nameW := 10
	numW := 7
	barW := inner - nameW - numW - 6
	if barW < 6 {
		barW = 6
	}
	var rows []string
	for _, b := range buckets {
		tool := b.Keys["tool"]
		bar := c.CompBar(Split(b), max, barW)
		name := c.tool(tool).Render(c.PadRight(tool, nameW))
		num := c.Number.Render(c.PadLeft(c.Humanize(b.Total), numW))
		glyphStyled := c.fg(c.ToolAccent(tool)).Render(c.ToolGlyph(tool))
		rows = append(rows, glyphStyled+c.pad(1)+name+c.pad(1)+bar+c.pad(1)+num)
	}
	return strings.Join(rows, "\n")
}

// splitGauge renders the four-component split of t as a full-width composition
// bar with a per-component share legend.
func splitGauge(c Ctx, t store.Bucket, inner int) string {
	comp := Split(t)
	sum := comp.Sum()
	w := inner
	if w < 8 {
		w = 8
	}
	gauge := c.CompBar(comp, sum, w)
	parts := make([]string, 0, len(c.Comp))
	for _, s := range c.Comp {
		parts = append(parts, c.compStyle(s).Render(s.Glyph+" "+s.Short+" "+c.Percent(s.Pick(comp), sum)))
	}
	legend := c.fitParts(parts, c.pad(2), inner)
	return c.Rule(c.titleChip("SPLIT", false), inner) + "\n" + gauge + "\n" + legend
}

// compactToolStrip renders the by-tool data as a single horizontal strip for
// narrow widths (the side panel is dropped below the hero).
func compactToolStrip(c Ctx, d OverviewData, width int) string {
	c = c.On(ElevCard)
	card := c.Block(ElevCard).Width(width)
	inner := width - 4
	title := c.titleRule("BY TOOL", inner, false)
	if len(d.ByTool) == 0 {
		return card.Render(title + "\n" + EmptyState(c, EmptyNoRows, inner))
	}
	if zeroTotals(d.ByTool) {
		return card.Render(title + "\n" + EmptyState(c, EmptyZeroTokens, inner))
	}
	var parts []string
	limit := len(d.ByTool)
	if limit > 4 {
		limit = 4
	}
	for _, b := range d.ByTool[:limit] {
		tool := b.Keys["tool"]
		parts = append(parts, c.fg(c.ToolAccent(tool)).Render(c.ToolGlyph(tool))+c.pad(1)+
			c.tool(tool).Render(tool)+c.pad(1)+c.Number.Render(c.Humanize(b.Total)))
	}
	return card.Render(title + "\n" + c.fitParts(parts, c.pad(3), inner))
}
