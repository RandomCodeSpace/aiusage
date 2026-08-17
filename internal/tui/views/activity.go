package views

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

// activity.go renders the Activity tab: what the agent actually DID — which
// tools it called, which skills and hooks fired, how often, and what that cost.
//
// The tab has SIX readings of one window, cycled with the pivot key, and they
// are the six partitions of the same dollars. The first reads the activity
// ledger (store.ActivityBucket): one row per invocation, cost derived by
// dividing the turn between the calls that shared it. The other five read the
// turn-context table (store.TurnContextBucket) along ONE dimension each —
// agent, skill, mcp tool, mcp server, plugin — where a value's cost is the FULL
// cost of the turns that ran under it, with no divisor, because within one
// dimension a turn belongs to exactly one value.
//
// THE UI IS THE PARTITION INVARIANT. Exactly one pivot is on screen at a time,
// each load asks the store for exactly one dimension, and nothing on this tab
// adds two of them together — which is the whole reason the store makes the
// dimension a required argument rather than a filter. A screen showing agents
// and skills side by side would report a turn carrying both twice, and there is
// no state this tab can be in that produces one.
//
// Whichever pivot is showing, the surface is built around one fact the other
// tabs never have to state: a number here can be UNKNOWN. In the calls pivot an
// activity row names the usage row its turn produced, and where that join does
// not exist — codex records its calls and its token counts in unrelated
// records, hooks carry no usage at all — the tokens are not zero, they are
// unknowable. On this machine that is roughly half of all rows, so a ranking
// that let those read as free would put the busiest tool in the dashboard at the
// bottom of the cost list. Every place a number could be mistaken for a complete
// one therefore carries the "?" mark, and the rank panel's footnote states the
// volume outright at every geometry — it is the one line that is never dropped.

// ActivityData feeds the Activity view.
//
// Rows/Kinds/Calls/Totals are the CALLS pivot's three readings of the window.
// CtxRows/CtxTools/CtxTotals are the turn-context pivots' equivalents. Exactly
// one set is populated per load — Pivot says which, and the two are separate
// fields rather than one projected onto the other because they are different
// facts: an activity bucket's tokens are a divided SHARE of a turn, a turn
// context's are the turn's whole cost, and a shape that let a caller read one
// as the other is the mistake this tab is most exposed to.
type ActivityData struct {
	Rows     []store.ActivityBucket // ranked (name, kind, tool) page, display order
	Kinds    []store.ActivityBucket // grouped by kind: tool / skill / hook
	Calls    []store.ActivityBucket // per hour/day call counts, ascending
	CallsDim string                 // the Calls bucket key ("hour" / "day")
	Totals   store.ActivityBucket   // grand total over the window

	// Pivot names the turn-context dimension on screen ("agent", "skill",
	// "mcp_tool", "mcp_server", "plugin"), or "" for the calls pivot. It is the
	// ONLY thing that decides which set below is read.
	Pivot string
	// CtxRows is the ranked (value, tool) page of the pivoted dimension,
	// CtxBuckets its per-time-bucket turn counts, CtxTools the same window
	// grouped by tool (which is both the coverage note and the grand total's
	// source), and CtxTotals the grand total.
	CtxRows    []store.TurnContextBucket
	CtxBuckets []store.TurnContextBucket
	CtxTools   []store.TurnContextBucket
	CtxTotals  store.TurnContextBucket

	Selected   int    // index of the selected/focused row
	RangeLbl   string // the window the tab is showing
	OrderLbl   string // the metric Rows is ranked by ("calls" / "cost" / …)
	Limit      int    // the cap the page came back under (0 = uncapped)
	ActivePane int    // PaneActivity* — which pane wears the ring
}

// Activity view panes. The rank list is the one interactive surface; the
// summary card and the detail card are read-only, like every other tab.
const (
	PaneActivityRank = iota
	PaneActivityDetail
)

// minRankPanelH is the smallest rank panel worth rendering: the card's two
// padding rows, its titled rule, one data row and the footnote. The summary
// card is dropped rather than let the body fall under it.
const minRankPanelH = 2*blockPadY + 3

// activityKindOrder is the order kinds are listed in, most voluminous first.
// SQL returns them alphabetically (hook, skill, tool), which puts the eight
// skill invocations of a busy week between the two large counts.
var activityKindOrder = []string{"tool", "skill", "hook"}

