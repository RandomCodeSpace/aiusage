package tui

import "github.com/RandomCodeSpace/aiusage/internal/model"

// pivot.go carries the Activity tab's reading selector. Six readings share one
// tab: the activity ledger's per-invocation view, and the five turn-context
// dimensions.
//
// ONE AT A TIME IS THE POINT. The six are partitions of the same dollars — a
// turn commonly carries three of them at once and every one names its full cost
// — so a screen showing two would report the same tokens twice. The store makes
// the dimension a required argument for exactly that reason; this type is the
// UI half of the same rule, and it is a single scalar rather than a set so
// "show me agents and skills together" is unrepresentable rather than merely
// discouraged.

// ActivityPivot names one reading of the Activity window. The zero value is the
// activity ledger (calls), which is what the tab has always shown.
type ActivityPivot string

// PivotCalls is the activity_events reading: one row per invocation, cost
// divided between the calls that shared a turn. Its empty value is deliberate —
// it is the absence of a turn-context dimension, not a sixth one.
const PivotCalls ActivityPivot = ""

// activityPivotOrder is the cycle the pivot key walks, calls first and then the
// five dimensions in model.TurnDimensions order. Built at init from the store's
// own closed vocabulary rather than restated, so a sixth attribution axis
// arrives on this tab by being added there.
var activityPivotOrder = buildPivotOrder()

func buildPivotOrder() []ActivityPivot {
	dims := model.TurnDimensions()
	out := make([]ActivityPivot, 0, len(dims)+1)
	out = append(out, PivotCalls)
	for _, d := range dims {
		out = append(out, ActivityPivot(d))
	}
	return out
}

// Dimension returns the turn-context dimension this pivot reads, and false for
// PivotCalls, which reads the other ledger entirely. A caller must branch on the
// second result: there is no dimension that means "the activity ledger", and
// inventing one would be the mixing this type exists to prevent.
func (p ActivityPivot) Dimension() (model.TurnDimension, bool) {
	if p == PivotCalls {
		return "", false
	}
	return model.TurnDimension(p), true
}

// Label is the human name for the pivot, plural because it labels a list.
func (p ActivityPivot) Label() string {
	switch p {
	case PivotCalls:
		return "calls"
	case ActivityPivot(model.DimensionAgent):
		return "agents"
	case ActivityPivot(model.DimensionSkill):
		return "skills"
	case ActivityPivot(model.DimensionMCPTool):
		return "mcp tools"
	case ActivityPivot(model.DimensionMCPServer):
		return "mcp servers"
	case ActivityPivot(model.DimensionPlugin):
		return "plugins"
	default:
		return string(p)
	}
}

// Next cycles to the following pivot (wrapping).
func (p ActivityPivot) Next() ActivityPivot {
	for i, v := range activityPivotOrder {
		if v == p {
			return activityPivotOrder[(i+1)%len(activityPivotOrder)]
		}
	}
	return PivotCalls
}

// Key is the stable string persisted across launches. PivotCalls is spelled out
// rather than persisted as the empty string, so a state file that simply lacks
// the field is distinguishable from one that chose calls — both land on calls,
// but only one of them is a decision.
func (p ActivityPivot) Key() string {
	if p == PivotCalls {
		return "calls"
	}
	return string(p)
}

// PivotFromKey parses a persisted pivot key, reporting ok=false for an unknown
// value so the caller can fall back to its default. A dimension this binary no
// longer knows must NOT resolve to a neighbouring one: an unknown key is a
// reading that does not exist, and answering it with a different partition's
// numbers is precisely the confusion the six-way split is guarded against.
func PivotFromKey(k string) (ActivityPivot, bool) {
	for _, v := range activityPivotOrder {
		if v.Key() == k {
			return v, true
		}
	}
	return PivotCalls, false
}
