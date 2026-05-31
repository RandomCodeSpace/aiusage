package views

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// ByToolData feeds the By-Tool view: per-tool stacked fresh/cache bars on the
// left, a detail card for the selected tool on the right (trend sparkline +
// stats). CopilotAbsent triggers the OTEL-disabled footnote.
type ByToolData struct {
	Rows          []store.Bucket // grouped by tool, sorted
	Grand         int64          // grand total for share %
	Selected      int            // index of the selected/focused bar
	SelTrend      []store.Bucket // selected tool's daily trend (ascending)
	SelSessions   int64          // distinct sessions for the selected tool
	RangeLbl      string
	ActivePane    int  // PaneByX* — which pane wears the ring
	CopilotAbsent bool // append the OTEL note
}

// By-Tool / By-Model view panes (pane 0 = rail).
const (
	PaneByXBars = iota
	PaneByXDetail
)

// ByTool renders the by-tool dashboard.
func ByTool(c Ctx, d ByToolData, lay Layout) string {
	return byEntity(c, byEntityData{
		title:      "BY TOOL · " + d.RangeLbl,
		dim:        "tool",
		rows:       d.Rows,
		grand:      d.Grand,
		selected:   d.Selected,
		selTrend:   d.SelTrend,
		selSess:    d.SelSessions,
		activePane: d.ActivePane,
		footnote:   copilotFootnote(c, d.CopilotAbsent),
	}, lay)
}

// byEntityData is the shared input for the By-Tool / By-Model skeleton.
type byEntityData struct {
	title      string
	dim        string
	rows       []store.Bucket
	grand      int64
	selected   int
	selTrend   []store.Bucket
	selSess    int64
	activePane int
	ownerTool  func(store.Bucket) string // for by-model: dominant owning tool
	footnote   string                    // optional footer note
}

// byEntity renders the shared bars-left / detail-right layout. The detail card
// appears only when the layout grants a side panel; otherwise the bars take the
// whole body. Bars are hand-rolled cell-safe StackBars (no ntcharts), so they
// never overflow at any width.
func byEntity(c Ctx, d byEntityData, lay Layout) string {
	bodyH := lay.BodyH
	if bodyH < 3 {
		bodyH = 3
	}

	if !lay.SidePanel {
		return barsPanel(c, d, lay.MainW, bodyH, true)
	}

	bars := barsPanel(c, d, lay.MainW, bodyH, d.activePane == PaneByXBars)
	detail := detailCard(c, d, lay.SideW, bodyH, d.activePane == PaneByXDetail)
	return lipgloss.JoinHorizontal(lipgloss.Top, bars, " ", detail)
}

