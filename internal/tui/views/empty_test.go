package views

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

func emptyTestCtx() Ctx {
	trunc := func(s string, w int) string {
		r := []rune(s)
		if w < 1 {
			return ""
		}
		if len(r) <= w {
			return s
		}
		return string(r[:w-1]) + "…"
	}
	return Ctx{
		Faint:     lipgloss.NewStyle(),
		Subtle:    lipgloss.NewStyle(),
		GoodColor: compat.AdaptiveColor{Light: lipgloss.Color("#1A7F37"), Dark: lipgloss.Color("#56D364")},
		NowColor:  compat.AdaptiveColor{Light: lipgloss.Color("#B5780A"), Dark: lipgloss.Color("#F2B441")},
		WarnColor: compat.AdaptiveColor{Light: lipgloss.Color("#C0362C"), Dark: lipgloss.Color("#E5534B")},
		Truncate:  trunc,
	}
}

// TestEmptyStatesVisuallyDistinct: the three honest empty treatments must
// never share a rendering — each carries its own glyph, so they stay distinct
// on the text channel alone (monochrome-safe).
func TestEmptyStatesVisuallyDistinct(t *testing.T) {
	c := emptyTestCtx()
	zero := EmptyState(c, EmptyZeroTokens, 40)
	rows := EmptyState(c, EmptyNoRows, 40)
	failed := EmptyState(c, EmptyQueryFailed, 40)

	if !strings.Contains(zero, "∅") || !strings.Contains(zero, "zero tokens") {
		t.Fatalf("zero-tokens treatment missing its glyph channel: %q", zero)
	}
	if !strings.Contains(rows, "◌") || !strings.Contains(rows, "no rows") {
		t.Fatalf("no-rows treatment missing its glyph channel: %q", rows)
	}
	if !strings.Contains(failed, "✕") || !strings.Contains(failed, "query failed") {
		t.Fatalf("query-failed treatment missing its glyph channel: %q", failed)
	}
	if zero == rows || rows == failed || zero == failed {
		t.Fatal("empty treatments are not pairwise distinct")
	}
}

// TestZeroTotals separates "rows exist, zero tokens" from "no rows".
func TestZeroTotals(t *testing.T) {
	if !zeroTotals([]store.Bucket{{Total: 0}, {Total: 0}}) {
		t.Fatal("all-zero rows not detected")
	}
	if zeroTotals([]store.Bucket{{Total: 0}, {Total: 5}}) {
		t.Fatal("nonzero row misclassified as zero tokens")
	}
	if !zeroTotals(nil) {
		t.Fatal("nil rows: zeroTotals is vacuously true by contract")
	}
}

// TestDeltaChipStyleDirections locks the documented Delta contract end to end:
// dir>0 styles in the warm now color, dir<0 in GoodColor (falling spend is
// good — it must NOT render muted), dir==0 stays subtle.
func TestDeltaChipStyleDirections(t *testing.T) {
	c := emptyTestCtx()
	if got := deltaChipStyle(c, 1).GetForeground(); got != c.NowColor {
		t.Fatalf("dir>0 foreground = %v, want NowColor", got)
	}
	if got := deltaChipStyle(c, -1).GetForeground(); got != c.GoodColor {
		t.Fatalf("dir<0 foreground = %v, want GoodColor (was muted)", got)
	}
	if deltaChipStyle(c, 0).GetForeground() == c.GoodColor {
		t.Fatal("dir==0 must not borrow the good color")
	}
}

// TestDetailCardQueryFailedTreatment: a failed trend query renders the ✕
// treatment in the detail card instead of an ambiguous blank strip.
func TestDetailCardQueryFailedTreatment(t *testing.T) {
	c := emptyTestCtx()
	c.PanelTitle = lipgloss.NewStyle()
	c.Stat = lipgloss.NewStyle()
	c.StatLabel = lipgloss.NewStyle()
	c.Number = lipgloss.NewStyle()
	c.Panel = lipgloss.NewStyle()
	c.Focused = lipgloss.NewStyle()
	c.AccentColor = c.NowColor
	c.Humanize = func(int64) string { return "1" }
	c.PadLeft = func(s string, _ int) string { return s }
	c.PadRight = func(s string, _ int) string { return s }
	c.Percent = func(int64, int64) string { return "0%" }
	c.ToolAccent = func(string) compat.AdaptiveColor { return c.NowColor }
	c.ToolGlyph = func(string) string { return "◆" }

	d := byEntityData{
		title:    "BY TOOL",
		dim:      "tool",
		rows:     []store.Bucket{{Keys: map[string]string{"tool": "codex"}, Total: 10}},
		grand:    10,
		selected: 0,
		selErr:   true,
	}
	out := detailCard(c, d, 40, 20, false)
	if !strings.Contains(out, "✕ query failed") {
		t.Fatalf("detail card with selErr missing the query-failed treatment:\n%s", out)
	}
}
