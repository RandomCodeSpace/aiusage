package views

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// CopilotSourceState is what ADAPTER DISCOVERY knows about copilot's local data
// source, which is a fact about the machine. It is deliberately not derived from
// the loaded rows: an empty range says nothing about whether the source exists,
// and a footnote that confuses the two tells a user with telemetry enabled and
// data on disk that they have no data source (issue #44).
type CopilotSourceState int

const (
	// CopilotUnknown: no discovery result reached the view (headless embedding,
	// or discovery itself failed). Claim nothing.
	CopilotUnknown CopilotSourceState = iota
	// CopilotNoSource: discovery ran and located no copilot source.
	CopilotNoSource
	// CopilotIdle: a source exists, but nothing landed in the shown range.
	CopilotIdle
	// CopilotActive: a source exists and the range carries its usage.
	CopilotActive
)

// ByToolData feeds the By-Tool view: per-tool stacked fresh/cache bars on the
// left, a detail card for the selected tool on the right (trend sparkline +
// stats). Copilot carries the discovery-sourced footnote state.
type ByToolData struct {
	Rows        []store.Bucket // grouped by tool, sorted, long tail already folded
	Grand       int64          // grand total for share %
	Selected    int            // index of the selected/focused bar
	SelTrend    []store.Bucket // selected tool's daily trend (ascending)
	SelTrendErr bool           // the trend query failed (distinct from "no rows")
	SelSessions int64          // distinct sessions for the selected tool
	RangeLbl    string
	ActivePane  int                // PaneByX* — which pane wears the ring
	Copilot     CopilotSourceState // drives the absent/idle source footnote

	// FoldIndex is the position in Rows of the synthetic long-tail row, FoldCount
	// how many real tools it stands for, and FoldOpen whether the tail below it
	// is listed. A ZERO FoldCount is what means "nothing folded" — index 0 is a
	// legitimate fold position, so the count is the flag and the zero value of
	// this struct carries no fold. The fold is computed by the root model
	// (tui.foldMinorTools) against the same denominator Grand carries, so a row
	// reading "<1%" is exactly a row the fold would take.
	FoldIndex int
	FoldCount int
	FoldOpen  bool
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
		footnote:   copilotFootnote(c, d.Copilot),
		foldIndex:  d.FoldIndex,
		foldCount:  d.FoldCount,
		foldOpen:   d.FoldOpen,
		capability: true,
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

	// Long-tail fold (By-Tool only). See ByToolData.
	foldIndex int
	foldCount int
	foldOpen  bool
	// capability adds the per-tool capability block to the detail card. Only
	// By-Tool sets it: the declarations are per TOOL, and a model id has none.
	capability bool
	// reasoning adds the reasoning-share line to the detail card. Only By-Model
	// sets it — see byModelReasoningLine.
	reasoning bool
}

// hasFold reports whether this list carries a synthetic long-tail row. The test
// is the COUNT and not the index, so the zero value of byEntityData is inert:
// an index of 0 is a legitimate position for a fold row, and a struct nobody set
// the fold fields on would otherwise render its first tool as the tail.
func (d byEntityData) hasFold() bool { return d.foldCount > 0 && d.foldIndex >= 0 }

