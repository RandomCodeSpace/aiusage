package views

import "strconv"

// Zone IDs are stable string constants used by both the renderers (which Mark
// regions) and the root mouse dispatch (which resolves a click to an action).
// Keeping them in package views avoids a views→tui import while letting the
// root model in tui reference the same constants via the exported names below.
const (
	zoneHero      = "hero"      // hero / timeline chart body
	zoneBars      = "bars"      // by-tool / by-model bars pane
	zoneTable     = "table"     // browse table pane
	zonePreview   = "preview"   // browse preview / by-entity detail pane
	zoneRangePill = "rangepill" // header range pill
	zoneHelp      = "help"      // header help toggle
	zoneFreshness = "freshness" // header freshness chip (click = force refresh)
	zoneSort      = "sort"      // breadcrumb-bar sort chip (click = cycle sort)
	zoneFilter    = "filter"    // footer filter chip (click = focus the input)
)

// Exported zone-ID builders/constants so package tui can resolve clicks without
// duplicating the string literals.

// ZoneHero is the hero / timeline chart body. It is the wheel's scrub target:
// a notch over the chart steps the crosshair.
const ZoneHero = zoneHero

// ZoneBars is the by-tool/by-model bars pane.
const ZoneBars = zoneBars

// ZoneTable is the browse table pane.
const ZoneTable = zoneTable

// ZonePreview is the browse preview pane.
const ZonePreview = zonePreview

// ZoneRangePill is the header range pill.
const ZoneRangePill = zoneRangePill

// ZoneHelp is the header help toggle.
const ZoneHelp = zoneHelp

// ZoneFreshness is the header freshness chip. The indicator is where you act:
// a left-press on it forces a refresh.
const ZoneFreshness = zoneFreshness

// ZoneSort is the breadcrumb bar's sort chip: it renders as an action chip, so
// pressing it acts — it cycles the sort order, exactly as the `s` key does.
const ZoneSort = zoneSort

// ZoneFilter is the footer's active-filter chip: pressing it reopens the filter
// input with the current term, exactly as `/` does.
const ZoneFilter = zoneFilter

// RailZone returns the click-zone id for a nav-rail entry (a view index).
func RailZone(viewIdx int) string { return "rail:" + strconv.Itoa(viewIdx) }

// BarZone returns the click-zone id for a single bar (tool or model name).
func BarZone(name string) string { return "bar:" + name }

// RowZone returns the click-zone id for a browse table row by index.
func RowZone(idx int) string { return "row:" + strconv.Itoa(idx) }

// ActZone returns the click-zone id for an Activity rank row by index. Activity
// rows are keyed by index rather than by name (the way BarZone keys tool and
// model bars) because a name is not unique on that tab: the same tool name is
// invoked by more than one agent CLI, and two zones sharing an id would resolve
// a press to whichever the manager happened to record last.
func ActZone(idx int) string { return "act:" + strconv.Itoa(idx) }

// CrumbZone returns the click-zone id for a breadcrumb at a drill depth.
func CrumbZone(depth int) string { return "crumb:" + strconv.Itoa(depth) }
