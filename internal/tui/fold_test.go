package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/store"
)

var ansiFold = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// toolRow builds one by-tool bucket with a given total.
func toolRow(name string, total int64) store.Bucket {
	return store.Bucket{
		Keys:        map[string]string{"tool": name},
		OrderedKeys: []string{"tool"},
		Events:      total / 1000,
		Input:       total / 2,
		Output:      total / 2,
		Total:       total,
	}
}

// The threshold is a SHARE of the window, not a rank: everything under 1% of the
// grand total folds, however many rows that is, and everything at or above it
// stays.
func TestFoldTakesEverythingUnderOnePercent(t *testing.T) {
	rows := []store.Bucket{
		toolRow("big", 900_000),
		toolRow("mid", 80_000),
		toolRow("exactly-one-percent", 10_000),
		toolRow("small-a", 5_000),
		toolRow("small-b", 4_000),
		toolRow("small-c", 1_000),
	}
	const grand = 1_000_000

	got := foldMinorTools(rows, grand, false)
	if got.Count != 3 {
		t.Fatalf("folded %d tools, want 3 (the three under 1%%)", got.Count)
	}
	if got.Index != 3 {
		t.Errorf("fold row at %d, want 3 (after the three major tools)", got.Index)
	}
	if n := len(got.Rows); n != 4 {
		t.Errorf("collapsed list has %d rows, want 4", n)
	}
	// A row sitting exactly on the threshold is NOT under it, and the panel's own
	// share column would read "1%" for it — the fold and the number agree.
	for _, b := range got.Rows[:3] {
		if b.Keys["tool"] == "" {
			t.Fatal("a major row lost its name")
		}
	}
	if want := int64(10_000); got.Rows[2].Total != want {
		t.Errorf("the 1%% row was folded; row 2 total = %d, want %d", got.Rows[2].Total, want)
	}
}

// The fold row is a TOTAL, not a placeholder: the panel still adds up to the
// window whether the tail is open or shut.
func TestFoldRowCarriesTheTailsRealNumbers(t *testing.T) {
	rows := []store.Bucket{toolRow("big", 990_000), toolRow("a", 5_000), toolRow("b", 4_000), toolRow("c", 1_000)}
	got := foldMinorTools(rows, 1_000_000, false)
	if got.Index < 0 {
		t.Fatal("nothing folded")
	}
	fold := got.Rows[got.Index]
	if want := int64(10_000); fold.Total != want {
		t.Errorf("fold total = %d, want %d", fold.Total, want)
	}
	if want := int64(10); fold.Events != want {
		t.Errorf("fold events = %d, want %d", fold.Events, want)
	}
	// A distinct-session count does not add across buckets, so the fold row
	// deliberately carries none rather than an over-counted one.
	if fold.Sessions != 0 {
		t.Errorf("fold sessions = %d, want 0 — distinct counts do not add", fold.Sessions)
	}
	if fold.Keys != nil {
		t.Errorf("fold row carries keys %v; it names no tool and must not pretend to", fold.Keys)
	}
}

