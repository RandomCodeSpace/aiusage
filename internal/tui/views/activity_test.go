package views

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// activityTestCtx builds a headless Ctx with real formatters, so the numbers
// the honesty assertions look for actually reach the frame.
func activityTestCtx() Ctx {
	pad := func(s string, w int) string {
		if n := utf8.RuneCountInString(s); n < w {
			return s + strings.Repeat(" ", w-n)
		}
		return s
	}
	padL := func(s string, w int) string {
		if n := utf8.RuneCountInString(s); n < w {
			return strings.Repeat(" ", w-n) + s
		}
		return s
	}
	trunc := func(s string, w int) string {
		if w < 1 {
			return ""
		}
		r := []rune(s)
		if len(r) <= w {
			return s
		}
		return string(r[:w-1]) + "…"
	}
	ac := func(s string) compat.AdaptiveColor {
		return compat.AdaptiveColor{Light: lipgloss.Color(s), Dark: lipgloss.Color(s)}
	}
	c := Ctx{
		NowColor:    ac("#F2B441"),
		AccentColor: ac("#3DD6E0"),
		FaintColor:  ac("#4A535F"),
		GoodColor:   ac("#56D364"),
		WarnColor:   ac("#E5534B"),
		PanelTitle:  lipgloss.NewStyle(),
		Stat:        lipgloss.NewStyle(),
		StatLabel:   lipgloss.NewStyle(),
		Subtle:      lipgloss.NewStyle(),
		Number:      lipgloss.NewStyle(),
		Faint:       lipgloss.NewStyle(),
		Comp:        CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("5")),
		Humanize:    func(v int64) string { return humanTest(v) },
		PadLeft:     padL,
		PadRight:    pad,
		Truncate:    trunc,
		Percent: func(v, total int64) string {
			if total == 0 {
				return "0%"
			}
			return itoaTest(v*100/total) + "%"
		},
		Money:     model.FormatCost,
		ToolGlyph: func(string) string { return "◆" },
	}
	c.ToolAccent = func(string) compat.AdaptiveColor { return c.NowColor }
	return c
}

func itoaTest(v int64) string {
	if v == 0 {
		return "0"
	}
	var d []byte
	for v > 0 {
		d = append([]byte{byte('0' + v%10)}, d...)
		v /= 10
	}
	return string(d)
}

func humanTest(v int64) string {
	if v < 1000 {
		return itoaTest(v)
	}
	return itoaTest(v/1000) + "K"
}

// activityTestData is a page with the real ledger's awkward shape: an
// attributed row, a partially unpriced one, and a large fully unattributed one
// whose cost is unknown rather than zero.
func activityTestData() ActivityData {
	row := func(name, kind, tool string, calls, tokens, cost, unattributed, unpriced int64) store.ActivityBucket {
		return store.ActivityBucket{
			Keys:                   map[string]string{"name": name, "kind": kind, "tool": tool},
			OrderedKeys:            []string{"name", "kind", "tool"},
			Calls:                  calls,
			Sessions:               calls / 3,
			AttributedInput:        tokens / 3,
			AttributedOutput:       tokens / 3,
			AttributedTotal:        tokens,
			AttributedCostMicroUSD: cost,
			UnattributedCalls:      unattributed,
			UnpricedCalls:          unpriced,
		}
	}
	rows := []store.ActivityBucket{
		row("exec", "tool", "codex", 79093, 0, 0, 79093, 0),
		row("Bash", "tool", "claude-code", 20412, 4_000_000, 6_200_000, 0, 0),
		row("mcp__server__a_very_long_tool_name_that_keeps_going", "tool", "claude-code", 812, 900_000, 100_000, 0, 40),
		row("code-review", "skill", "claude-code", 8, 400_000, 900_000, 0, 0),
		row("PreToolUse", "hook", "claude-code", 2135, 0, 0, 2135, 0),
	}
	return ActivityData{
		Rows: rows,
		Kinds: []store.ActivityBucket{
			{Keys: map[string]string{"kind": "tool"}, Calls: 100317},
			{Keys: map[string]string{"kind": "skill"}, Calls: 8},
			{Keys: map[string]string{"kind": "hook"}, Calls: 2135},
		},
		Calls: []store.ActivityBucket{
			{Keys: map[string]string{"day": "2026-05-28"}, Calls: 40000},
			{Keys: map[string]string{"day": "2026-05-29"}, Calls: 62460},
		},
		CallsDim: "day",
		Totals: store.ActivityBucket{
			Calls: 102460, Sessions: 318, AttributedTotal: 5_300_000,
			AttributedCostMicroUSD: 7_200_000, UnattributedCalls: 81228, UnpricedCalls: 40,
		},
		RangeLbl: "7d", OrderLbl: "calls", Limit: 200,
	}
}

// widths returns the display width of every line in a rendered block.
func widths(s string) []int {
	lines := strings.Split(s, "\n")
	out := make([]int, len(lines))
	for i, l := range lines {
		out[i] = lipgloss.Width(l)
	}
	return out
}