// isFold reports whether row idx is the fold row.
func (d byEntityData) isFold(idx int) bool { return d.hasFold() && idx == d.foldIndex }

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
	// The fold row's label is a name too, and the widest form of it is the one
	// carrying the row count. Sizing the column without it would guarantee the
	// count is the first thing truncated on every terminal.
	if d.hasFold() && d.foldIndex < len(d.rows) {
		if w := lipgloss.Width(foldLabelForms(c, d)[0]); w > nameW {
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
		if d.isFold(idx) {
			rows = append(rows, c.mark(ZoneFold,
				foldRow(rc, d, b, max, nameW, barW, numW, shareW, idx == d.selected)))
			continue
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

// foldGlyph is the fold row's stand-in for a tool glyph: three dots, the
// typographic sign for elided material, in place of a per-tool mark it has no
// right to wear.
const foldGlyph = "⋯"

// foldOpenMark / foldShutMark are the disclosure triangles, drawn in the DRILL
// slot. The slot is one cell at every width (DrillMark), so swapping the chevron
// for a triangle changes what the row advertises without moving a column: on a
// tool row that cell says "this descends into Sessions", on the fold row it says
// "this opens in place".
const (
	foldShutMark = "▸"
	foldOpenMark = "▾"
)

// foldLabelForms names the fold row in every form the name column may choose
// between, widest first, on the same contract Span.labelForms uses: each form is
// complete, so a narrow column picks a shorter name instead of truncating a
// longer one into "9 others · 14…", which reads as a count and is not one.
//
// The token total is NOT in these forms — it is the row's own number column, a
// cell to the right, and stating it twice would cost the count its space.
func foldLabelForms(c Ctx, d byEntityData) []string {
	n := strconv.Itoa(d.foldCount)
	rows := int64(0)
	if d.hasFold() && d.foldIndex < len(d.rows) {
		rows = d.rows[d.foldIndex].Events
	}
	return []string{
		n + " others · " + hl(c, rows) + " rows",
		n + " others · " + hl(c, rows),
		n + " others",
		n + " oth",
	}
}

// foldRow renders the synthetic long-tail row. It carries the tail's REAL
// aggregate composition bar, token total and share, because it is a total and
// not a placeholder: the panel still adds up to the window whether the fold is
// open or shut.
func foldRow(c Ctx, d byEntityData, b store.Bucket, max int64, nameW, barW, numW, shareW int, selected bool) string {
	mark := foldShutMark
	if d.foldOpen {
		mark = foldOpenMark
	}
	glyph := c.Subtle.Render(foldGlyph)
	label := pickForm(foldLabelForms(c, d), nameW)
	bar := c.CompBar(Split(b), max, barW)
	if b.Total == 0 {
		bar = c.Faint.Render(c.PadRight("∅ zero tokens", barW))
	}
	body := glyph + c.pad(1) + c.Subtle.Render(c.PadRight(label, nameW)) + c.pad(1) +
		bar + c.pad(1) + c.Number.Render(c.PadLeft(c.Humanize(b.Total), numW)) + c.pad(1) +
		c.Subtle.Render(c.PadLeft(c.Percent(b.Total, d.grand), shareW))
	return c.FocusMark(selected) + c.pad(1) + c.Subtle.Render(mark) + c.pad(1) + body
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
	folded := d.isFold(d.selected)
	if folded {
		// The fold row is several tools at once, so it has no identity to head the
		// card with and no capability declaration to make. Its numbers are real
		// and are shown; everything per-tool below is skipped.
		header = c.Subtle.Render(foldGlyph) + c.pad(1) +
			c.Stat.Render(truncTo(c, pickForm(foldLabelForms(c, d), inner-2), inner-2))
	}

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
	// The cost total is BOUNDED and provenance-marked: unpriced rows in the
	// selection make it a floor, and a rate-card price makes it an estimate. The
	// budget is the card's interior minus the 9-cell stat label.
	if cost := costText(c, b.CostMicroUSD, b.UnpricedEvents, b.ComputedCostEvents, inner-9); cost != "" {
		lines = append(lines, stat("cost", cost))
	}
	if d.selSess > 0 {
		lines = append(lines, stat("sessions", c.Humanize(d.selSess)))
	}
	if d.reasoning {
		if line := reasoningShareLine(c, b, inner); line != "" {
			lines = append(lines, line)
		}
	}
	if d.capability && !folded {
		lines = append(lines, capabilityLines(c, name, inner)...)
	}
	// The card is as tall as the bars panel beside it. Trimming trailing lines
	// here rather than letting the root frame clamp the overflow means the rows
	// that go are chosen by what matters least — the capability block, then the
	// per-series split — instead of by arithmetic.
	if avail := maxInt(h, 3) - 2*blockPadY - 1; avail > 0 && len(lines) > avail {
		lines = lines[:avail]
	}
	return c.mark(ZonePreview, style.Render(c.titleRule("DETAIL", inner, focus)+"\n"+strings.Join(lines, "\n")))
}

// reasoningShareLine states what fraction of the selected model's OUTPUT tokens
// were reasoning: "reasoning 31% of output". One line, on the By-Model card
// only.
//
// The denominator is output and not the total, because that is the only ratio
// the number means anything as. Every tool that reports a reasoning count either
// carries it inside output or beside it (model.ReasoningReportFor), so the share
// is either "how much of the output was thinking" or "how much thinking rode
// alongside it" — both read against output, neither against a total that is
// mostly cache. A model whose rows report no reasoning at all renders nothing:
// a "0% of output" would claim the source measured it and found none.
func reasoningShareLine(c Ctx, b store.Bucket, inner int) string {
	if b.Reasoning <= 0 || b.Output <= 0 || c.Percent == nil {
		return ""
	}
	return c.StatLabel.Render(c.PadRight("reasoning", 9)) +
		c.Number.Render(truncTo(c, c.Percent(b.Reasoning, b.Output)+" of output", inner-9))
}

// capabilityLines renders the four per-tool declarations: where a cost figure
// came from, whether a tool call can be joined to the turn that paid for it, how
// the source reports reasoning, and how well the adapter is verified
// (model.CapabilityFor).
//
// They are the answer to the question the numbers above raise and cannot
// answer: a tool showing "-" for cost and one showing "$12.40" differ because of
// what their SOURCE exposes, not because of what was spent, and without this
// block the difference reads as a bug. A tool id the table has not been taught
// about renders one honest line saying so, rather than four plausible defaults.
func capabilityLines(c Ctx, tool string, inner int) []string {
	if tool == "" {
		return nil
	}
	head := c.Rule(c.StatLabel.Render("SOURCE"), inner)
	cap, ok := model.CapabilityFor(tool)
	if !ok {
		return []string{head, c.Faint.Render(truncTo(c, "no capability declaration", inner))}
	}
	// Each value gets its own width ladder. A truncated declaration is worse
	// than a shorter one: "recorded, unattribu…" reads as a rendering fault,
	// while "unattributed" is the same claim in fewer cells. Every rung is a
	// complete statement, on the contract Span.labelForms established.
	// 10, not 9: "reasoning" is exactly nine cells, and padding it to its own
	// width leaves no gap at all ("reasoningsubset").
	const capLabelW = 10
	row := func(label string, forms []string) string {
		return c.StatLabel.Render(c.PadRight(label, capLabelW)) +
			c.Subtle.Render(pickForm(forms, inner-capLabelW))
	}
	return []string{
		head,
		row("cost", costProvenanceForms(cap.Cost)),
		row("activity", activityCaptureForms(cap.Activity)),
		row("reasoning", reasoningReportForms(cap.Reasoning)),
		row("tier", []string{string(cap.Tier)}),
	}
}

// costProvenanceForms names a cost provenance, widest first.
func costProvenanceForms(p model.CostProvenance) []string {
	switch p {
	case model.CostVendor:
		return []string{"vendor-reported", "vendor"}
	case model.CostComputed:
		return []string{"computed", "est."}
	default:
		return []string{string(p)}
	}
}

// activityCaptureForms names an activity capability, widest first. The shortest
// rung of the unattributed case keeps the WORD rather than shortening to a
// symbol: "unattributed" is the whole point of that declaration.
func activityCaptureForms(a model.ActivityCapture) []string {
	switch a {
	case model.ActivityExact:
		return []string{"exact join", "exact"}
	case model.ActivityUnattributed:
		return []string{"recorded, unattributed", "unattributed"}
	default:
		return []string{string(a)}
	}
}

// reasoningReportForms names a reasoning report, widest first. "not reported"
// never shortens to "none": in this column "none" would read as "no reasoning
// happened", which is the confusion the third state exists to prevent.
func reasoningReportForms(r model.ReasoningReport) []string {
	if r == model.ReasoningReportNone {
		return []string{"not reported", "unreported"}
	}
	return []string{string(r)}
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

// copilotFootnote states what discovery found. Zeros alone read as "you used
// nothing"; when no source exists the honest reading is that there is nothing
// to read yet (`doctor` carries the enablement checklist). When a source DOES
// exist the note says only what is true — this range is empty — and when
// discovery has nothing to say, neither does the footnote.
func copilotFootnote(c Ctx, st CopilotSourceState) string {
	switch st {
	case CopilotNoSource:
		return c.Faint.Render("copilot: configured, no data source")
	case CopilotIdle:
		return c.Faint.Render("copilot: source present, no usage in this range")
	}
	return ""
}
