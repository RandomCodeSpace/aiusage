package views

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// heroBody must return EXACTLY the requested height for every size, so a chart
// pane never pushes its panel one line past its budget. This guards the ntcharts
// trailing-row quirk that fitHeight corrects (the frame-level clamp in render.go
// would mask, not prevent, the per-panel overflow).
func TestHeroBodyExactHeight(t *testing.T) {
	c := Ctx{
		Comp:     CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("5")),
		Humanize: func(n int64) string { return "9.4M" },
		Truncate: func(s string, w int) string { return s },
		Faint:    lipgloss.NewStyle(), Subtle: lipgloss.NewStyle(),
	}
	var tl []store.Bucket
	for i := 0; i < 7; i++ {
		tl = append(tl, store.Bucket{
			Keys:  map[string]string{"day": "2026-05-2" + string(rune('0'+i))},
			Input: 100000, Output: 50000, CacheRead: 9000000, CacheCreation: 300000, Total: 9450000,
		})
	}
	lay := ComputeLayout(120, 32)
	for _, w := range []int{48, 66, 100, 116} {
		for _, h := range []int{5, 6, 9, 12, 20, 29} {
			got := len(strings.Split(heroBody(c, tl, "day", lay, w, h, 6), "\n"))
			if got != h {
				t.Errorf("heroBody(w=%d,h=%d) = %d lines, want exactly %d", w, h, got, h)
			}
		}
	}
}
