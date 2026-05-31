package tui

import (
	"regexp"
	"strings"
	"testing"
)

var ansiFill = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestPanelsFillBodyHeight guards the big-screen responsiveness fix: every
// tab's main bordered panel must extend to (within a row or two of) the bottom
// of the body, instead of rendering as a short box that leaves a large band of
// empty terminal below it. Regression target: panels that sized their box to
// content height and ignored the body height they were handed.
func TestPanelsFillBodyHeight(t *testing.T) {
	for _, sz := range [][2]int{{120, 30}, {160, 40}, {200, 50}} {
		for _, tab := range []struct{ key, name string }{
			{"1", "Overview"}, {"2", "ByTool"}, {"3", "ByModel"}, {"4", "Sessions"},
		} {
			m := newTestModelWH(t, &fakeData{}, sz[0], sz[1])
			m = send(m, keyMsg(tab.key))
			clean := ansiFill.ReplaceAllString(m.View(), "")
			lines := strings.Split(strings.TrimRight(clean, "\n"), "\n")

			// Lowest line carrying a panel bottom-border glyph.
			lastBorder := -1
			for i, ln := range lines {
				if strings.ContainsAny(ln, "┗┛╰╯") {
					lastBorder = i
				}
			}
			if lastBorder < 0 {
				t.Errorf("%s @%dx%d: no panel bottom border found", tab.name, sz[0], sz[1])
				continue
			}
			// The footer is the last line; the main panel's bottom border should sit
			// just above it. A large gap means the panel floated short above empty
			// terminal (the bug). Allow a 2-line slack for footer + spacer.
			gap := len(lines) - 1 - lastBorder
			if gap > 2 {
				t.Errorf("%s @%dx%d: %d empty rows below panel (border at line %d of %d) — not filling body height",
					tab.name, sz[0], sz[1], gap, lastBorder+1, len(lines))
			}
		}
	}
}