// invRow is one ranked row, projected from whichever ledger the pivot names.
// It exists so the rank panel, the detail card and the summary line are written
// ONCE: the two sources agree on every field a renderer needs and disagree only
// on what the fields mean, which is a matter for the labels and the footnote,
// not for a second copy of the layout arithmetic.
//
// The projection is one-way and per-pivot. Nothing builds a slice holding rows
// from two sources, and nothing sums across pivots.
type invRow struct {
	name, kind, tool string
	// count is calls in the calls pivot and TURNS in a turn-context pivot. The
	// column that shows it is labelled from ActivityData.countLabel, so the
	// number on screen always says which it is.
	count    int64
	sessions int64
	input    int64
	output   int64
	total    int64
	cost     int64
	// unattributed is calls with no usage row to join (calls pivot only). A
	// turn context always has its usage row — that is what it is keyed on — so
	// this is the store's UnjoinedTurns there, which is zero in practice and
	// reported rather than assumed.
	unattributed int64
	unpriced     int64
	// computed counts the rows in cost that were valued from a public rate card
	// rather than reported by the harness (model.PriceProvenance).
	computed int64
}

// rowFromActivity projects an activity bucket. Its token fields are the call's
// SHARE of its turn (see store.ActivityBucket).
func rowFromActivity(b store.ActivityBucket) invRow {
	return invRow{
		name: b.Keys["name"], kind: b.Keys["kind"], tool: b.Keys["tool"],
		count: b.Calls, sessions: b.Sessions,
		input: b.AttributedInput, output: b.AttributedOutput, total: b.AttributedTotal,
		cost:         b.AttributedCostMicroUSD,
		unattributed: b.UnattributedCalls, unpriced: b.UnpricedCalls,
		computed: b.ComputedCostCalls,
	}
}

// rowFromTurnContext projects a turn-context bucket along dimension dim. Its
// token fields are the turns' FULL counts — no share is taken, because within
// one dimension the join is 1:1 (see store.TurnContextBucket).
func rowFromTurnContext(b store.TurnContextBucket, dim string) invRow {
	return invRow{
		name: b.Keys["value"], kind: dim, tool: b.Keys["tool"],
		count: b.Turns, sessions: b.Sessions,
		input: b.InputTokens, output: b.OutputTokens, total: b.TotalTokens,
		cost:         b.CostMicroUSD,
		unattributed: b.UnjoinedTurns, unpriced: b.UnpricedTurns,
		computed: b.ComputedCostTurns,
	}
}

// rows projects the page the active pivot named.
func (d ActivityData) rows() []invRow {
	if d.Pivot == "" {
		out := make([]invRow, len(d.Rows))
		for i, b := range d.Rows {
			out[i] = rowFromActivity(b)
		}
		return out
	}
	out := make([]invRow, len(d.CtxRows))
	for i, b := range d.CtxRows {
		out[i] = rowFromTurnContext(b, d.Pivot)
	}
	return out
}

// RowCount is how many rows the active pivot holds. The root model's selection
// clamp reads it, so a pivot switch can never leave the cursor past the end of
// the list it now points at.
func (d ActivityData) RowCount() int {
	if d.Pivot == "" {
		return len(d.Rows)
	}
	return len(d.CtxRows)
}

// totals projects the grand total of the active pivot.
func (d ActivityData) totals() invRow {
	if d.Pivot == "" {
		return rowFromActivity(d.Totals)
	}
	return rowFromTurnContext(d.CtxTotals, d.Pivot)
}

// countLabel names what the count column counts. A turn context has no per-call
// row to count, so ranking "by calls" there ranks turns, and the column has to
// say so or the two pivots read as the same number.
func (d ActivityData) countLabel() string {
	if d.Pivot == "" {
		return "calls"
	}
	return "turns"
}

// panelTitle names the rank list for the active pivot.
func (d ActivityData) panelTitle() string {
	if d.Pivot == "" {
		return "INVOCATIONS"
	}
	return "TURNS BY " + strings.ToUpper(strings.ReplaceAll(d.Pivot, "_", " "))
}

