package tui

import "github.com/RandomCodeSpace/aiusage/internal/tui/views"

// View identifies the active tab. The four views form the command-center spine;
// 1..4 (and clicking a top tab) select them directly, and Tab / Shift+Tab cycle
// through them. There is no within-view pane cycling: each view has exactly one
// interactive surface (see interactivePane), and every other panel is read-only.
type View int

const (
	ViewOverview View = iota
	ViewByTool
	ViewByModel
	ViewBrowse // Sessions / Browse — the real drill list
	viewCount  // sentinel: number of routed views (keep last)
)

// viewMeta describes each view for the top tab strip: a glyph, a label and the
// hotkey digit.
type viewMeta struct {
	v     View
	glyph string
	label string
	key   string
}

// Tab glyphs are all East-Asian-Width "Neutral" (single-cell on every terminal);
// the previous By-Tool "◆" was "Ambiguous" and rendered two cells on some
// terminals, knocking the active-tab pill and click zones out of alignment.
var viewList = []viewMeta{
	{ViewOverview, "◧", "Overview", "1"},
	{ViewByTool, "❖", "By Tool", "2"},
	{ViewByModel, "⬡", "By Model", "3"},
	{ViewBrowse, "≣", "Sessions", "4"},
}

// nextView / prevView cycle the active tab (Tab / Shift+Tab), wrapping around.
func nextView(v View) View { return View((int(v) + 1) % int(viewCount)) }
func prevView(v View) View { return View((int(v) - 1 + int(viewCount)) % int(viewCount)) }

// Key is the stable string used to persist the active tab across launches.
func (v View) Key() string {
	for _, m := range viewList {
		if m.v == v {
			return m.key
		}
	}
	return ""
}

// viewFromKey parses a persisted tab key, reporting ok=false for an unknown
// value so the caller can fall back to Overview.
func viewFromKey(k string) (View, bool) {
	for _, m := range viewList {
		if m.key == k {
			return m.v, true
		}
	}
	return ViewOverview, false
}

// interactivePane returns the single interactive pane index for a view — the
// only pane that wears the focus ring and that arrows / Enter / wheel act on.
// Every other pane is read-only and never focusable, so there is no within-view
// focus cycle (which previously let the ring land on dead panels).
func interactivePane(v View) int {
	switch v {
	case ViewByTool, ViewByModel:
		return views.PaneByXBars
	case ViewBrowse:
		return views.PaneBrowseTable
	default: // ViewOverview — the trend
		return views.PaneOverviewHero
	}
}

// compile-time check that the view-local pane constants referenced across the
// package still exist.
var _ = [...]int{
	views.PaneOverviewKPIs, views.PaneOverviewHero, views.PaneOverviewTools,
	views.PaneByXBars, views.PaneByXDetail,
	views.PaneBrowseTable, views.PaneBrowsePreview,
}
