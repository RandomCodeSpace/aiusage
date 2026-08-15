package views

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// activity.go renders the Activity tab: what the agent actually DID — which
// tools it called, which skills and hooks fired, how often, and what that cost.
// It reads the activity ledger (store.ActivityBucket), never usage_events.
//
// The whole surface is built around one fact the other tabs never have to
// state: a call's cost can be UNKNOWN. An activity row names the usage row its
// turn produced, and where that join does not exist — codex records its calls
// and its token counts in unrelated records, hooks carry no usage at all — the
// tokens are not zero, they are unknowable from this table. On this machine
// that is roughly half of all rows, so a ranking that let those read as free
// would put the busiest tool in the dashboard at the bottom of the cost list.
// Every place a number could be mistaken for a complete one therefore carries
// the "?" mark, and the rank panel's footnote states the volume outright at
// every geometry — it is the one line that is never dropped.

// ActivityData feeds the Activity view. Rows is the ranked (name, kind, tool)
// page; Kinds and Calls are the summary card's two readings of the same window;
// Totals is the grand total the footnote quantifies against.
type ActivityData struct {
	Rows     []store.ActivityBucket // ranked page, in display order
	Kinds    []store.ActivityBucket // grouped by kind: tool / skill / hook
	Calls    []store.ActivityBucket // per hour/day call counts, ascending
	CallsDim string                 // the Calls bucket key ("hour" / "day")
	Totals   store.ActivityBucket   // grand total over the window

	Selected   int    // index of the selected/focused row
	RangeLbl   string // the window the tab is showing
	OrderLbl   string // the metric Rows is ranked by ("calls" / "cost" / …)
	Limit      int    // the cap Rows came back under (0 = uncapped)
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

	if !lay.SidePanel {
		return join(activityRankPanel(c, d, width, bodyH, d.ActivePane == PaneActivityRank))
	}
	rank := activityRankPanel(c, d, lay.MainW, bodyH, d.ActivePane == PaneActivityRank)
	detail := activityDetailCard(c, d, lay.SideW, bodyH, d.ActivePane == PaneActivityDetail)
	return join(lipgloss.JoinHorizontal(lipgloss.Top, rank, " ", detail))
}

// activitySummaryCard is the read-only header: the kind breakdown (what KIND of
// thing ran), one line of coverage totals, and — where the body can pay for a
// row — a heat strip of call volume over the window, drawn with the same ramp
// the KPI tiles use.
func activitySummaryCard(c Ctx, d ActivityData, width int, lay Layout) string {
	c = c.On(ElevCard)
	inner := width - 2*blockPadX
	if inner < 8 {
		inner = 8
	}
	card := c.Block(ElevCard).Width(width)
	chip := c.titleChip("ACTIVITY · "+d.RangeLbl, false)
	title := c.Rule(chip, inner)

	if d.Totals.Calls == 0 {
		return card.Render(title + "\n" + EmptyState(c, EmptyNoRows, inner))
	}
	// The series legend hangs off the end of the rule, where it names the
	// colours of the rank bars below — the same legend, and the same three
	// series, the rest of the dashboard uses.
	if legend := c.CompLegend(); lipgloss.Width(chip)+lipgloss.Width(legend)+2 <= inner {
		title = c.RuleBetween(chip, legend, inner)
	}

	lines := []string{title, activityKindRow(c, d, inner), activityTotalsRow(c, d, inner)}
	if lay.Sparklines && !lay.Dense {
		lines = append(lines, activityCallStrip(c, d, inner))
	}
	return card.Render(strings.Join(lines, "\n"))
}

