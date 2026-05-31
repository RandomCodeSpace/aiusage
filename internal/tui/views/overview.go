package views

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

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
	Sys         []SysGauge // container CPU/mem/disk gauges (empty → strip omitted)
}

// Overview view panes (focus order; pane 0 = rail is owned by the root frame).
const (
	PaneOverviewKPIs = iota
	PaneOverviewHero
	PaneOverviewTools
)

// Overview renders the calm landing hub: a KPI strip, a hero time-series, and a
// side by-tool stacked bar panel over a fresh/cache split gauge. The layout is
// fully driven by lay: KPI columns reflow to the body width, the side panel
// appears only when lay grants it, and the hero degrades to a sparkline when
// there isn't room for a full chart.
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

	kpis := overviewKPIs(c, d, lay)
	kpisH := lipgloss.Height(kpis)
	bodyH := lay.BodyH - kpisH - stripH - 1
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
		tools := compactToolStrip(c, d, width)
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
	series             []float64 // nil → no sparkline (total/events are never graphed)
	style              lipgloss.Style
	shareVal, shareTot int64 // shareTot>0 shows a share %
}

// overviewKPIs renders the read-only KPI strip: one tile per token component
// (input, output, cache-read, cache-creation) with a self-scaled sparkline and
// its share of the component sum, then total and events as bare numbers (total
// is never graphed). Tiles reflow to fit the body width.
func overviewKPIs(c Ctx, d OverviewData, lay Layout) string {
	width := lay.BodyW
	tot := Split(d.Totals)
	prev := Split(d.Prev)
	sum := tot.Sum()

	specs := make([]kpiSpec, 0, len(c.Comp)+2)
	for _, s := range c.Comp {
		s := s
		specs = append(specs, kpiSpec{
			label:    s.Short,
			value:    s.Pick(tot),
			prev:     s.Pick(prev),
			series:   SeriesFor(d.Timeline, func(b store.Bucket) int64 { return s.Pick(Split(b)) }),
			style:    s.Style(),
			shareVal: s.Pick(tot),
			shareTot: sum,
		})
	}
	specs = append(specs,
		kpiSpec{label: "total", foot: "tokens", value: d.Totals.Total, prev: d.Prev.Total, style: c.Subtle},
		kpiSpec{label: "events", foot: "requests", value: d.Totals.Events, prev: d.Prev.Events, style: c.Subtle},
	)

	// How many tiles fit across, given a minimum useful tile width + 1-col gutters.
	const minTileW = 16
	per := (width + 1) / (minTileW + 1)
	if per > len(specs) {
		per = len(specs)
	}
	if per < 1 {
		per = 1
	}
	tileW := (width-(per-1))/per - 2 // -2 for each tile's border
	if tileW < 14 {
		tileW = 14
	}

	tiles := make([]string, len(specs))
	for i, s := range specs {
		tiles[i] = kpiTile(c, s, tileW, lay.Sparklines)
	}

	// Arrange the tiles into rows of `per`.
	var rows []string
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
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, segs...))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// kpiTile renders one read-only KPI bento card: title on the border, big
// right-aligned number + delta chip, an optional self-scaled sparkline, then a
// share % or unit. KPI tiles are not interactive (the trend is the only
// interactive surface on Overview).
func kpiTile(c Ctx, s kpiSpec, innerW int, spark bool) string {
	// The panel has Padding(0,1), so the usable content width is innerW-2. Build
	// every row to exactly cw cells so nothing wraps inside the box.
	cw := innerW - 2
	if cw < 6 {
		cw = 6
	}

	num := c.Humanize(s.value)
	deltaTxt, dir := "·", 0
	if c.Delta != nil {
		deltaTxt, dir = c.Delta(s.value, s.prev)
	}
	deltaStyle := c.Subtle
	if dir > 0 {
		deltaStyle = c.now() // up = warm (more spend)
	}

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
	numberRow := c.Stat.Render(num) + strings.Repeat(" ", gap) + deltaStyle.Render(deltaTxt)

	footRow := c.StatLabel.Render(c.Truncate(s.foot, cw))
	if s.shareTot > 0 {
		footRow = s.style.Render(c.Percent(s.shareVal, s.shareTot)) + " " +
			c.StatLabel.Render(c.Truncate(s.foot, cw-5))
	}

	body := numberRow
	if spark && s.series != nil {
		body += "\n" + newColumnSparkline(s.series, cw, 1, s.style)
	}
	body += "\n" + footRow

	return c.Panel.Width(innerW).Render(c.PanelTitle.Render(s.label) + "\n" + body)
}