// Activity renders the tab: a compact summary card over a rank list, with a
// read-only detail card for the selected row when the layout grants a side
// panel.
func Activity(c Ctx, d ActivityData, lay Layout) string {
	width := lay.BodyW
	if width < 8 {
		width = 8
	}

	summary := ""
	summaryH := 0
	if card := activitySummaryCard(c, d, width, lay); card != "" {
		h := lipgloss.Height(card)
		if lay.BodyH-h >= minRankPanelH {
			summary, summaryH = card, h
		}
	}

	bodyH := lay.BodyH - summaryH
	if bodyH < 3 {
		bodyH = 3
	}

	join := func(rest string) string {
		if summary == "" {
			return rest
		}
		return lipgloss.JoinVertical(lipgloss.Left, summary, rest)
	}

	rows := d.rows()
	if !lay.SidePanel {
		return join(activityRankPanel(c, d, rows, width, bodyH, d.ActivePane == PaneActivityRank))
	}
	rank := activityRankPanel(c, d, rows, lay.MainW, bodyH, d.ActivePane == PaneActivityRank)
	detail := activityDetailCard(c, d, rows, lay.SideW, bodyH, d.ActivePane == PaneActivityDetail)
	return join(lipgloss.JoinHorizontal(lipgloss.Top, rank, " ", detail))
}

// activitySummaryCard is the read-only header: what KIND of thing ran (calls
// pivot) or which tools contributed the context at all (turn-context pivots),
// one line of coverage totals, and — where the body can pay for a row — a heat
// strip of volume over the window, drawn with the same ramp the KPI tiles use.
func activitySummaryCard(c Ctx, d ActivityData, width int, lay Layout) string {
	c = c.On(ElevCard)
	inner := width - 2*blockPadX
	if inner < 8 {
		inner = 8
	}
	card := c.Block(ElevCard).Width(width)
	label := "ACTIVITY"
	if d.Pivot != "" {
		label = strings.ToUpper(strings.ReplaceAll(d.Pivot, "_", " "))
	}
	chip := c.titleChip(label+" · "+d.RangeLbl, false)
	title := c.Rule(chip, inner)

	if d.totals().count == 0 {
		return card.Render(title + "\n" + EmptyState(c, EmptyNoRows, inner))
	}
	// The series legend hangs off the end of the rule, where it names the
	// colours of the rank bars below — the same legend, and the same three
	// series, the rest of the dashboard uses.
	if legend := c.CompLegend(); lipgloss.Width(chip)+lipgloss.Width(legend)+2 <= inner {
		title = c.RuleBetween(chip, legend, inner)
	}

	lines := []string{title, activityBreakdownRow(c, d, inner), activityTotalsRow(c, d, inner)}
	if lay.Sparklines && !lay.Dense {
		lines = append(lines, activityCallStrip(c, d, inner))
	}
	return card.Render(strings.Join(lines, "\n"))
}

// activityCallStrip is the volume heat row: one cell per time bucket, newest at
// the right, on the same six-rung ramp the KPI tiles use. It is LABELLED with
// its cadence — a bare strip of shades cannot say whether a cell is an hour or a
// day, and the two read identically.
func activityCallStrip(c Ctx, d ActivityData, inner int) string {
	var vals []float64
	if d.Pivot == "" {
		vals = make([]float64, len(d.Calls))
		for i, b := range d.Calls {
			vals[i] = float64(b.Calls)
		}
	} else {
		vals = make([]float64, len(d.CtxBuckets))
		for i, b := range d.CtxBuckets {
			vals[i] = float64(b.Turns)
		}
	}
	w := inner
	label := ""
	if d.CallsDim != "" {
		label = "per " + d.CallsDim
		if lw := lipgloss.Width(label) + 1; w-lw >= 8 {
			w -= lw
			label = c.StatLabel.Render(label) + c.pad(1)
		} else {
			label, w = "", inner
		}
	}
	return label + heatStrip(c, vals, w, heatPeak(vals), heatConstInk(c.fg(c.AccentColor)))
}

