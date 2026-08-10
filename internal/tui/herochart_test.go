package tui

import (
	"regexp"
	"strings"
	"testing"
)

// herochart_test.go is the model-level acceptance for issue #48: the hero chart
// must survive the classic 80x24 terminal, and no terminal may lose the chart
// that a NARROWER one renders.

var ansiHeroChart = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// hasBrailleFrame reports whether the frame carries braille (U+2800..U+28FF) -
// the signature of a built ntcharts body, as opposed to a block sparkline strip.
func hasBrailleFrame(s string) bool {
	for _, r := range s {
		if r >= 0x2800 && r <= 0x28FF {
			return true
		}
	}
	return false
}

// heroCharted renders m and reports whether the Overview hero is a BUILT chart:
// braille under a pane that declares its scale in text. Both channels are
// required - the KPI tiles carry block sparklines, so braille alone would also
// match a hero that degraded to its strip.
func heroCharted(m Model) (bool, string) {
	out := ansiHeroChart.ReplaceAllString(m.View().Content, "")
	return hasBrailleFrame(out) && strings.Contains(out, "SCALE "), out
}

// TestClassicTerminalRendersTheHeroChart: 80x24 is the classic terminal size and
// the most-hit layout in the project. It used to get no hero chart at any
// height: the layout granted ChartFull on a 51-column primary column, then the
// hero re-tested the 47 columns the card chrome left against the same 48, and
// the KPI strip claimed its rows before the hero had a floor.
func TestClassicTerminalRendersTheHeroChart(t *testing.T) {
	for _, h := range []int{24, 30, 40, 50} {
		m := newTestModelWH(t, &fakeData{}, 80, h)
		charted, out := heroCharted(m)
		if !charted {
			t.Fatalf("80x%d renders no built hero chart:\n%s", h, out)
		}
	}
}

// TestHeroChartIsMonotonicInWidth: widening a terminal never takes the hero
// chart away. That is the general form of the double-charged width budget -
// 70 and 90 columns charted while 80 did not - so it is checked across the whole
// width sweep rather than at the three sizes the issue happened to name.
func TestHeroChartIsMonotonicInWidth(t *testing.T) {
	for _, h := range []int{24, 30, 44} {
		charted := -1
		for w := 42; w <= 160; w += 2 {
			m := newTestModelWH(t, &fakeData{}, w, h)
			got, out := heroCharted(m)
			if got && charted < 0 {
				charted = w
				continue
			}
			if !got && charted >= 0 {
				t.Fatalf("%dx%d renders no hero chart although the narrower %dx%d does:\n%s",
					w, h, charted, h, out)
			}
		}
		if charted < 0 {
			t.Fatalf("no width in [42,160] charts the hero at height %d", h)
		}
	}
}
