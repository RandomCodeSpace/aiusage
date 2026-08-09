package tui

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// mono_test.go is the monochrome-legibility gate for the visual design language
// (issues #20 / #22). Every main surface must carry its FULL meaning through the
// glyph channel alone: with all SGR sequences stripped, a reader must still be
// able to tell which tab is active, which pane has focus, which window is shown,
// what the chart's scale is, whether a number rose or fell, and why a panel is
// empty. Color is decoration on top of that channel — never the channel itself.
//
// This is what makes "borders retreat, elevation and paint do the work" safe:
// paint is invisible in monochrome, so the focus bar, the chip markers and the
// titled rules are what actually carry the structure.

var ansiMono = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// monoFrame renders m and strips every SGR sequence — the monochrome channel.
func monoFrame(m Model) string { return ansiMono.ReplaceAllString(m.View().Content, "") }

// emptySource is a DataSource with no rows at all, for the empty-state channel.
type emptySource struct{}

func (emptySource) Summarize(_ context.Context, fl store.Filter) (*store.Summary, error) {
	return &store.Summary{GroupBy: fl.GroupBy}, nil
}

// containsAny reports whether s contains at least one of the alternatives.
func containsAny(s string, alts ...string) bool {
	for _, a := range alts {
		if strings.Contains(s, a) {
			return true
		}
	}
	return false
}

// TestMonoPaneLabelsAndScaleReadouts: pane identity and the hero's declared
// scale are text, so they survive the strip. The scale is the only thing that
// makes two independently detented panes comparable.
func TestMonoPaneLabelsAndScaleReadouts(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	out := monoFrame(m)

	for _, want := range []string{"TREND", "BY TOOL", "SPLIT", "SCALE ", "/div"} {
		if !strings.Contains(out, want) {
			t.Errorf("mono Overview is missing %q:\n%s", want, out)
		}
	}
}

// TestMonoFocusIndication: the focused pane is marked by the width-invariant
// focus bar, not by a border glyph set or a fill color. Exactly one pane per
// view may wear it, and it must sit directly on that pane's title.
func TestMonoFocusIndication(t *testing.T) {
	for _, tc := range []struct{ key, title string }{
		{"1", "TREND"},
		{"2", "BY TOOL"},
		{"3", "BY MODEL"},
		{"4", "TOOL"}, // Browse opens on the first drill dimension
	} {
		m := newTestModelWH(t, &fakeData{}, 160, 44)
		m = step(t, m, keyMsg(tc.key))
		out := monoFrame(m)
		if !strings.Contains(out, views.FocusBar+" "+tc.title) {
			t.Errorf("tab %s: focused pane %q carries no focus bar %q in mono:\n%s",
				tc.key, tc.title, views.FocusBar, out)
		}
	}
}

// TestMonoFocusBarIsWidthInvariant: the focus marker is a fixed two-cell slot
// at every pane width — a bar, never a rule that grows with the pane — and the
// unfocused slot is exactly as wide, so nothing reflows when focus moves.
func TestMonoFocusBarIsWidthInvariant(t *testing.T) {
	for _, w := range []int{100, 140, 200} {
		m := newTestModelWH(t, &fakeData{}, w, 44)
		out := monoFrame(m)
		if !strings.Contains(out, views.FocusBar+" TREND") {
			t.Errorf("w=%d: the focused pane's marker is not a single bar + space:\n%s", w, out)
		}
		if strings.Contains(out, views.FocusBar+views.FocusBar) {
			t.Errorf("w=%d: the focus marker repeated — it must not scale with the pane", w)
		}

		// Rows: the selected one wears the bar, the rest a blank cell of the same
		// width, so every row's name stays in the same column.
		bt := monoFrame(step(t, m, keyMsg("2")))
		cols := map[int]bool{}
		for _, name := range []string{"claude-code", "codex"} {
			for _, ln := range strings.Split(bt, "\n") {
				if i := strings.Index(ln, name); i >= 0 {
					cols[lipgloss.Width(ln[:i])] = true // display column, not byte offset
					break
				}
			}
		}
		if len(cols) != 1 {
			t.Errorf("w=%d: selected and unselected rows start in different columns %v — the focus slot reflows",
				w, cols)
		}
	}
}

// TestMonoActiveTabChip: which tab is active must be readable without color.
// The tab chip's marker slot is the channel; it is width-invariant so the strip
// does not reflow when the selection moves.
func TestMonoActiveTabChip(t *testing.T) {
	for _, tc := range []struct{ key, label string }{
		{"1", "Overview"}, {"2", "By Tool"}, {"3", "By Model"}, {"4", "Sessions"},
	} {
		m := newTestModelWH(t, &fakeData{}, 160, 44)
		m = step(t, m, keyMsg(tc.key))
		out := monoFrame(m)
		marked := 0
		for _, meta := range viewList {
			if strings.Contains(out, views.FocusBar+" "+meta.glyph+" "+meta.label) {
				marked++
				if meta.label != tc.label {
					t.Errorf("tab %s: %q is marked active but %q was selected", tc.key, meta.label, tc.label)
				}
			}
		}
		if marked != 1 {
			t.Errorf("tab %s: %d tab chips carry the active marker, want exactly 1:\n%s", tc.key, marked, out)
		}
	}
}

// TestMonoStateAndRangeChips: the freshness state and the shown window are
// chips; their glyph+word pair is what survives.
func TestMonoStateAndRangeChips(t *testing.T) {
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	out := monoFrame(m)

	if !containsAny(out, "live", "sync", "stale", "cold") {
		t.Errorf("no freshness state word in the mono frame:\n%s", out)
	}
	if !strings.Contains(out, "‹ "+m.spanLabel()+" ›") {
		t.Errorf("range chip does not name the window %q in mono:\n%s", m.spanLabel(), out)
	}
	if !strings.Contains(out, "? help") {
		t.Errorf("the help action chip did not survive the strip:\n%s", out)
	}
	// The sort chip is a press target (issue #23), so what it will do has to be
	// readable before pressing it — with colour gone, the word is all there is.
	if !strings.Contains(out, "sort "+m.sort.Label()) {
		t.Errorf("the sort action chip did not survive the strip:\n%s", out)
	}
}