// activityBreakdownRow is the summary card's second line: the tool / skill /
// hook breakdown in the calls pivot, and the CONTRIBUTING TOOLS in a
// turn-context pivot.
//
// The coverage note is not decoration. Turn context is claude-code's five
// attribution strings plus what copilot, dsh and qwen-code derive for the agent
// axis, and NOTHING else on this machine emits it — so a pivot showing one tool
// is complete rather than broken, and a reader who cannot tell those apart will
// read "0 skills" as "I ran no skills" instead of "no harness here reports
// them".
func activityBreakdownRow(c Ctx, d ActivityData, inner int) string {
	if d.Pivot != "" {
		return activityCoverageRow(c, d, inner)
	}
	byKind := make(map[string]int64, len(d.Kinds))
	for _, b := range d.Kinds {
		byKind[b.Keys["kind"]] += b.Calls
	}
	parts := make([]string, 0, len(activityKindOrder))
	for _, k := range activityKindOrder {
		n, ok := byKind[k]
		if !ok {
			continue
		}
		parts = append(parts, c.StatLabel.Render(k)+c.pad(1)+c.Number.Render(hl(c, n)))
	}
	if len(parts) == 0 {
		return c.Faint.Render(truncTo(c, "no kinds in range", inner))
	}
	return c.fitParts(parts, c.pad(3), inner)
}

// activityCoverageRow names the tools whose turns carry this dimension at all,
// each with its turn count, prefixed by the word that makes the list a claim
// about COVERAGE rather than a ranking.
func activityCoverageRow(c Ctx, d ActivityData, inner int) string {
	if len(d.CtxTools) == 0 {
		return c.Faint.Render(truncTo(c, "no tool here reports this context", inner))
	}
	parts := make([]string, 0, len(d.CtxTools)+1)
	parts = append(parts, c.StatLabel.Render("from"))
	for _, b := range d.CtxTools {
		tool := b.Keys["tool"]
		parts = append(parts, c.fg(c.ToolAccent(tool)).Render(c.ToolGlyph(tool))+c.pad(1)+
			c.tool(tool).Render(tool)+c.pad(1)+c.Number.Render(hl(c, b.Turns)))
	}
	return c.fitParts(parts, c.pad(2), inner)
}

// activityTotalsRow states the window's coverage in one line: how many calls (or
// turns) ran, over how many sessions, how many tokens and how much spend. The
// unattributed volume is NOT here — it is the rank panel's footnote, which no
// geometry drops.
func activityTotalsRow(c Ctx, d ActivityData, inner int) string {
	t := d.totals()
	stat := func(label string, value string) string {
		return c.StatLabel.Render(label) + c.pad(1) + c.Number.Render(value)
	}
	parts := []string{
		stat(d.countLabel(), hl(c, t.count)),
		stat("sessions", hl(c, t.sessions)),
		stat("tokens", hl(c, t.total)),
	}
	// The summary's cost is the tab's one cost TOTAL, so it gets the full
	// bounded form with its rider where the line has room for it.
	if cost := activityCostForm(c, t, inner/3); cost != "" {
		parts = append(parts, stat("cost", cost))
	}
	return c.fitParts(parts, c.pad(3), inner)
}

