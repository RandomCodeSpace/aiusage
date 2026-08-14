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
	"image/color"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
	zone "github.com/lrstanley/bubblezone/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// Ctx carries the styling and formatting the views need, injected by the root
// model so views stay free of any dependency on package tui. Style fields are
// pre-built lipgloss styles; the func fields humanise/align numbers and render
// the per-component (input/output/cache) token model; ToolAccent/ToolGlyph
// yield per-tool channels.
type Ctx struct {
	// Styles. Panels themselves are built from the elevation ladder
	// (Ctx.Block), not from a pre-made panel style — there is no box any more.
	PanelTitle lipgloss.Style
	Stat       lipgloss.Style
	StatLabel  lipgloss.Style
	Subtle     lipgloss.Style
	Number     lipgloss.Style
	Faint      lipgloss.Style // gridlines / ghosted series / disabled

	// Elev is the 4-step background elevation ladder (see surface.go). Views
	// never name a hex color: they name a step. Elev entries may be nil in the
	// partial contexts headless tests build, in which case nothing is painted.
	Elev [elevCount]color.Color
	// BG is the elevation the context is currently rendering on, set by On(). It
	// is what keeps ad-hoc colored runs and separators from tearing the block.
	BG color.Color

	// Adaptive colors for chart adapters and segment coloring. There is no
	// border color: the ONE border is the outer app frame, which package tui
	// draws itself (Theme.AppFrame). Nothing inside a view is boxed.
	NowColor    compat.AdaptiveColor
	AccentColor compat.AdaptiveColor
	FaintColor  compat.AdaptiveColor
	GoodColor   compat.AdaptiveColor // healthy/low utilisation (resource gauges)
	WarnColor   compat.AdaptiveColor // high/critical utilisation (resource gauges)

	// Comp is the ordered (input, output, cache-read, cache-creation) descriptor
	// every view iterates so KPI tiles, table columns, the trend chart and
	// legends stay in lockstep. Built from the theme palette in buildCtx.
	Comp []CompSpec

	// LeverageFloor is the configured per-bucket input token count below which
	// the hero's cache-leverage ratio is suppressed as noise. Zero — the
	// default, and what every partial headless Ctx carries — derives the floor
	// from the bucket span instead (defaultLeverageFloor). It is injected once
	// from config and static for the life of the process, which is why the
	// render memo does not key on it.
	LeverageFloor int64

	// Trend selects the hero's candidate LINE TREATMENT (ticket #65). Unlike
	// LeverageFloor it is NOT static - the reader flips it live with x - so the
	// render memo keys on it explicitly (HeroMemo.trend). The zero value is the
	// shipped renderer, so every partial headless Ctx keeps the old behaviour.
	Trend TrendRender

	// Formatting helpers.
	Humanize func(int64) string
	PadLeft  func(string, int) string
	PadRight func(string, int) string
	Truncate func(string, int) string
	Percent  func(value, total int64) string
	Delta    func(cur, prev int64) (text string, dir int)
	// Money renders a micro-USD amount — model.FormatCost, shared with the
	// report surfaces so the two cannot drift. approximate marks a figure the
	// ledger cannot state exactly (a range holding rows nothing priced is a
	// floor, not a bill); known is false when nothing could be priced at all.
	// Nil renders no cost tile rather than a bare number with no currency.
	Money      func(microUSD int64, approximate, known bool) string
	ToolAccent func(tool string) compat.AdaptiveColor
	ToolGlyph  func(tool string) string

	// Shared bubblezone manager for mouse hit-testing. Views Mark zones; the
	// root View() Scans the whole frame once.
	Zone *zone.Manager
}

// now returns the warm amber readout style.
func (c Ctx) now() lipgloss.Style { return c.fg(c.NowColor) }

// good returns the falling-spend / healthy style.
func (c Ctx) good() lipgloss.Style { return c.fg(c.GoodColor) }

// tool returns a bold per-tool accent style.
func (c Ctx) tool(name string) lipgloss.Style {
	return c.fg(c.ToolAccent(name)).Bold(true)
}

// mark wraps s in a zone if a manager is present; otherwise returns s as-is so
// headless rendering (tests) still works.
func (c Ctx) mark(id, s string) string {
	if c.Zone == nil {
		return s
	}
	return c.Zone.Mark(id, s)
}

// paneElev is the elevation a pane's floor sits at: raised while it holds
// focus, the resting card step otherwise.
func paneElev(focused bool) Elevation {
	if focused {
		return ElevRaised
	}
	return ElevCard
}

// titleChip renders a panel title WITHOUT its rule: the width-invariant focus
// slot plus the name. Focus is the bar, not a border and not a color — the bar
// is the one channel that survives a monochrome terminal, and it costs the same
// two cells whether the pane is focused or not, so nothing reflows.
func (c Ctx) titleChip(label string, focused bool) string {
	if focused {
		return c.FocusMark(true) + c.pad(1) + c.fg(c.AccentColor).Bold(true).Render(label)
	}
	return c.FocusMark(false) + c.pad(1) + c.PanelTitle.Render(label)
}

// titleRule is titleChip plus the hairline out to w: the titled rule that
// replaced the bordered box caption. Panes that append chips to their title
// (the hero) compose the head themselves and call Rule directly.
func (c Ctx) titleRule(label string, w int, focused bool) string {
	return c.Rule(c.titleChip(label, focused), w)
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