// rowPrefix returns the text left of name in the row that carries it: the
// SHORTEST such prefix in the frame. Row surfaces are the left-hand column of
// every side-by-side frame, while the detail/preview pane on the right echoes
// the selected name far into the line — and both land on the same rendered line
// once the two panes are joined, so "first line containing it" is not enough.
func rowPrefix(frame, name string) (string, bool) {
	best, found := "", false
	for _, ln := range strings.Split(frame, "\n") {
		i := strings.Index(ln, name)
		if i < 0 {
			continue
		}
		if !found || i < len(best) {
			best, found = ln[:i], true
		}
	}
	return best, found
}

// TestMonoDrillChevron: the drill affordance is a glyph, so a reader on a
// monochrome terminal can still see WHICH rows descend before pressing one
// (issue #24 — and an underline was rejected because in a terminal it reads as
// "URL", not "button"). It sits in the row's leading gutter on every surface
// with rows, and disappears at the deepest drill level, where nothing descends.
func TestMonoDrillChevron(t *testing.T) {
	for _, tc := range []struct{ tab, name string }{
		{"2", "claude-code"}, // By-Tool bars
		{"2", "codex"},
		{"3", "claude-opus"}, // By-Model bars
		{"4", "claude-code"}, // Browse table
		{"4", "codex"},
	} {
		m := newTestModelWH(t, &fakeData{}, 160, 44)
		m = step(t, m, keyMsg(tc.tab))
		pre, ok := rowPrefix(monoFrame(m), tc.name)
		if !ok {
			t.Fatalf("tab %s: no row for %q on screen", tc.tab, tc.name)
		}
		if !strings.Contains(pre, views.Chevron) {
			t.Errorf("tab %s: row %q carries no drill chevron %q in mono (prefix %q)",
				tc.tab, tc.name, views.Chevron, pre)
		}
	}

	// Deepest Browse level: sessions have nothing under them, so the affordance
	// must not claim otherwise. The slot stays reserved — the columns hold — it
	// just goes blank.
	m := newTestModelWH(t, &fakeData{}, 160, 44)
	m = step(t, m, keyMsg("4"))
	for i := 0; i < len(drillDims)-1; i++ {
		m = step(t, m, keyMsg("enter"))
	}
	if m.browseDrillable() {
		t.Fatalf("setup: %d crumbs is not the deepest drill level", len(m.crumbs))
	}
	pre, ok := rowPrefix(monoFrame(m), "sess-1")
	if !ok {
		t.Fatalf("no session row on screen at the deepest level (crumbs %v)", m.crumbs)
	}
	if strings.Contains(pre, views.Chevron) {
		t.Errorf("the deepest Browse level still advertises a drill: prefix %q", pre)
	}
}

// (The drill slot's width invariance is proved where the data can be held
// constant across the toggle: TestBrowseDrillSlotIsWidthInvariant in package
// views. Drilling changes the grouping dimension too, so comparing two depths
// here would measure the tool glyph, not the slot.)

// TestMonoDeltaDirection: a KPI's direction is a glyph, never a color.
func TestMonoDeltaDirection(t *testing.T) {
	out := monoFrame(newTestModelWH(t, &fakeData{}, 160, 44))
	if !containsAny(out, "▲ ", "▼ ", "· —", "= 0") {
		t.Errorf("no delta direction glyph in the mono frame:\n%s", out)
	}
}

// TestMonoEmptyStates: the three honest empty treatments are glyph+word, so an
// empty range still says why it is empty with color stripped.
func TestMonoEmptyStates(t *testing.T) {
	m := newTestModelWH(t, emptySource{}, 160, 44)
	out := monoFrame(m)
	if !strings.Contains(out, "no rows in range") {
		t.Errorf("empty range lost its treatment in mono:\n%s", out)
	}
}

// TestBordersRetreatToTheAppFrame: the design language allows exactly ONE
// border — the outer app frame. Cards carry elevation and titled rules instead,
// so any per-panel box drawing is a regression.
func TestBordersRetreatToTheAppFrame(t *testing.T) {
	for _, tab := range []string{"1", "2", "3", "4"} {
		m := newTestModelWH(t, &fakeData{}, 160, 44)
		m = step(t, m, keyMsg(tab))
		out := monoFrame(m)
		for _, corner := range []string{"╭", "╮", "╰", "╯"} {
			if n := strings.Count(out, corner); n != 1 {
				t.Errorf("tab %s: %d %q corners, want exactly 1 (the app frame):\n%s", tab, n, corner, out)
			}
		}
		for _, thick := range []string{"┏", "┓", "┗", "┛", "┃"} {
			if strings.Contains(out, thick) {
				t.Errorf("tab %s: a thick panel border %q survives — focus is a bar, not a box", tab, thick)
			}
		}
	}
}

// TestMonoTitledRules: panel titles are titled rules, so the pane's extent is
// readable without a box. The rule must run from the title to the pane edge.
func TestMonoTitledRules(t *testing.T) {
	out := monoFrame(newTestModelWH(t, &fakeData{}, 160, 44))
	found := false
	for _, ln := range strings.Split(out, "\n") {
		if i := strings.Index(ln, "TREND"); i >= 0 && strings.Contains(ln[i:], "──") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the TREND pane has no titled rule:\n%s", out)
	}
}