// activityRankPanel renders the ranked page. It is the tab's one interactive
// pane: the selection moves here and the detail card follows it.
//
// Rows are windowed to what the card holds, on the same contract as the
// by-entity bars (barsPanel): rendering the whole list and letting the frame
// clamp the overflow would take the hidden rows' click zones — and this panel's
// own closing zone marker — with it.
func activityRankPanel(c Ctx, d ActivityData, rows []invRow, w, h int, focus bool) string {
	elev := paneElev(focus)
	c = c.On(elev)
	style := c.Block(elev).Width(w).Height(maxInt(h, 3))
	inner := w - 2*blockPadX
	if inner < 8 {
		inner = 8
	}
	title := d.panelTitle() + " · by " + d.OrderLbl
	if d.Limit > 0 && len(rows) >= d.Limit {
		// The page came back capped, so the list is a top-N and says so: the
		// "1-12/200" readout below counts what is HELD, which would otherwise
		// read as the whole vocabulary.
		title = d.panelTitle() + " · top " + strconv.Itoa(d.Limit) + " by " + d.OrderLbl
	}

	if len(rows) == 0 {
		return c.mark(ZoneBars, style.Render(
			c.titleRule(title, inner, focus)+"\n"+emptyChartFrame(c, inner, h-3)))
	}

	cols := activityColumns(c, inner, d.Pivot == "")
	note := activityNote(c, d, inner)
	fit := h - 2*blockPadY - 1 // the card's padding rows and the titled rule
	if note != "" {
		fit--
	}
	// The column header buys itself a row only where two data rows still fit
	// under it: four right-aligned numeric columns are unreadable without it
	// (calls and tokens are both counts), but a header over one row is worse
	// than the row it displaced.
	header := ""
	if fit >= 3 {
		header = activityHeader(c, d, cols, inner)
		fit--
	}
	if fit < 1 {
		fit = 1
	}
	top, end := barWindow(len(rows), d.Selected, fit)

	head := c.titleRule(title, inner, focus)
	if end-top < len(rows) {
		// Say which slice is on screen; without it a windowed list reads as the
		// whole list. Dropped rather than allowed to crowd the title.
		tail := c.Subtle.Render(strconv.Itoa(top+1) + "-" + strconv.Itoa(end) + "/" + strconv.Itoa(len(rows)))
		if chip := c.titleChip(title, focus); lipgloss.Width(chip)+lipgloss.Width(tail)+2 <= inner {
			head = c.RuleBetween(chip, tail, inner)
		}
	}

	var peak int64
	for _, r := range rows {
		if r.total > peak {
			peak = r.total
		}
	}

	out := make([]string, 0, end-top+1)
	if header != "" {
		out = append(out, header)
	}
	for i, r := range rows[top:end] {
		out = append(out, activityRow(c, r, cols, peak, top+i == d.Selected, top+i))
	}
	content := head + "\n" + strings.Join(out, "\n")
	if note != "" {
		content += "\n" + note
	}
	return c.mark(ZoneBars, style.Render(content))
}

// activityCols is the resolved column budget of one rank row. The ladder is
// name+count (always), then kind and cost, then the composition bar and the
// token count — the same "drop columns before dropping rows" rule Browse
// follows. Widths are display cells, gutters excluded.
type activityCols struct {
	name, kind, calls, bar, tokens, cost int
}

// Fixed cell costs of a rank row: the focus slot and the tool glyph, each with
// its trailing space.
const activityRowFixed = 4

// Column widths. calls/tokens hold a humanized count ("154.6K"), cost a
// formatted amount. The cost column holds the WIDEST bounded form,
// "≥ ~$1234.56" — the bound mark, the provenance mark and the amount — because
// a cost cell that cannot fit its own marks would drop them silently and turn a
// floor into a figure.
const (
	actKindW   = 5
	actCallsW  = 6
	actTokensW = 6
	actCostW   = 11
	actNameMin = 10
)

// activityColumns resolves the row layout for an inner width. The bar is sized
// from the space left after the fixed columns and capped so a wide terminal
// spends its surplus on names — which is where the mcp__server__tool ids that
// dominate the long tail need it — rather than on a longer bar.
//
// showKind is false in a turn-context pivot, and the column is DROPPED there
// rather than filled: every row of a pivot has the same kind — the dimension —
// which the panel title already names, so the column would spend five cells
// repeating one word. Worse, five cells cannot hold it: "mcp_tool" and
// "mcp_server" both truncate to "mcp_s…" and the two partitions would look
// identical in the one column meant to tell them apart.
func activityColumns(c Ctx, inner int, showKind bool) activityCols {
	cost := 0
	if c.Money != nil {
		cost = actCostW
	}
	kind := 0
	kindCost := 0
	if showKind {
		kind = actKindW
		kindCost = 1 + actKindW
	}
	// Widest form: glyph + name + kind + calls + bar + tokens + cost.
	fixed := activityRowFixed + kindCost + 1 + actCallsW + 1 + actTokensW
	if cost > 0 {
		fixed += 1 + cost
	}
	if rest := inner - fixed - 1; rest >= actNameMin+8 {
		bar := rest / 4
		if bar < 8 {
			bar = 8
		}
		if bar > 20 {
			bar = 20
		}
		return activityCols{name: rest - bar, kind: kind, calls: actCallsW, bar: bar, tokens: actTokensW, cost: cost}
	}
	// Middle form: no bar, no token count — the detail card carries both.
	mid := activityRowFixed + kindCost + 1 + actCallsW
	if cost > 0 {
		mid += 1 + cost
	}
	if name := inner - mid; name >= actNameMin {
		return activityCols{name: name, kind: kind, calls: actCallsW, cost: cost}
	}
	// Narrow form: the two facts that are always true — what ran, how often.
	name := inner - activityRowFixed - 1 - actCallsW
	if name < 4 {
		name = 4
	}
	return activityCols{name: name, calls: actCallsW}
}