// heroPanel renders the hero time-series chart panel, degrading to a sparkline +
// top-tool readout when there isn't room for a full axed chart.
func heroPanel(c Ctx, d OverviewData, w, h int, lay Layout, focus bool) string {
	style := c.panelStyle(focus).Width(w - 2)
	inner := w - 4
	if inner < 4 {
		inner = 4
	}
	title := c.titleChip("TREND", focus) + "  " + c.CompLegend()
	if d.ScrubLabel != "" {
		title += "  " + c.now().Render("◷ "+d.ScrubLabel)
	}
	chartH := h - 3 // title + border
	if chartH < 1 {
		chartH = 1
	}
	body := heroBody(c, d.Timeline, d.TimelineDim, lay, inner, chartH, -1)
	return style.Render(title + "\n" + body)
}

// sidePanel renders the read-only by-tool composition bars over a four-component
// split gauge.
func sidePanel(c Ctx, d OverviewData, w, h int, focus bool) string {
	// Fill the box to the hero's height (border = 2 rows) so the right column
	// matches the trend panel instead of floating short above empty terminal.
	style := c.panelStyle(focus).Width(w - 2).Height(maxInt(h-2, 1))
	inner := w - 4
	if inner < 4 {
		inner = 4
	}
	title := c.titleChip("BY TOOL · "+d.RangeLbl, focus)
	body := toolRows(c, d.ByTool, inner)
	gauge := splitGauge(c, d.Totals, inner)
	content := title + "\n" + body + "\n" + gauge
	return style.Render(content)
}

// toolRows renders one per-tool row: glyph + colored name + a four-component
// composition bar + humanized total.
func toolRows(c Ctx, buckets []store.Bucket, inner int) string {
	if len(buckets) == 0 {
		return c.Faint.Render("no usage in range")
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
		glyphStyled := lipgloss.NewStyle().Foreground(c.ToolAccent(tool)).Render(c.ToolGlyph(tool))
		rows = append(rows, glyphStyled+" "+name+" "+bar+" "+num)
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
		parts = append(parts, s.Style().Render(s.Short+" "+c.Percent(s.Pick(comp), sum)))
	}
	legend := c.Truncate(strings.Join(parts, "  "), inner)
	return c.StatLabel.Render("SPLIT") + "\n" + gauge + "\n" + legend
}

// compactToolStrip renders the by-tool data as a single horizontal strip for
// narrow widths (the side panel is dropped below the hero).
func compactToolStrip(c Ctx, d OverviewData, width int) string {
	if len(d.ByTool) == 0 {
		return c.Panel.Width(width - 2).Render(c.PanelTitle.Render("BY TOOL") + "\n" + c.Faint.Render("no usage in range"))
	}
	var parts []string
	limit := len(d.ByTool)
	if limit > 4 {
		limit = 4
	}
	for _, b := range d.ByTool[:limit] {
		tool := b.Keys["tool"]
		parts = append(parts, lipgloss.NewStyle().Foreground(c.ToolAccent(tool)).Render(c.ToolGlyph(tool))+" "+
			c.tool(tool).Render(tool)+" "+c.Number.Render(c.Humanize(b.Total)))
	}
	return c.Panel.Width(width - 2).Render(c.PanelTitle.Render("BY TOOL") + "\n" + c.Truncate(strings.Join(parts, "   "), width-4))
}