// Expanded, the fold row STAYS and the tail follows it. A control that vanishes
// when used is a control the reader cannot use again.
func TestExpandedFoldKeepsItsControlRow(t *testing.T) {
	rows := []store.Bucket{toolRow("big", 990_000), toolRow("a", 5_000), toolRow("b", 4_000), toolRow("c", 1_000)}
	open := foldMinorTools(rows, 1_000_000, true)
	if open.Index != 1 {
		t.Fatalf("fold row at %d, want 1", open.Index)
	}
	if n := len(open.Rows); n != 5 {
		t.Fatalf("expanded list has %d rows, want 5 (major + fold + 3 minor)", n)
	}
	for i, want := range []string{"big", "", "a", "b", "c"} {
		if got := open.Rows[i].Keys["tool"]; got != want {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
}

// One small tool is not a tail. Collapsing it costs a row, saves a row and hides
// a name for nothing.
func TestFoldRefusesASingleMinorTool(t *testing.T) {
	rows := []store.Bucket{toolRow("big", 995_000), toolRow("lonely", 5_000)}
	got := foldMinorTools(rows, 1_000_000, false)
	if got.Index != -1 || got.Count != 0 {
		t.Errorf("folded a single minor tool: index=%d count=%d", got.Index, got.Count)
	}
	if len(got.Rows) != len(rows) {
		t.Errorf("row list changed for a refused fold: %d rows, want %d", len(got.Rows), len(rows))
	}
}

// THE POINT OF A SHARE THRESHOLD: a tool that is invisible over a wide window
// surfaces by itself in a window where it mattered. No list to maintain, no
// setting to find.
func TestSmallToolSurfacesWhenItCrossesTheThreshold(t *testing.T) {
	// Wide window: "occasional" is 0.5% of the total and folds.
	wide := []store.Bucket{toolRow("main", 995_000), toolRow("occasional", 5_000), toolRow("rare", 3_000)}
	if got := foldMinorTools(wide, 1_003_000, false); got.Count != 2 {
		t.Fatalf("wide window folded %d tools, want 2", got.Count)
	}

	// Narrow window, same tool, 5% of a quiet day: it stands on its own.
	narrow := []store.Bucket{toolRow("main", 95_000), toolRow("occasional", 5_000), toolRow("rare", 300)}
	got := foldMinorTools(narrow, 100_300, false)
	if got.Count != 0 || got.Index != -1 {
		t.Fatalf("narrow window folded %d tools (index %d); only 'rare' is under 1%% and one is not a tail",
			got.Count, got.Index)
	}
	names := make([]string, 0, len(got.Rows))
	for _, b := range got.Rows {
		names = append(names, b.Keys["tool"])
	}
	if !contains(names, "occasional") {
		t.Errorf("the tool that crossed 1%% did not surface: rows = %v", names)
	}
}

// An unknown denominator cannot produce a share, so nothing folds. Folding
// against a zero grand total would collapse the entire list.
func TestFoldNeedsADenominator(t *testing.T) {
	rows := []store.Bucket{toolRow("a", 1), toolRow("b", 1), toolRow("c", 1)}
	if got := foldMinorTools(rows, 0, false); got.Index != -1 {
		t.Errorf("folded against a zero grand total: %+v", got)
	}
}

// Enter on the fold row TOGGLES it and must not drill: the row names no tool, so
// descending would open a Sessions list filtered to the empty name, which reads
// as "these tools have no sessions".
func TestEnterOnTheFoldRowTogglesInsteadOfDrilling(t *testing.T) {
	m := newTestModelWH(t, &wideData{}, 160, 44)
	m = step(t, m, keyMsg("2"))
	if m.byTool.FoldIndex < 0 {
		t.Fatal("no fold row on a 40-tool list")
	}
	shut := len(m.byTool.Rows)

	m.byTool.Selected = m.byTool.FoldIndex
	m = step(t, m, keyMsg("enter"))

	if len(m.crumbs) != 0 {
		t.Fatalf("Enter on the fold row drilled: crumbs = %v", m.crumbs)
	}
	if m.view != ViewByTool {
		t.Fatalf("Enter on the fold row left the tab: view = %v", m.view)
	}
	if !m.byTool.FoldOpen {
		t.Fatal("Enter on the fold row did not expand it")
	}
	if len(m.byTool.Rows) <= shut {
		t.Errorf("expanded list has %d rows, want more than the collapsed %d", len(m.byTool.Rows), shut)
	}
	if m.byTool.Selected != m.byTool.FoldIndex {
		t.Errorf("selection moved off the fold row to %d; a second press must be possible",
			m.byTool.Selected)
	}

	// And back.
	m = step(t, m, keyMsg("enter"))
	if m.byTool.FoldOpen {
		t.Error("a second Enter did not collapse the fold")
	}
	if len(m.byTool.Rows) != shut {
		t.Errorf("collapsed list has %d rows, want the original %d", len(m.byTool.Rows), shut)
	}
}

// Enter on a REAL tool row still drills. The special case must be the fold row
// and nothing else.
func TestEnterOnAToolRowStillDrills(t *testing.T) {
	m := newTestModelWH(t, &wideData{}, 160, 44)
	m = step(t, m, keyMsg("2"))
	m.byTool.Selected = 0
	m = step(t, m, keyMsg("enter"))
	if len(m.crumbs) != 1 || m.crumbs[0].Dim != "tool" || m.crumbs[0].Value != "tool-00" {
		t.Fatalf("Enter on a tool row produced crumbs %v, want [tool:tool-00]", m.crumbs)
	}
}

// Expanding must not query. The rows are already loaded; the toggle changes what
// is SHOWN, not what was asked for.
func TestExpandingTheFoldRunsNoQueries(t *testing.T) {
	src := &wideData{}
	m := newTestModelWH(t, src, 160, 44)
	m = step(t, m, keyMsg("2"))
	m.byTool.Selected = m.byTool.FoldIndex

	if n := queriesDuring(&src.fakeData, func() { m = step(t, m, keyMsg("enter")) }); n != 0 {
		t.Errorf("expanding the fold ran %d store queries, want 0", n)
	}
}

// Collapsed is the default on every load: the fold is a reading aid for the list
// in front of you, and a load replaces that list.
func TestALoadResetsTheFoldToCollapsed(t *testing.T) {
	m := newTestModelWH(t, &wideData{}, 160, 44)
	m = step(t, m, keyMsg("2"))
	m.byTool.Selected = m.byTool.FoldIndex
	m = step(t, m, keyMsg("enter"))
	if !m.byTool.FoldOpen {
		t.Fatal("the fold did not open")
	}
	m = step(t, m, keyMsg("t")) // cycle range: a fresh load
	if m.byTool.FoldOpen {
		t.Error("the fold survived a load; every load must land collapsed")
	}
}

// The By-Tool frame must name the fold and its aggregate at wide AND narrow
// geometries — a fold that hides rows without saying how many is just missing
// data.
func TestFoldRowRendersAtEveryGeometry(t *testing.T) {
	for _, geo := range []struct{ w, h int }{{80, 24}, {100, 30}, {120, 40}, {160, 44}, {200, 50}} {
		m := newTestModelWH(t, &wideData{}, geo.w, geo.h)
		m = step(t, m, keyMsg("2"))
		if m.byTool.FoldIndex < 0 {
			t.Fatalf("%dx%d: nothing folded", geo.w, geo.h)
		}
		// Put the fold row's page on screen.
		m.byTool.Selected = m.byTool.FoldIndex
		out := ansiFold.ReplaceAllString(m.View().Content, "")
		want := fmt.Sprintf("%d oth", m.byTool.FoldCount)
		if !strings.Contains(out, want) {
			t.Errorf("%dx%d: frame does not name the fold (%q):\n%s", geo.w, geo.h, want, out)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