// activityHeader labels the row columns. It is built from the same resolved
// widths the rows are, so a header can never drift off the numbers under it;
// the columns a width dropped are absent from both. The count column is named
// by the pivot, which is what stops turns from reading as calls.
func activityHeader(c Ctx, d ActivityData, cols activityCols, inner int) string {
	line := c.pad(activityRowFixed) + c.StatLabel.Render(c.PadRight("name", cols.name))
	if cols.kind > 0 {
		line += c.pad(1) + c.StatLabel.Render(c.PadRight("kind", cols.kind))
	}
	line += c.pad(1) + c.StatLabel.Render(c.PadLeft(d.countLabel(), cols.calls))
	if cols.bar > 0 {
		// The bar column is left unlabelled: the series legend on the summary
		// card names its colours, and any word short enough for the narrowest
		// bar (8 cells) would be an abbreviation of "attributed tokens" that the
		// "tokens" header one column right already says in full.
		line += c.pad(1 + cols.bar)
	}
	if cols.tokens > 0 {
		line += c.pad(1) + c.StatLabel.Render(c.PadLeft("tokens", cols.tokens))
	}
	if cols.cost > 0 {
		line += c.pad(1) + c.StatLabel.Render(c.PadLeft("cost", cols.cost))
	}
	if w := inner - lipgloss.Width(line); w > 0 {
		line += c.pad(w)
	}
	return line
}

// activityRow renders one ranked row. Every column that could be read as a
// complete number carries the "?" mark when it is not one.
func activityRow(c Ctx, r invRow, cols activityCols, peak int64, selected bool, idx int) string {
	rc := c
	if selected {
		rc = c.On(ElevRaised)
	}
	unknown := activityUnknown(r)

	glyph := rc.fg(rc.ToolAccent(r.tool)).Render(rc.ToolGlyph(r.tool))
	line := rc.FocusMark(selected) + rc.pad(1) + glyph + rc.pad(1) +
		rc.Stat.Render(rc.PadRight(displayName(rc, r.name, cols.name), cols.name))
	if cols.kind > 0 {
		line += rc.pad(1) + rc.Subtle.Render(rc.PadRight(activityKindLabel(r), cols.kind))
	}
	line += rc.pad(1) + rc.Number.Render(rc.PadLeft(hl(rc, r.count), cols.calls))
	switch {
	case unknown && cols.bar > 0:
		// The bar and the token count are one statement on an unattributed row,
		// so they are written as one: eight cells of bar cannot hold the word,
		// and "?" alone twice over says less than saying it once in full.
		line += rc.pad(1) + rc.Faint.Render(rc.PadRight(
			truncTo(rc, activityUnknownMark+" unattributed", cols.bar+1+cols.tokens), cols.bar+1+cols.tokens))
	case cols.bar > 0:
		line += rc.pad(1) + rc.CompBar(activitySplit(r), peak, cols.bar) +
			rc.pad(1) + rc.Number.Render(rc.PadLeft(hl(rc, r.total), cols.tokens))
	case cols.tokens > 0 && unknown:
		line += rc.pad(1) + rc.Faint.Render(rc.PadLeft(activityUnknownMark, cols.tokens))
	case cols.tokens > 0:
		line += rc.pad(1) + rc.Number.Render(rc.PadLeft(hl(rc, r.total), cols.tokens))
	}
	if cols.cost > 0 {
		line += rc.pad(1) + rc.Number.Render(rc.PadLeft(activityCostForm(rc, r, cols.cost), cols.cost))
	}
	return c.mark(ActZone(idx), line)
}

