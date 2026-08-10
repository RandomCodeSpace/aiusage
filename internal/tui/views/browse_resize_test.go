package views

import (
	"testing"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// browseResizeCtx is a renderable Ctx: View() styles colours, and a zero-value
// compat.AdaptiveColor panics inside Render, so the colour fields must be set.
func browseResizeCtx() Ctx {
	return Ctx{
		Comp:        CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("5")),
		Faint:       lipgloss.NewStyle(),
		Subtle:      lipgloss.NewStyle(),
		AccentColor: compat.AdaptiveColor{Light: lipgloss.Color("#0969DA"), Dark: lipgloss.Color("#58A6FF")},
		GoodColor:   compat.AdaptiveColor{Light: lipgloss.Color("#1A7F37"), Dark: lipgloss.Color("#56D364")},
		NowColor:    compat.AdaptiveColor{Light: lipgloss.Color("#B5780A"), Dark: lipgloss.Color("#F2B441")},
		WarnColor:   compat.AdaptiveColor{Light: lipgloss.Color("#C0362C"), Dark: lipgloss.Color("#E5534B")},
		Humanize:    func(v int64) string { return "0" },
		PadLeft:     func(s string, w int) string { return s },
		Truncate:    func(s string, w int) string { return s },
	}
}

// A resize that widens the table swaps the narrow 3-column layout for the full
// per-component breakdown. bubbles v2 SetColumns renders the viewport as a side
// effect, against whatever rows the table is still holding - so the wider column
// set indexes cells the old rows do not have and panics inside renderRow. That
// is a hard crash of the whole TUI, reachable by nothing more exotic than
// changing the terminal font size.
//
// The sweep runs both directions across the breakpoint, because only widening
// panics: narrowing leaves the surplus cells unread.
func TestBrowseResizeAcrossColumnBreakpointDoesNotPanic(t *testing.T) {
	c := browseResizeCtx()
	rows := []store.Bucket{
		{Keys: map[string]string{"session": "s1"}, Events: 3, Input: 1, Output: 1, CacheRead: 1, Total: 3},
		{Keys: map[string]string{"session": "s2"}, Events: 1, Input: 1, Output: 1, Total: 2},
	}
	cell := lipgloss.NewStyle().PaddingRight(1)

	b := NewBrowse(c)
	b.ApplyStyles(cell, cell, cell)
	b.SetData(c, "session", rows, 5)

	// Ascending then descending, so every adjacent pair of widths is crossed in
	// both directions. 53x27 is the size reported by the crash.
	widths := []int{53, 60, 72, 90, 110, 140, 180, 140, 110, 90, 72, 60, 53}
	for _, w := range widths {
		// SetLayout is the whole reproduction: SetColumns updates the widget's
		// viewport synchronously, so the bad render happens inside this call -
		// exactly where the production stack trace lands.
		b.SetLayout(ComputeLayout(w, 27))
	}
}

// The invariant behind the fix, stated directly: whatever the layout does to the
// column set, every row the table holds must carry exactly one cell per column.
// A mismatch is a panic waiting for the next render.
func TestBrowseRowArityMatchesColumnsAtEveryWidth(t *testing.T) {
	c := browseResizeCtx()
	rows := []store.Bucket{
		{Keys: map[string]string{"session": "s1"}, Events: 3, Input: 1, Output: 1, CacheRead: 1, Total: 3},
		{Keys: map[string]string{"session": "s2"}, Events: 1, Input: 1, Output: 1, Total: 2},
	}
	cell := lipgloss.NewStyle().PaddingRight(1)

	b := NewBrowse(c)
	b.ApplyStyles(cell, cell, cell)
	b.SetData(c, "session", rows, 5)

	for _, w := range []int{40, 53, 60, 72, 90, 110, 140, 180, 200} {
		b.SetLayout(ComputeLayout(w, 27))
		wantCells := len(b.cols)
		if wantCells == 0 {
			t.Fatalf("w=%d: no columns applied", w)
		}
		for i, row := range b.table.Rows() {
			if len(row) != wantCells {
				t.Errorf("w=%d row %d: %d cells against %d columns; the next render panics",
					w, i, len(row), wantCells)
			}
		}
	}
}
