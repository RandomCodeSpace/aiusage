package views

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"aiusage/internal/store"
)

var ansiBrowseTest = regexp.MustCompile("\x1b\\[[0-9;]*m")

// browseTableMaxLine returns the widest display line of the raw bubbles table
// (no surrounding panel), used to detect column overflow.
func browseTableMaxLine(b Browse) int {
	clean := ansiBrowseTest.ReplaceAllString(b.table.View(), "")
	mx := 0
	for _, ln := range strings.Split(clean, "\n") {
		if w := lipgloss.Width(ln); w > mx {
			mx = w
		}
	}
	return mx
}

// TestBrowseTableFitsPanel guards the column-width math: the bubbles table must
// render no wider than the panel's text area (panel total - border 2 - padding
// 2), or lipgloss word-wraps the trailing "total" column onto its own line
// (the "tools and total in the same column" bug). Checked across widths and for
// both the full per-component and the narrow 3-column layouts.
func TestBrowseTableFitsPanel(t *testing.T) {
	c := Ctx{
		Comp:     CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("5")),
		Humanize: func(v int64) string { return "0" },
		PadLeft:  func(s string, w int) string { return s },
		Truncate: func(s string, w int) string { return s },
		Panel:    lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1),
		Focused:  lipgloss.NewStyle().Border(lipgloss.ThickBorder()).Padding(0, 1),
	}
	rows := []store.Bucket{
		{Keys: map[string]string{"model": "claude-haiku-4-5-20251001"}, Events: 3, Input: 1, Output: 1, CacheRead: 1, Total: 3},
		{Keys: map[string]string{"model": "gpt-5"}, Events: 1, Input: 1, Output: 1, Total: 2},
	}
	// The column math reserves one gutter cell per column, matching the app's
	// per-cell PaddingRight(1) styles (app.go buildCtx). The test must apply the
	// same styles, or the bubbles default Padding(0,1) adds a second gutter per
	// column and the width budget no longer holds.
	cell := lipgloss.NewStyle().PaddingRight(1)
	for _, w := range []int{40, 56, 64, 80, 100, 120, 160, 200} {
		lay := ComputeLayout(w, 40)
		b := NewBrowse()
		b.ApplyStyles(cell, cell, cell)
		b.SetData(c, "model", rows, 5)
		b.SetLayout(lay)

		// The panel's usable text area = on-screen panel width - border(2) - pad(2).
		content := b.tablePanelW() - 4
		if got := browseTableMaxLine(b); got > content {
			t.Errorf("w=%d: table renders %d cells wide but panel text area is only %d — trailing column will wrap",
				w, got, content)
		}
	}
}