// activityUnknownMark is what a column shows when the ledger cannot answer it.
// It is deliberately not "0" and not "—": a zero reads as free and a dash reads
// as absent, while the question mark reads as the only true thing — nobody
// knows. A row that carries it renders the mark where the bar would be, rather
// than an empty bar: an empty bar in a column of full ones reads as "this one
// was cheap", which is the exact lie this tab exists to prevent. It is
// deliberately a different treatment from the "∅ zero tokens" bar the
// by-entity panel draws — zero and unknown are different facts.
const activityUnknownMark = "?"

// activityUnknown reports whether NONE of a row's calls joined a usage row,
// which is the case where every token and cost figure on the row is unknowable
// rather than small.
func activityUnknown(r invRow) bool {
	return r.count > 0 && r.unattributed >= r.count
}

// activitySplit projects a row onto the dashboard's token series so the rank
// bars speak the same language as every other bar in the app. input and output
// are real columns; the cache series is the REMAINDER of the total after them,
// because neither ledger carries a cache column of its own and the provider
// total does. The remainder is clamped at zero: a provider whose total excludes
// something the columns include must not render as a negative segment.
func activitySplit(r invRow) Components {
	cache := r.total - r.input - r.output
	if cache < 0 {
		cache = 0
	}
	return Components{Input: r.input, Output: r.output, CacheRead: cache}
}

// activityKindLabel is the kind column's text, padded by the caller. In a
// turn-context pivot the "kind" of every row is the dimension itself, which is
// what keeps the column honest when a page is filtered down to one value.
func activityKindLabel(r invRow) string {
	if r.kind != "" {
		return r.kind
	}
	return "—"
}

// activityCostForm renders a row's cost through the shared bounded renderer.
//
// Both marks apply here and mean what they mean everywhere else: "≥" because
// the figure omits calls whose cost is unknowable (no usage row joined) or
// unpriced, "~" because some of what IS priced was valued from a rate card. A
// row where nothing at all could be priced is not a zero bill but an unknown
// one, and renders as the unpriced mark. Returns "" when no money formatter is
// wired (a partial context), so the caller drops the column rather than
// printing a bare number with no currency.
func activityCostForm(c Ctx, r invRow, w int) string {
	if c.Money == nil {
		return ""
	}
	partial := r.unattributed > 0 || r.unpriced > 0
	known := r.cost > 0 || !partial
	base := c.Money(r.cost, r.computed > 0, known)
	if !known || !partial {
		return base
	}
	// The rider names what is actually missing. A turn context never has an
	// unattributed row — it is keyed on its usage row — so calling its shortfall
	// "unknown" would invent a second failure mode; on the calls pivot, where
	// both can be missing at once, "unknown" is the only word that covers them.
	rider := hl(c, r.unpriced) + " unpriced"
	if r.unattributed > 0 {
		rider = hl(c, r.unpriced+r.unattributed) + " unknown"
	}
	return pickForm([]string{
		boundedMark + " " + base + " · " + rider,
		boundedMark + " " + base,
	}, w)
}

// activityNote is the rank panel's footnote: the volume of calls whose cost is
// unknown, stated in the widest form the pane can hold. It is rendered under
// every geometry that has rows at all — the ranking above it is incomplete
// without it, in the same way a rollup-served summary is incomplete without its
// source label. Empty only when there is genuinely nothing to disclose, which is
// the normal case in a turn-context pivot: a context is only recorded alongside
// an accepted usage event, so there is nothing there to fail to join.
func activityNote(c Ctx, d ActivityData, w int) string {
	t := d.totals()
	unit := d.countLabel()
	if t.unattributed == 0 && t.unpriced == 0 {
		return ""
	}
	var forms []string
	if t.unattributed > 0 {
		share := ""
		if c.Percent != nil {
			share = " (" + c.Percent(t.unattributed, t.count) + ")"
		}
		n := hl(c, t.unattributed)
		forms = append(forms,
			activityUnknownMark+" "+n+" of "+hl(c, t.count)+" "+unit+share+" have no token join — cost unknown, not zero",
			activityUnknownMark+" "+n+" "+unit+share+" unattributed: cost unknown",
			activityUnknownMark+" "+n+" unattributed",
		)
	}
	if t.unpriced > 0 {
		if len(forms) == 0 {
			forms = append(forms,
				activityUnknownMark+" "+hl(c, t.unpriced)+" "+unit+" joined an unpriced usage row",
				activityUnknownMark+" "+hl(c, t.unpriced)+" unpriced")
		} else {
			// Rides along on EVERY rung, not just the widest: a note that drops
			// the unpriced count as the pane narrows discloses less on a small
			// terminal than on a large one, which is the wrong way round.
			rider := " · " + hl(c, t.unpriced) + " unpriced"
			for i := range forms {
				forms[i] += rider
			}
		}
	}
	for _, f := range forms {
		if lipgloss.Width(f) <= w {
			return c.Faint.Render(f)
		}
	}
	return c.Faint.Render(truncTo(c, forms[len(forms)-1], w))
}

