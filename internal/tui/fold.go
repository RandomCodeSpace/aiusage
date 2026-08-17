package tui

import "github.com/RandomCodeSpace/aiusage/store"

// fold.go collapses the by-tool long tail. Sixteen tool ids are supported and a
// real machine runs several of them for a few hundred tokens a week, so an
// unfolded by-tool list spends most of its rows on tools that are, at the
// resolution the panel can draw, indistinguishable from zero — while the two or
// three that matter get paged off the bottom.
//
// THE THRESHOLD IS A SHARE OF THE SELECTED WINDOW, NOT A RANK. A fixed "top 5"
// would hide a tool that took a third of a quiet day and show four that took
// nothing on a busy one; a share threshold makes the list a function of what
// actually happened in the window on screen. It is the SAME denominator the
// share column beside each row is computed against (grandOf), so a row reading
// "<1%" is exactly a row the fold would have taken — the panel cannot show a
// number that disagrees with its own folding rule.
//
// The consequence is the one the feature is for: a tool that crosses 1% in some
// window surfaces there by itself, with no list to maintain and no setting to
// find.

// foldThresholdPercent is the share below which a tool joins the fold. It is
// stated in percent and applied as an integer comparison (Total*100 < grand) so
// the rule matches format.Percent's "<1%" rung exactly, with no float rounding
// standing between what a row says and whether it is folded.
const foldThresholdPercent = 1

// foldMinRows is the smallest tail worth folding. Collapsing ONE tool into a
// row that says "1 others" costs a row, saves a row, and hides a name for
// nothing; two is where the trade starts paying.
const foldMinRows = 2

// FoldResult describes a folded row list: the rows to render, which of them is
// the synthetic fold row, and how many real tools it stands for.
//
// Index is -1 when nothing was folded, which is the only signal a renderer
// needs — there is no sentinel value in the bucket keys for a caller to sniff
// for, and therefore none for a caller to mistake a real tool named "" for.
type FoldResult struct {
	Rows  []store.Bucket
	Index int
	Count int
}

// foldMinorTools splits rows into the tools that clear foldThresholdPercent of
// grand and the tail that does not, and returns the list to render.
//
// Collapsed, the tail becomes ONE synthetic bucket appended after the major
// rows, carrying the tail's real summed counts — the fold row is a total, not a
// placeholder, so the panel still adds up to the window. Expanded, that same
// row stays where it is and the tail follows it: it is the control that
// collapses the list again, and a control that vanishes when used is a control
// a reader cannot find.
//
// Sessions is deliberately left at zero on the fold row. A distinct-session
// count does not add across buckets, and summing the tail's would over-count
// every session that used two of the folded tools. Nothing renders it on this
// row; a zero that is never read beats a number that is wrong.
func foldMinorTools(rows []store.Bucket, grand int64, expanded bool) FoldResult {
	none := FoldResult{Rows: rows, Index: -1}
	if grand <= 0 || len(rows) == 0 {
		return none
	}
	major := make([]store.Bucket, 0, len(rows))
	minor := make([]store.Bucket, 0, len(rows))
	for _, b := range rows {
		if isMinorShare(b.Total, grand) {
			minor = append(minor, b)
			continue
		}
		major = append(major, b)
	}
	if len(minor) < foldMinRows {
		return none
	}

	out := make([]store.Bucket, 0, len(rows)+1)
	out = append(out, major...)
	idx := len(out)
	out = append(out, sumBuckets(minor))
	if expanded {
		out = append(out, minor...)
	}
	return FoldResult{Rows: out, Index: idx, Count: len(minor)}
}

// isMinorShare reports whether value is strictly under foldThresholdPercent of
// grand, in integer arithmetic. Token totals reach ~1e10 on a real ledger and
// the multiplier is 100, so the product stays four orders of magnitude inside
// int64.
func isMinorShare(value, grand int64) bool {
	return value*100 < grand*foldThresholdPercent
}

// sumBuckets adds the summable fields of a group of buckets into one. Keys is
// left nil: the fold row names no tool, and giving it a made-up key would let a
// drill or a filter act on a tool that does not exist.
func sumBuckets(bs []store.Bucket) store.Bucket {
	var out store.Bucket
	for _, b := range bs {
		out.Events += b.Events
		out.Input += b.Input
		out.Output += b.Output
		out.CacheCreation += b.CacheCreation
		out.CacheRead += b.CacheRead
		out.Reasoning += b.Reasoning
		out.Total += b.Total
		out.CostMicroUSD += b.CostMicroUSD
		out.UnpricedEvents += b.UnpricedEvents
		out.ComputedCostEvents += b.ComputedCostEvents
	}
	return out
}