// barsPanel renders the per-entity stacked fresh/cache rows (hand-rolled rather
// than ntcharts barchart, so each row carries glyph + colored name + number +
// share and its own click zone — richer than the raw barchart labels).
func barsPanel(c Ctx, d byEntityData, w, h int, focus bool) string {
	// Height(h-2) fills the bordered box to the full body height (border = 2 rows)
	// so the panel doesn't float as a short box above empty terminal on tall
	// screens; lipgloss pads the interior with blank rows.
	style := c.panelStyle(focus).Width(w - 2).Height(maxInt(h-2, 1))
	inner := w - 4
	title := c.titleChip(d.title, focus)

	if len(d.rows) == 0 {
		return style.Render(title + "\n" + emptyChartFrame(c, inner, h-3))
	}

	var max int64
	for _, b := range d.rows {
		if b.Total > max {
			max = b.Total
		}
	}
	numW := 7
	shareW := 4
	// Row = marker(2) + glyph(1) + 4 single-space separators + name + bar + num
	// + share. Reserve the 7 fixed cols, then split the rest between name and bar.
	avail := inner - numW - shareW - 7
	if avail < 8 {
		avail = 8
	}
	// Size the name column to the longest entity name so long model ids
	// ("claude-haiku-4-5-20251001") aren't clipped to short tool-name width;
	// clamp to ~3/5 of the splittable space so the name never crowds out the bar.
	nameW := 8
	for _, b := range d.rows {
		if w := lipgloss.Width(b.Keys[d.dim]); w > nameW {
			nameW = w
		}
	}
	if maxName := avail * 3 / 5; nameW > maxName {
		nameW = maxName
	}
	barW := avail - nameW
	// Cap the bar so it never becomes an unreadably long line on ultrawide
	// terminals (proportions read fine well before this); the surplus becomes
	// trailing whitespace. This is a bar-chart panel, so the bar is allowed to
	// use real width — the cap is generous, not the old tight 30.
	if barW > 80 {
		barW = 80
	}
	if barW < 6 {
		barW = 6
	}
	// The bar floor can overrun the budget on very narrow widths; trim the name
	// back so name+bar always fit and the row never wraps.
	if nameW+barW > avail {
		nameW = avail - barW
		if nameW < 1 {
			nameW = 1
		}
	}

	var rows []string
	for i, b := range d.rows {
		name := b.Keys[d.dim]
		ownTool := name
		if d.ownerTool != nil {
			ownTool = d.ownerTool(b)
		}
		glyph := lipgloss.NewStyle().Foreground(c.ToolAccent(ownTool)).Render(c.ToolGlyph(ownTool))

		var bar string
		if b.Total == 0 {
			bar = c.Faint.Render(c.PadRight("no data", barW))
		} else {
			bar = c.CompBar(Split(b), max, barW)
		}
		nameStyle := c.tool(ownTool)
		marker := "  "
		body := glyph + " " + nameStyle.Render(c.PadRight(displayName(c, name, nameW), nameW)) + " " +
			bar + " " + c.Number.Render(c.PadLeft(c.Humanize(b.Total), numW)) + " " +
			c.Subtle.Render(c.PadLeft(c.Percent(b.Total, d.grand), shareW))
		if i == d.selected {
			marker = c.now().Render("▎ ")
			body = lipgloss.NewStyle().Bold(true).Render(body)
		}
		rows = append(rows, c.mark(BarZone(name), marker+body))
	}
	content := title + "\n" + strings.Join(rows, "\n")
	if d.footnote != "" {
		content += "\n" + d.footnote
	}
	return c.mark(ZoneBars, style.Render(content))
}

// detailCard renders the selected entity's trend sparkline + stat block.
func detailCard(c Ctx, d byEntityData, w, h int, focus bool) string {
	// Fill the box to the full body height so it lines up with the bars panel
	// instead of floating short above empty terminal.
	style := c.panelStyle(focus).Width(w - 2).Height(maxInt(h-2, 1))
	inner := w - 4
	if len(d.rows) == 0 || d.selected < 0 || d.selected >= len(d.rows) {
		return style.Render(c.titleChip("DETAIL", focus) + "\n" + c.Faint.Render("no selection"))
	}
	b := d.rows[d.selected]
	name := b.Keys[d.dim]
	ownTool := name
	if d.ownerTool != nil {
		ownTool = d.ownerTool(b)
	}
	comp := Split(b)

	glyph := lipgloss.NewStyle().Foreground(c.ToolAccent(ownTool)).Render(c.ToolGlyph(ownTool))
	header := glyph + " " + c.tool(ownTool).Render(displayName(c, name, inner-3))

	spark := trendStrip(c, d.selTrend, inner, 4)

	stat := func(label, value string) string {
		return c.StatLabel.Render(c.PadRight(label, 9)) + c.Number.Render(value)
	}
	lines := []string{
		header,
		"",
		c.Faint.Render(strings.Repeat("─", inner)),
		spark,
		c.Faint.Render(strings.Repeat("─", inner)),
		stat("total", c.Humanize(b.Total)),
	}
	for _, s := range c.Comp {
		lines = append(lines, stat(s.Short, c.Humanize(s.Pick(comp))+" ("+c.Percent(s.Pick(comp), comp.Sum())+")"))
	}
	lines = append(lines,
		stat("events", c.Humanize(b.Events)),
		stat("share", c.Percent(b.Total, d.grand)),
	)
	if d.selSess > 0 {
		lines = append(lines, stat("sessions", c.Humanize(d.selSess)))
	}
	return c.mark(ZonePreview, style.Render(c.titleChip("DETAIL", focus)+"\n"+strings.Join(lines, "\n")))
}

// displayName truncates an entity name to width.
func displayName(c Ctx, name string, width int) string {
	if name == "" {
		return "—"
	}
	if c.Truncate != nil {
		return c.Truncate(name, width)
	}
	return name
}

// copilotFootnote returns the OTEL note when copilot has no recorded usage.
func copilotFootnote(c Ctx, absent bool) string {
	if !absent {
		return ""
	}
	return c.Faint.Render("copilot: enable OpenTelemetry export to record usage")
}