// activityDetailCard renders the selected row's full reading: what it is, how
// often it ran, and every number the ledger can and cannot state about it. It is
// a pure projection of the selected row — no query of its own — which is why
// moving the selection on this tab costs nothing.
func activityDetailCard(c Ctx, d ActivityData, rows []invRow, w, h int, focus bool) string {
	elev := paneElev(focus)
	c = c.On(elev)
	style := c.Block(elev).Width(w).Height(maxInt(h, 3))
	inner := w - 2*blockPadX
	if inner < 4 {
		inner = 4
	}
	head := c.titleRule("DETAIL", inner, focus)

	if d.Selected < 0 || d.Selected >= len(rows) {
		return c.mark(ZonePreview, style.Render(head+"\n"+c.Faint.Render("no selection")))
	}
	r := rows[d.Selected]
	comp := activitySplit(r)

	stat := func(label, value string) string {
		return c.StatLabel.Render(c.PadRight(label, 10)) + c.Number.Render(value)
	}
	// Ordered by what a short card must keep. Identity first, then the counts,
	// then what the ledger cannot state about them, and the per-series split
	// last: the split is the one block whose absence costs a reader nothing the
	// rank bar has not already shown.
	lines := []string{
		c.fg(c.ToolAccent(r.tool)).Render(c.ToolGlyph(r.tool)) + c.pad(1) +
			c.Stat.Render(displayName(c, r.name, inner-2)),
		c.Subtle.Render(truncTo(c, activityKindLabel(r)+" · "+r.tool, inner)),
		stat(d.countLabel(), hl(c, r.count)),
		stat("sessions", hl(c, r.sessions)),
	}
	if activityUnknown(r) {
		// Nothing below this point is knowable for this row; say so once instead
		// of printing a column of honest-looking zeros.
		lines = append(lines,
			c.Faint.Render(truncTo(c, activityUnknownMark+" no usage row joins", inner)),
			c.Faint.Render(truncTo(c, "these "+d.countLabel()+": tokens and", inner)),
			c.Faint.Render(truncTo(c, "cost unknown", inner)),
		)
	} else {
		lines = append(lines, stat("tokens", hl(c, r.total)))
		if cost := activityCostForm(c, r, inner-10); cost != "" {
			lines = append(lines, stat("cost", cost))
		}
		if r.unattributed > 0 {
			lines = append(lines, stat("no join", hl(c, r.unattributed)+" "+d.countLabel()))
		}
		if r.unpriced > 0 {
			lines = append(lines, stat("unpriced", hl(c, r.unpriced)+" "+d.countLabel()))
		}
		sum := comp.Sum()
		lines = append(lines, c.Faint.Render(strings.Repeat("─", inner)))
		for _, s := range c.Comp {
			v := s.Pick(comp)
			lines = append(lines, c.compStyle(s).Render(c.PadRight(s.Short, 10))+
				c.Number.Render(hl(c, v)+" ("+pct(c, v, sum)+")"))
		}
	}
	// Trim to the rows the card actually has, trailing ones first. The card is
	// as tall as the rank panel beside it, and a body that overflows it would
	// be cut by the root frame's clamp instead — same rows lost, but chosen by
	// arithmetic rather than by what matters least.
	if avail := h - 2*blockPadY - 1; avail > 0 && len(lines) > avail {
		lines = lines[:avail]
	}
	return c.mark(ZonePreview, style.Render(head+"\n"+strings.Join(lines, "\n")))
}

// pct renders a share through the injected helper when present (headless view
// tests construct partial Ctx values).
func pct(c Ctx, v, total int64) string {
	if c.Percent == nil {
		return ""
	}
	return c.Percent(v, total)
}
