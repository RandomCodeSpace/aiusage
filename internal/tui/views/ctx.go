// Package views holds the routed surfaces of the aiusage TUI: Overview,
// Timeline, By-Tool, By-Model, Sessions/Browse and Detail. Each view renders
// against summaries/events supplied by the root model and is responsive to
// terminal width.
//
// To avoid an import cycle with package tui (which imports views to render
// them), views depends only on lipgloss + ntcharts + bubblezone + the
// domain/store packages — never on package tui. The root model injects all
// styling, formatting and the shared zone manager through a Ctx value built in
// package tui (buildCtx).
package views

import (
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// Ctx carries the styling and formatting the views need, injected by the root
// model so views stay free of any dependency on package tui. Style fields are
// pre-built lipgloss styles; the func fields humanise/align numbers and render
// the per-component (input/output/cache) token model; ToolAccent/ToolGlyph
// yield per-tool channels.
type Ctx struct {
	// Styles.
	Panel      lipgloss.Style
	Focused    lipgloss.Style // active-pane style (cyan ring + elevated fill)
	PanelTitle lipgloss.Style
	Stat       lipgloss.Style
	StatLabel  lipgloss.Style
	Subtle     lipgloss.Style
	Number     lipgloss.Style
	Faint      lipgloss.Style // gridlines / ghosted series / disabled

	// Adaptive colors for chart adapters and segment coloring.
	NowColor    lipgloss.AdaptiveColor
	AccentColor lipgloss.AdaptiveColor
	FaintColor  lipgloss.AdaptiveColor
	BorderColor lipgloss.AdaptiveColor
	GoodColor   lipgloss.AdaptiveColor // healthy/low utilisation (resource gauges)
	WarnColor   lipgloss.AdaptiveColor // high/critical utilisation (resource gauges)

	// Comp is the ordered (input, output, cache-read, cache-creation) descriptor
	// every view iterates so KPI tiles, table columns, the trend chart and
	// legends stay in lockstep. Built from the theme palette in buildCtx.
	Comp []CompSpec

	// Formatting helpers.
	Humanize   func(int64) string
	PadLeft    func(string, int) string
	PadRight   func(string, int) string
	Truncate   func(string, int) string
	Percent    func(value, total int64) string
	Delta      func(cur, prev int64) (text string, dir int)
	ToolAccent func(tool string) lipgloss.AdaptiveColor
	ToolGlyph  func(tool string) string

	// Shared bubblezone manager for mouse hit-testing. Views Mark zones; the
	// root View() Scans the whole frame once.
	Zone *zone.Manager
}

// now returns the warm amber readout style.
func (c Ctx) now() lipgloss.Style { return lipgloss.NewStyle().Foreground(c.NowColor) }

// tool returns a bold per-tool accent style.
func (c Ctx) tool(name string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(c.ToolAccent(name)).Bold(true)
}

// mark wraps s in a zone if a manager is present; otherwise returns s as-is so
// headless rendering (tests) still works.
func (c Ctx) mark(id, s string) string {
	if c.Zone == nil {
		return s
	}
	return c.Zone.Mark(id, s)
}

// panelStyle returns the focused or idle panel style based on the flag.
func (c Ctx) panelStyle(focused bool) lipgloss.Style {
	if focused {
		return c.Focused
	}
	return c.Panel
}

// titleChip renders a panel title, appending a non-color focus marker (a cyan
// chevron) when the pane is focused. Color is never the only channel: the
// chevron glyph itself signals focus even in monochrome terminals, complementing
// the cyan ring + elevated fill.
func (c Ctx) titleChip(label string, focused bool) string {
	if focused {
		// Focused: the whole title goes bright cyan + bold and gains a chevron,
		// reinforcing the thick border ring on the active pane.
		return lipgloss.NewStyle().Foreground(c.AccentColor).Bold(true).Render(label) +
			" " + lipgloss.NewStyle().Foreground(c.AccentColor).Render("◂")
	}
	return c.PanelTitle.Render(label)
}

// SeriesFor extracts one metric across buckets (in order) as a []float64 for
// feeding sparklines/charts. selector picks the metric out of each bucket.
func SeriesFor(buckets []store.Bucket, selector func(store.Bucket) int64) []float64 {
	out := make([]float64, len(buckets))
	for i, b := range buckets {
		out[i] = float64(selector(b))
	}
	return out
}
