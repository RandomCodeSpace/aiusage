package views

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// ByToolData feeds the By-Tool view: per-tool stacked fresh/cache bars on the
// left, a detail card for the selected tool on the right (trend sparkline +
// stats). CopilotAbsent triggers the "no data source" footnote.
type ByToolData struct {
	Rows          []store.Bucket // grouped by tool, sorted
	Grand         int64          // grand total for share %
	Selected      int            // index of the selected/focused bar
	SelTrend      []store.Bucket // selected tool's daily trend (ascending)
	SelTrendErr   bool           // the trend query failed (distinct from "no rows")
	SelSessions   int64          // distinct sessions for the selected tool
	RangeLbl      string
	ActivePane    int  // PaneByX* — which pane wears the ring
	CopilotAbsent bool // append the absent-source note
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
		selErr:     d.SelTrendErr,
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
	selErr     bool // the selected entity's trend query failed
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
//
// The rows are WINDOWED to what the card's interior holds. Rendering the whole
// list and letting the frame clamp cut the overflow is not "the rest is off
// screen": the cut takes the hidden rows' click zones with it — and this
// panel's own closing zone marker, which is what the wheel hit-tests to decide
// it is pointing at the bars at all. The window pages with the selection, so
// every row stays reachable by moving it.
func barsPanel(c Ctx, d byEntityData, w, h int, focus bool) string {
	// Height(h) fills the painted card to the full body height so the panel
	// doesn't float short above empty terminal on tall screens; lipgloss pads the
	// interior with blank rows, which the card's background paints.
	elev := paneElev(focus)
	c = c.On(elev)
	style := c.Block(elev).Width(w).Height(maxInt(h, 3))
	inner := w - 4

	if len(d.rows) == 0 {
		return style.Render(c.titleRule(d.title, inner, focus) + "\n" + emptyChartFrame(c, inner, h-3))
	}

	// Interior rows = h - block padding(2) - the title rule(1) - the footnote.
	fit := h - 3
	if d.footnote != "" {
		fit--
	}
	if fit < 1 {
		fit = 1
	}
	top, end := barWindow(len(d.rows), d.selected, fit)
	title := c.titleRule(d.title, inner, focus)
	if end-top < len(d.rows) {
		// Say which slice is on screen; without it a windowed list reads as the
		// whole list. The readout is dropped rather than allowed to crowd the
		// title on a narrow pane.
		tail := c.Subtle.Render(strconv.Itoa(top+1) + "-" + strconv.Itoa(end) + "/" + strconv.Itoa(len(d.rows)))
		if head := c.titleChip(d.title, focus); lipgloss.Width(head)+lipgloss.Width(tail)+2 <= inner {
			title = c.RuleBetween(head, tail, inner)
		}
	}

	var max int64
	for _, b := range d.rows {
		if b.Total > max {
			max = b.Total
		}
	}
	numW := 7
	shareW := 4
	// Row = marker(2) + drill(2) + glyph(1) + 4 single-space separators + name +
	// bar + num + share. Reserve the 9 fixed cols, then split the rest between
	// name and bar.
	avail := inner - numW - shareW - 9
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
	for i, b := range d.rows[top:end] {
		idx := top + i // the selection index is absolute, the window is not
		name := b.Keys[d.dim]
		ownTool := name
		if d.ownerTool != nil {
			ownTool = d.ownerTool(b)
		}
		// The selected row is a painted block one step up the ladder, marked by
		// the same width-invariant focus bar the pane title uses. The bar is the
		// monochrome channel; the paint is what makes the row read as a block.
		rc := c
		if idx == d.selected {
			rc = c.On(ElevRaised)
		}
		glyph := rc.fg(rc.ToolAccent(ownTool)).Render(rc.ToolGlyph(ownTool))

		var bar string
		if b.Total == 0 {
			// Zero-token row: the ∅ treatment, not a generic "no data" (which
			// would be indistinguishable from a failed or missing query).
			bar = rc.Faint.Render(rc.PadRight("∅ zero tokens", barW))
		} else {
			bar = rc.CompBar(Split(b), max, barW)
		}
		nameStyle := rc.tool(ownTool)
		body := glyph + rc.pad(1) + nameStyle.Render(rc.PadRight(displayName(rc, name, nameW), nameW)) + rc.pad(1) +
			bar + rc.pad(1) + rc.Number.Render(rc.PadLeft(rc.Humanize(b.Total), numW)) + rc.pad(1) +
			rc.Subtle.Render(rc.PadLeft(rc.Percent(b.Total, d.grand), shareW))
		// Every bar descends into Browse filtered by this entity, so every named
		// row wears the chevron; an unnamed ("—") bucket has no value to filter on
		// and therefore no affordance.
		marker := rc.FocusMark(idx == d.selected) + rc.pad(1) + rc.DrillMark(name != "") + rc.pad(1)
		rows = append(rows, c.mark(BarZone(name), marker+body))
	}
	content := title + "\n" + strings.Join(rows, "\n")
	if d.footnote != "" {
		content += "\n" + d.footnote
	}
	return c.mark(ZoneBars, style.Render(content))
}

// barWindow returns the half-open row range the bars panel renders: the page of
// fit rows that holds the selection. Paging (rather than a scroll offset that
// trails the cursor) keeps the panel a pure function of the data it is handed —
// no window state has to be threaded through the root model and kept in sync
// with two views' selections — and the page a row sits on never changes under
// the reader as the selection moves inside it.
func barWindow(n, selected, fit int) (top, end int) {
	if fit < 1 {
		fit = 1
	}
	if n <= fit {
		return 0, n
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= n {
		selected = n - 1
	}
	top = (selected / fit) * fit
	end = top + fit
	if end > n {
		end = n
	}
	return top, end
}

// detailCard renders the selected entity's trend sparkline + stat block.
func detailCard(c Ctx, d byEntityData, w, h int, focus bool) string {
	// Fill the card to the full body height so it lines up with the bars panel
	// instead of floating short above empty terminal.
	elev := paneElev(focus)
	c = c.On(elev)
	style := c.Block(elev).Width(w).Height(maxInt(h, 3))
	inner := w - 4
	if len(d.rows) == 0 || d.selected < 0 || d.selected >= len(d.rows) {
		return style.Render(c.titleRule("DETAIL", inner, focus) + "\n" + c.Faint.Render("no selection"))
	}
	b := d.rows[d.selected]
	name := b.Keys[d.dim]
	ownTool := name
	if d.ownerTool != nil {
		ownTool = d.ownerTool(b)
	}
	comp := Split(b)

	glyph := c.fg(c.ToolAccent(ownTool)).Render(c.ToolGlyph(ownTool))
	header := glyph + c.pad(1) + c.tool(ownTool).Render(displayName(c, name, inner-3))

	spark := trendStrip(c, d.selTrend, inner, 4)
	if d.selErr {
		spark = EmptyState(c, EmptyQueryFailed, inner)
	}

	stat := func(label, value string) string {
		return c.StatLabel.Render(c.PadRight(label, 9)) + c.Number.Render(value)
	}
	lines := []string{
		header,
		c.pad(inner),
		c.Rule(c.StatLabel.Render("TREND"), inner),
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
	return c.mark(ZonePreview, style.Render(c.titleRule("DETAIL", inner, focus)+"\n"+strings.Join(lines, "\n")))
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

// copilotFootnote states the absent source when copilot has no recorded usage.
// Zeros alone read as "you used nothing"; the honest reading is that no data
// source exists yet. `doctor` carries the enablement checklist.
func copilotFootnote(c Ctx, absent bool) string {
	if !absent {
		return ""
	}
	return c.Faint.Render("copilot: configured, no data source")
}