// The frame must never overflow its region at any geometry — including the
// widths where the long mcp__ names are far wider than the name column.
func TestActivityFitsEveryGeometry(t *testing.T) {
	c := activityTestCtx()
	d := activityTestData()
	for _, geo := range []struct{ w, h int }{
		{40, 10}, {48, 12}, {56, 14}, {74, 18}, {80, 24}, {100, 28}, {120, 40}, {200, 50},
	} {
		lay := ComputeLayout(geo.w, geo.h)
		if lay.TooSmall {
			continue
		}
		out := Activity(c, d, lay)
		for i, w := range widths(out) {
			if w > lay.BodyW {
				t.Errorf("%dx%d: line %d is %d cells, body is %d", geo.w, geo.h, i, w, lay.BodyW)
			}
		}
		if h := lipgloss.Height(out); h > lay.BodyH {
			t.Errorf("%dx%d: frame is %d rows, body is %d", geo.w, geo.h, h, lay.BodyH)
		}
	}
}

// The unattributed volume must be legible in the rendered frame at every
// geometry: it is what qualifies every cost number above it.
func TestActivityStatesUnattributedVolume(t *testing.T) {
	c := activityTestCtx()
	d := activityTestData()
	for _, geo := range []struct{ w, h int }{{40, 12}, {74, 18}, {100, 28}, {160, 40}} {
		lay := ComputeLayout(geo.w, geo.h)
		out := Activity(c, d, lay)
		if !strings.Contains(out, "81K") {
			t.Errorf("%dx%d: frame does not state the unattributed call count:\n%s", geo.w, geo.h, out)
		}
		if !strings.Contains(out, "unattributed") && !strings.Contains(out, "no token join") {
			t.Errorf("%dx%d: frame does not name the unattributed calls:\n%s", geo.w, geo.h, out)
		}
	}
}

// A fully unattributed row must never render a cost of zero: the ledger does
// not know what those calls cost, and "$0.00" is a different claim.
func TestActivityUnknownCostIsNotZero(t *testing.T) {
	c := activityTestCtx()
	d := activityTestData()
	d.Rows = d.Rows[:1] // the codex row: 79,093 calls, none of them joined
	out := Activity(c, d, ComputeLayout(120, 30))
	if strings.Contains(out, "$0.00") {
		t.Errorf("unattributed row rendered a zero cost:\n%s", out)
	}
	if !strings.Contains(out, model.UnpricedMark) {
		t.Errorf("unattributed row does not carry the unpriced mark:\n%s", out)
	}
	if !strings.Contains(out, "? unattributed") {
		t.Errorf("unattributed row does not carry the ? treatment:\n%s", out)
	}
}

// A row with SOME unattributed or unpriced calls is an understatement, and says
// so with the approximate mark rather than presenting a partial bill as exact.
func TestActivityPartialCostIsMarkedApproximate(t *testing.T) {
	c := activityTestCtx()
	partial := store.ActivityBucket{
		Keys:  map[string]string{"name": "n", "kind": "tool", "tool": "t"},
		Calls: 100, AttributedTotal: 1000, AttributedCostMicroUSD: 2_000_000, UnpricedCalls: 5,
	}
	exact := partial
	exact.UnpricedCalls = 0
	if got := activityCostText(c, partial); !strings.HasPrefix(got, "~") {
		t.Errorf("partial cost = %q, want the approximate mark", got)
	}
	if got := activityCostText(c, exact); strings.HasPrefix(got, "~") {
		t.Errorf("complete cost = %q, want no approximate mark", got)
	}
}

// An empty ledger (a fresh database) renders the honest no-rows treatment, not
// a broken frame.
func TestActivityEmptyRenders(t *testing.T) {
	c := activityTestCtx()
	lay := ComputeLayout(100, 30)
	out := Activity(c, ActivityData{RangeLbl: "7d", OrderLbl: "calls"}, lay)
	if !strings.Contains(out, "no rows in range") {
		t.Errorf("empty activity frame missing the no-rows treatment:\n%s", out)
	}
	for i, w := range widths(out) {
		if w > lay.BodyW {
			t.Fatalf("empty frame line %d is %d cells, body is %d", i, w, lay.BodyW)
		}
	}
}

// The token split projects the real attributed columns and derives cache as the
// remainder, never a negative segment.
func TestActivitySplitClampsDerivedCache(t *testing.T) {
	got := activitySplit(store.ActivityBucket{AttributedInput: 10, AttributedOutput: 20, AttributedTotal: 100})
	if got.Input != 10 || got.Output != 20 || got.CacheRead != 70 {
		t.Errorf("split = %+v, want input 10 output 20 cache 70", got)
	}
	if got := activitySplit(store.ActivityBucket{AttributedInput: 90, AttributedOutput: 90, AttributedTotal: 100}); got.CacheRead != 0 {
		t.Errorf("derived cache = %d, want it clamped to 0", got.CacheRead)
	}
}