// activityCallStrip is the call-volume heat row: one cell per time bucket,
// newest at the right, on the same six-rung ramp the KPI tiles use. It is
// LABELLED with its cadence — a bare strip of shades cannot say whether a cell
// is an hour or a day, and the two read identically.
func activityCallStrip(c Ctx, d ActivityData, inner int) string {
	vals := make([]float64, len(d.Calls))
	for i, b := range d.Calls {
		vals[i] = float64(b.Calls)
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

// activityKindRow renders the tool / skill / hook breakdown. The kind word is
// the whole channel — no glyph — because the three words are unambiguous and
// survive a monochrome terminal on their own.
func activityKindRow(c Ctx, d ActivityData, inner int) string {
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

// activityTotalsRow states the window's coverage in one line: how many calls
// ran, over how many sessions, how many tokens and how much spend could be
// attributed to them. The unattributed volume is NOT here — it is the rank
// panel's footnote, which no geometry drops.
func activityTotalsRow(c Ctx, d ActivityData, inner int) string {
	t := d.Totals
	stat := func(label string, value string) string {
		return c.StatLabel.Render(label) + c.pad(1) + c.Number.Render(value)
	}
	parts := []string{
		stat("calls", hl(c, t.Calls)),
		stat("sessions", hl(c, t.Sessions)),
		stat("tokens", hl(c, t.AttributedTotal)),
	}
	if cost := activityCostText(c, t); cost != "" {
		parts = append(parts, stat("cost", cost))
	}
	return c.fitParts(parts, c.pad(3), inner)
}

// activityRankPanel renders the ranked invocations. It is the tab's one
// interactive pane: the selection moves here and the detail card follows it.
//
// Rows are windowed to what the card holds, on the same contract as the
// by-entity bars (barsPanel): rendering the whole list and letting the frame
// clamp the overflow would take the hidden rows' click zones — and this panel's
// own closing zone marker — with it.
func activityRankPanel(c Ctx, d ActivityData, w, h int, focus bool) string {
	elev := paneElev(focus)
	c = c.On(elev)
	style := c.Block(elev).Width(w).Height(maxInt(h, 3))
	inner := w - 2*blockPadX
	if inner < 8 {
		inner = 8
	}
	title := "INVOCATIONS · by " + d.OrderLbl
	if d.Limit > 0 && len(d.Rows) >= d.Limit {
		// The page came back capped, so the list is a top-N and says so: the
		// "1-12/200" readout below counts what is HELD, which would otherwise
		// read as the whole vocabulary.
		title = "INVOCATIONS · top " + strconv.Itoa(d.Limit) + " by " + d.OrderLbl
	}

	if len(d.Rows) == 0 {
		return c.mark(ZoneBars, style.Render(
			c.titleRule(title, inner, focus)+"\n"+emptyChartFrame(c, inner, h-3)))
	}

	cols := activityColumns(c, inner)
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
		header = activityHeader(c, cols, inner)
		fit--
	}
	if fit < 1 {
		fit = 1
	}
	top, end := barWindow(len(d.Rows), d.Selected, fit)

	head := c.titleRule(title, inner, focus)
	if end-top < len(d.Rows) {
		// Say which slice is on screen; without it a windowed list reads as the
		// whole list. Dropped rather than allowed to crowd the title.
		tail := c.Subtle.Render(strconv.Itoa(top+1) + "-" + strconv.Itoa(end) + "/" + strconv.Itoa(len(d.Rows)))
		if chip := c.titleChip(title, focus); lipgloss.Width(chip)+lipgloss.Width(tail)+2 <= inner {
			head = c.RuleBetween(chip, tail, inner)
		}
	}

	var peak int64
	for _, b := range d.Rows {
		if b.AttributedTotal > peak {
			peak = b.AttributedTotal
		}
	}

	rows := make([]string, 0, end-top+1)
	if header != "" {
		rows = append(rows, header)
	}
	for i, b := range d.Rows[top:end] {
		rows = append(rows, activityRow(c, b, cols, peak, top+i == d.Selected, top+i))
	}
	content := head + "\n" + strings.Join(rows, "\n")
	if note != "" {
		content += "\n" + note
	}
	return c.mark(ZoneBars, style.Render(content))
}

// activityCols is the resolved column budget of one rank row. The ladder is
// name+calls (always), then kind and cost, then the composition bar and the
// token count — the same "drop columns before dropping rows" rule Browse
// follows. Widths are display cells, gutters excluded.
type activityCols struct {
	name, kind, calls, bar, tokens, cost int
}

// Fixed cell costs of a rank row: the focus slot and the tool glyph, each with
// its trailing space.
const activityRowFixed = 4

// Column widths. calls/tokens hold a humanized count ("154.6K"), cost a
// formatted amount ("~$1234.56" at its widest realistic).
const (
	actKindW   = 5
	actCallsW  = 6
	actTokensW = 6
	actCostW   = 9
	actNameMin = 10
)

// activityColumns resolves the row layout for an inner width. The bar is sized
// from the space left after the fixed columns and capped so a wide terminal
// spends its surplus on names — which is where the mcp__server__tool ids that
// dominate the long tail need it — rather than on a longer bar.
func activityColumns(c Ctx, inner int) activityCols {
	cost := 0
	if c.Money != nil {
		cost = actCostW
	}
	// Widest form: glyph + name + kind + calls + bar + tokens + cost.
	fixed := activityRowFixed + 1 + actKindW + 1 + actCallsW + 1 + actTokensW
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
		return activityCols{name: rest - bar, kind: actKindW, calls: actCallsW, bar: bar, tokens: actTokensW, cost: cost}
	}
	// Middle form: no bar, no token count — the detail card carries both.
	mid := activityRowFixed + 1 + actKindW + 1 + actCallsW
	if cost > 0 {
		mid += 1 + cost
	}
	if name := inner - mid; name >= actNameMin {
		return activityCols{name: name, kind: actKindW, calls: actCallsW, cost: cost}
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
// the columns a width dropped are absent from both.
func activityHeader(c Ctx, cols activityCols, inner int) string {
	line := c.pad(activityRowFixed) + c.StatLabel.Render(c.PadRight("name", cols.name))
	if cols.kind > 0 {
		line += c.pad(1) + c.StatLabel.Render(c.PadRight("kind", cols.kind))
	}
	line += c.pad(1) + c.StatLabel.Render(c.PadLeft("calls", cols.calls))
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

// activityRow renders one ranked invocation. Every column that could be read as
// a complete number carries the "?" mark when it is not one.
func activityRow(c Ctx, b store.ActivityBucket, cols activityCols, peak int64, selected bool, idx int) string {
	rc := c
	if selected {
		rc = c.On(ElevRaised)
	}
	tool := b.Keys["tool"]
	name := b.Keys["name"]
	unknown := activityUnknown(b)

	glyph := rc.fg(rc.ToolAccent(tool)).Render(rc.ToolGlyph(tool))
	line := rc.FocusMark(selected) + rc.pad(1) + glyph + rc.pad(1) +
		rc.Stat.Render(rc.PadRight(displayName(rc, name, cols.name), cols.name))
	if cols.kind > 0 {
		line += rc.pad(1) + rc.Subtle.Render(rc.PadRight(activityKindLabel(b), cols.kind))
	}
	line += rc.pad(1) + rc.Number.Render(rc.PadLeft(hl(rc, b.Calls), cols.calls))
	switch {
	case unknown && cols.bar > 0:
		// The bar and the token count are one statement on an unattributed row,
		// so they are written as one: eight cells of bar cannot hold the word,
		// and "?" alone twice over says less than saying it once in full.
		line += rc.pad(1) + rc.Faint.Render(rc.PadRight(
			truncTo(rc, activityUnknownMark+" unattributed", cols.bar+1+cols.tokens), cols.bar+1+cols.tokens))
	case cols.bar > 0:
		line += rc.pad(1) + rc.CompBar(activitySplit(b), peak, cols.bar) +
			rc.pad(1) + rc.Number.Render(rc.PadLeft(hl(rc, b.AttributedTotal), cols.tokens))
	case cols.tokens > 0 && unknown:
		line += rc.pad(1) + rc.Faint.Render(rc.PadLeft(activityUnknownMark, cols.tokens))
	case cols.tokens > 0:
		line += rc.pad(1) + rc.Number.Render(rc.PadLeft(hl(rc, b.AttributedTotal), cols.tokens))
	}
	if cols.cost > 0 {
		line += rc.pad(1) + rc.Number.Render(rc.PadLeft(activityCostText(rc, b), cols.cost))
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

// activityUnknown reports whether NONE of a bucket's calls joined a usage row,
// which is the case where every token and cost figure on the row is unknowable
// rather than small.
func activityUnknown(b store.ActivityBucket) bool {
	return b.Calls > 0 && b.UnattributedCalls >= b.Calls
}

// activitySplit projects an activity bucket onto the dashboard's token series
// so the rank bars speak the same language as every other bar in the app.
// input and output are real attributed columns; the cache series is the
// REMAINDER of the attributed total after them, because activity_events carries
// no cache column of its own and the provider total does. The remainder is
// clamped at zero: a provider whose total excludes something the columns
// include must not render as a negative segment.
func activitySplit(b store.ActivityBucket) Components {
	cache := b.AttributedTotal - b.AttributedInput - b.AttributedOutput
	if cache < 0 {
		cache = 0
	}
	return Components{Input: b.AttributedInput, Output: b.AttributedOutput, CacheRead: cache}
}

// activityKindLabel is the kind column's text, padded by the caller.
func activityKindLabel(b store.ActivityBucket) string {
	if k := b.Keys["kind"]; k != "" {
		return k
	}
	return "—"
}

// activityCostText renders a bucket's attributed cost through the shared money
// formatter, marked for what it leaves out. A bucket with unattributed or
// unpriced calls is APPROXIMATE (the "~" prefix); one where nothing at all
// could be priced is not a zero bill but an unknown one, and renders as the
// unpriced mark. Returns "" when no money formatter is wired (a partial
// context), so the caller drops the column rather than printing a bare number
// with no currency.
func activityCostText(c Ctx, b store.ActivityBucket) string {
	if c.Money == nil {
		return ""
	}
	partial := b.UnattributedCalls > 0 || b.UnpricedCalls > 0
	known := b.AttributedCostMicroUSD > 0 || !partial
	return c.Money(b.AttributedCostMicroUSD, partial, known)
}

// activityNote is the rank panel's footnote: the volume of calls whose cost is
// unknown, stated in the widest form the pane can hold. It is rendered under
// every geometry that has rows at all — the ranking above it is incomplete
// without it, in the same way a rollup-served summary is incomplete without its
// source label. Empty only when there is genuinely nothing to disclose.
func activityNote(c Ctx, d ActivityData, w int) string {
	t := d.Totals
	if t.UnattributedCalls == 0 && t.UnpricedCalls == 0 {
		return ""
	}
	var forms []string
	if t.UnattributedCalls > 0 {
		share := ""
		if c.Percent != nil {
			share = " (" + c.Percent(t.UnattributedCalls, t.Calls) + ")"
		}
		n := hl(c, t.UnattributedCalls)
		forms = append(forms,
			activityUnknownMark+" "+n+" of "+hl(c, t.Calls)+" calls"+share+" have no token join — cost unknown, not zero",
			activityUnknownMark+" "+n+" calls"+share+" unattributed: cost unknown",
			activityUnknownMark+" "+n+" unattributed",
		)
	}
	if t.UnpricedCalls > 0 {
		if len(forms) == 0 {
			forms = append(forms,
				activityUnknownMark+" "+hl(c, t.UnpricedCalls)+" calls joined an unpriced usage row",
				activityUnknownMark+" "+hl(c, t.UnpricedCalls)+" unpriced")
		} else {
			// Rides along on EVERY rung, not just the widest: a note that drops
			// the unpriced count as the pane narrows discloses less on a small
			// terminal than on a large one, which is the wrong way round.
			rider := " · " + hl(c, t.UnpricedCalls) + " unpriced"
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

// activityDetailCard renders the selected invocation's full reading: what it
// is, how often it ran, and every number the ledger can and cannot state about
// it. It is a pure projection of the selected row — no query of its own — which
// is why moving the selection on this tab costs nothing.
func activityDetailCard(c Ctx, d ActivityData, w, h int, focus bool) string {
	elev := paneElev(focus)
	c = c.On(elev)
	style := c.Block(elev).Width(w).Height(maxInt(h, 3))
	inner := w - 2*blockPadX
	if inner < 4 {
		inner = 4
	}
	head := c.titleRule("DETAIL", inner, focus)

	if d.Selected < 0 || d.Selected >= len(d.Rows) {
		return c.mark(ZonePreview, style.Render(head+"\n"+c.Faint.Render("no selection")))
	}
	b := d.Rows[d.Selected]
	tool := b.Keys["tool"]
	comp := activitySplit(b)

	stat := func(label, value string) string {
		return c.StatLabel.Render(c.PadRight(label, 10)) + c.Number.Render(value)
	}
	// Ordered by what a short card must keep. Identity first, then the counts,
	// then what the ledger cannot state about them, and the per-series split
	// last: the split is the one block whose absence costs a reader nothing the
	// rank bar has not already shown.
	lines := []string{
		c.fg(c.ToolAccent(tool)).Render(c.ToolGlyph(tool)) + c.pad(1) +
			c.Stat.Render(displayName(c, b.Keys["name"], inner-2)),
		c.Subtle.Render(truncTo(c, activityKindLabel(b)+" · "+tool, inner)),
		stat("calls", hl(c, b.Calls)),
		stat("sessions", hl(c, b.Sessions)),
	}
	if activityUnknown(b) {
		// Nothing below this point is knowable for this row; say so once instead
		// of printing a column of honest-looking zeros.
		lines = append(lines,
			c.Faint.Render(truncTo(c, activityUnknownMark+" no usage row joins", inner)),
			c.Faint.Render(truncTo(c, "these calls: tokens and", inner)),
			c.Faint.Render(truncTo(c, "cost unknown", inner)),
		)
	} else {
		lines = append(lines, stat("tokens", hl(c, b.AttributedTotal)))
		if cost := activityCostText(c, b); cost != "" {
			lines = append(lines, stat("cost", cost))
		}
		if b.UnattributedCalls > 0 {
			lines = append(lines, stat("no join", hl(c, b.UnattributedCalls)+" calls"))
		}
		if b.UnpricedCalls > 0 {
			lines = append(lines, stat("unpriced", hl(c, b.UnpricedCalls)+" calls"))
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
