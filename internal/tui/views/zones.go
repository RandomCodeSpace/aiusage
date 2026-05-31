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
	zonePreview   = "preview"   // browse preview pane
	zoneRangePill = "rangepill" // header range pill
	zoneHelp      = "help"      // header help toggle
)

// Exported zone-ID builders/constants so package tui can resolve clicks without
// duplicating the string literals.

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

// RailZone returns the click-zone id for a nav-rail entry (a view index).
func RailZone(viewIdx int) string { return "rail:" + strconv.Itoa(viewIdx) }

// BarZone returns the click-zone id for a single bar (tool or model name).
func BarZone(name string) string { return "bar:" + name }

// RowZone returns the click-zone id for a browse table row by index.
func RowZone(idx int) string { return "row:" + strconv.Itoa(idx) }

// CrumbZone returns the click-zone id for a breadcrumb at a drill depth.
func CrumbZone(depth int) string { return "crumb:" + strconv.Itoa(depth) }

// KPI metric ids (also the hero pivot metrics).
const (
	KPITotal  = "total"
	KPICache  = "cache"
	KPIEvents = "events"
)
