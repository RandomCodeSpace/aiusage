package views

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// byEntityTestCtx builds a Ctx with real padding/truncation semantics and
// bordered panel styles matching the app (border 1 + Padding(0,1) per side), so
// the rendered widths below measure the same box math as production.
func byEntityTestCtx() Ctx {
	padLeft := func(s string, w int) string {
		if n := utf8.RuneCountInString(s); n < w {
			return strings.Repeat(" ", w-n) + s
		}
		return s
	}
	padRight := func(s string, w int) string {
		if n := utf8.RuneCountInString(s); n < w {
			return s + strings.Repeat(" ", w-n)
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
	// Zero compat.AdaptiveColor values panic inside lipgloss; give every color
	// field a real value.
	ac := func(s string) compat.AdaptiveColor {
		return compat.AdaptiveColor{Light: lipgloss.Color(s), Dark: lipgloss.Color(s)}
	}
	c := Ctx{
		NowColor:    ac("#F2B441"),
		AccentColor: ac("#3DD6E0"),
		FaintColor:  ac("#4A535F"),
		BorderColor: ac("#232B38"),
		GoodColor:   ac("#56D364"),
		WarnColor:   ac("#E5534B"),
		Panel:       lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1),
		Focused:     lipgloss.NewStyle().Border(lipgloss.ThickBorder()).Padding(0, 1),
		PanelTitle:  lipgloss.NewStyle(),
		Stat:        lipgloss.NewStyle(),
		StatLabel:   lipgloss.NewStyle(),
		Subtle:      lipgloss.NewStyle(),
		Number:      lipgloss.NewStyle(),
		Faint:       lipgloss.NewStyle(),
		Comp:        CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("5")),
		Humanize:    func(v int64) string { return "9.9K" },
		PadLeft:     padLeft,
		PadRight:    padRight,
		Truncate:    trunc,
		Percent:     func(v, total int64) string { return "50%" },
		ToolGlyph:   func(string) string { return "◆" },
	}
	c.ToolAccent = func(string) compat.AdaptiveColor { return c.NowColor }
	return c
}

// lineWidths returns the display width of every line in a rendered block.
func lineWidths(s string) []int {
	lines := strings.Split(s, "\n")
	out := make([]int, len(lines))
	for i, ln := range lines {
		out[i] = lipgloss.Width(ln)
	}
	return out
}

// TestByEntityPanelsFillLayoutWidth is the direct width probe for the
// By-Tool/By-Model skeleton: every rendered line of the bars panel must be
// exactly lay.MainW cells, the detail card exactly lay.SideW, and the joined
// frame exactly MainW+gutter+SideW. Reverting the border math (panels 2 cells
// short) previously survived the whole suite because the side-by-side frame
// shrinks with its panels.
func TestByEntityPanelsFillLayoutWidth(t *testing.T) {
	c := byEntityTestCtx()
	rows := []store.Bucket{
		{Keys: map[string]string{"tool": "claude-code"}, Events: 3, Input: 10, Output: 5, CacheRead: 20, Total: 35},
		{Keys: map[string]string{"tool": "codex"}, Events: 1, Input: 4, Output: 2, Total: 6},
		{Keys: map[string]string{"tool": "gemini"}, Total: 0}, // zero-token row path
	}
	trend := []store.Bucket{{Total: 5}, {Total: 9}}

	for _, w := range []int{80, 100, 120, 200} {
		lay := ComputeLayout(w, 40)
		if !lay.SidePanel {
			t.Fatalf("w=%d: layout unexpectedly lost the side panel", w)
		}

		d := byEntityData{
			title:    "BY TOOL · 30d",
			dim:      "tool",
			rows:     rows,
			grand:    41,
			selected: 0,
			selTrend: trend,
			selSess:  2,
		}

		bars := barsPanel(c, d, lay.MainW, lay.BodyH, true)
		for i, lw := range lineWidths(bars) {
			if lw != lay.MainW {
				t.Fatalf("w=%d: barsPanel line %d is %d cells, want exactly MainW=%d", w, i, lw, lay.MainW)
			}
		}

		detail := detailCard(c, d, lay.SideW, lay.BodyH, false)
		for i, lw := range lineWidths(detail) {
			if lw != lay.SideW {
				t.Fatalf("w=%d: detailCard line %d is %d cells, want exactly SideW=%d", w, i, lw, lay.SideW)
			}
		}

		frame := byEntity(c, d, lay)
		wantFrame := lay.MainW + 1 + lay.SideW
		for i, lw := range lineWidths(frame) {
			if lw != wantFrame {
				t.Fatalf("w=%d: byEntity frame line %d is %d cells, want %d (MainW+gutter+SideW)", w, i, lw, wantFrame)
			}
		}
	}

	// Narrow terminal: no side panel — the bars panel must fill MainW alone.
	lay := ComputeLayout(60, 40)
	if lay.SidePanel {
		t.Fatal("w=60: expected single-column layout")
	}
	d := byEntityData{title: "BY TOOL", dim: "tool", rows: rows, grand: 41}
	frame := byEntity(c, d, lay)
	for i, lw := range lineWidths(frame) {
		if lw != lay.MainW {
			t.Fatalf("w=60: single-column frame line %d is %d cells, want MainW=%d", i, lw, lay.MainW)
		}
	}
}
