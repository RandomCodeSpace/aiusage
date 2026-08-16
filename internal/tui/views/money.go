package views

import "charm.land/lipgloss/v2"

// money.go renders every cost TOTAL the dashboard shows, so the two marks a
// summed figure can carry are decided in one place instead of per panel.
//
// The marks answer different questions and both can be true at once:
//
//   - "≥" is a BOUND. The window holds rows nothing could price, so the sum is
//     a floor and not a bill. The rider names how many, because "some of this is
//     missing" and "how much of it" are different facts and a reader deciding
//     whether to trust the number needs the second.
//   - "~" is PROVENANCE, and it is Money's existing `approximate` channel. The
//     sum contains at least one row this project valued off a public rate card
//     rather than reading off the harness (model.PriceProvenance). It is an
//     estimate of the charge, not the charge.
//
// A figure with neither mark is exact and complete: every row in it is priced,
// and every price came from the vendor. That is the only case that renders a
// bare "$1.23", which is what makes the bare form worth reading.
//
// PER-ROW cells are deliberately not routed through here. A single unpriced row
// renders the existing UnpricedMark ("-"): it has no partial sum to bound, and a
// "≥ $0.00" would be a worse lie than a dash.

// boundedMark prefixes a cost total that is a floor rather than a bill. ASCII
// would be ">=", which reads as two characters of code rather than one of
// notation; the rest of the dashboard already spends non-ASCII on ∅, ◷ and ‹›.
const boundedMark = "≥"

// costForms renders a cost total in every form the surface may choose between,
// WIDEST FIRST, on the same contract Span.labelForms and activityNote use:
// every form is complete on its own, so a narrow pane picks a shorter one
// instead of truncating a longer one and leaving a dangling fragment.
//
// micro is the summed cost, unpriced the number of rows in the same window that
// carry none, and computed the number of PRICED rows valued from a rate card.
// It returns nil when no money formatter is wired (the partial Ctx values the
// headless view tests build), so a caller drops the whole cell rather than
// printing a bare number with no currency.
func costForms(c Ctx, micro, unpriced, computed int64) []string {
	if c.Money == nil {
		return nil
	}
	// known is false only when NOTHING in the window could be priced: a zero sum
	// beside rows that carry no cost is an unknown bill, not a free one.
	known := micro > 0 || unpriced == 0
	base := c.Money(micro, computed > 0, known)
	if !known || unpriced <= 0 {
		return []string{base}
	}
	return []string{
		boundedMark + " " + base + " · " + hl(c, unpriced) + " unpriced",
		boundedMark + " " + base,
	}
}

// costValue is costForms' narrowest complete form: the marked amount with no
// rider. It exists for the KPI tile, whose number row is a dozen cells wide and
// whose foot has somewhere better to put the count (costFootForms).
func costValue(c Ctx, micro, unpriced, computed int64) string {
	forms := costForms(c, micro, unpriced, computed)
	if len(forms) == 0 {
		return ""
	}
	return forms[len(forms)-1]
}

// costFootForms names what a cost tile is showing, widest first: the unpriced
// count, then the bare word that the figure is partial, then the plain label.
// The "≥" on the number is what survives every rung — the tile can lose the
// count but never the statement that the number is a floor.
func costFootForms(c Ctx, unpriced, computed int64) []string {
	switch {
	case unpriced > 0:
		return []string{"spend · " + hl(c, unpriced) + " unpriced", "spend (partial)", "spend"}
	case computed > 0:
		return []string{"spend (estimated)", "spend"}
	default:
		return []string{"spend"}
	}
}

// costText picks the widest form that fits w cells. With no form short enough
// the narrowest is returned anyway and the caller's frame clamps it: a clipped
// "≥ $12" still reads as a bounded figure, which is the property that must not
// break.
func costText(c Ctx, micro, unpriced, computed int64, w int) string {
	return pickForm(costForms(c, micro, unpriced, computed), w)
}

// pickForm returns the widest of forms that fits w display cells, or the last
// (narrowest) one when none does. An empty list renders nothing.
func pickForm(forms []string, w int) string {
	if len(forms) == 0 {
		return ""
	}
	for _, f := range forms {
		if lipgloss.Width(f) <= w {
			return f
		}
	}
	return forms[len(forms)-1]
}

// The three aggregate shapes spell their counts differently — a usage bucket
// has UnpricedEvents/ComputedCostEvents, an activity bucket UnpricedCalls (plus
// UnattributedCalls, whose cost is unknowable rather than merely unpriced) and
// ComputedCostCalls, a turn context UnpricedTurns/ComputedCostTurns. They are
// passed in as three numbers rather than behind an interface: the shapes live in
// package store, which cannot grow methods for this package's benefit, and a
// wrapper per shape would be more machinery than the three arguments it hides.
