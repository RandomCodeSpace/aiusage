package views

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"aiusage/internal/store"
)

// components.go is the single source of truth for the four DB-backed token
// series the dashboard renders — input, output, cache-read, cache-creation — and
// the ordered descriptor (label/short/color/glyph/selector) that KPI tiles,
// table columns, the trend chart and legends all iterate so they never drift
// out of sync. Replaces the old "fresh vs cache" two-axis model.

// Components is one bucket split into the four display series. Reasoning is NOT a
// separate series: the schema records it as a subset of Output, so adding it
// would double-count; Output is shown as the raw column. The provider total
// (store.Bucket.Total) is shown separately as a bare number, never derived here.
type Components struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
}

// Split breaks a bucket into the four display components (raw DB columns).
func Split(b store.Bucket) Components {
	return Components{
		Input:         b.Input,
		Output:        b.Output,
		CacheRead:     b.CacheRead,
		CacheCreation: b.CacheCreation,
	}
}

// Sum is input+output+cache-read+cache-creation — distinct from the provider
// total store.Bucket.Total.
func (c Components) Sum() int64 { return c.Input + c.Output + c.CacheRead + c.CacheCreation }

// CompSpec describes one token series for uniform rendering across every view:
// its key, display labels, accent color, monochrome-safe glyph, and a selector
// that pulls its value out of a Components. Color is a TerminalColor so callers
// can pass ANSI palette indices (which adapt to the user's terminal theme).
type CompSpec struct {
	Key   string
	Label string // legend / KPI title, e.g. "cache"
	Short string // narrow table header, e.g. "cache"
	Glyph string
	Color lipgloss.TerminalColor
	Pick  func(Components) int64
}

// Style returns the colored style for this series.
func (s CompSpec) Style() lipgloss.Style { return lipgloss.NewStyle().Foreground(s.Color) }

// CompSpecs builds the ordered (input, output, cache) descriptor. cache combines
// the two DB cache sub-types (read + creation): that split is only meaningful for
// Claude and is kept in the DB purely so internal cost pricing can value them
// differently later — the UI always shows a single combined cache series. Order
// is load-bearing: charts, tiles and columns all render in this order.
func CompSpecs(input, output, cache lipgloss.TerminalColor) []CompSpec {
	return []CompSpec{
		{"input", "input", "input", "▰", input, func(c Components) int64 { return c.Input }},
		{"output", "output", "output", "▱", output, func(c Components) int64 { return c.Output }},
		{"cache", "cache", "cache", "◆", cache, func(c Components) int64 { return c.CacheRead + c.CacheCreation }},
	}
}

// CompBar renders a horizontal bar split into the four component segments,
// proportional to each value against max and colored per series. Any non-zero
// series gets at least one cell so it stays visible even when another dominates;
// the remainder is a faint track. Segment length encodes the bucket's total
// relative to max, segment colors encode its composition.
func (c Ctx) CompBar(comps Components, max int64, width int) string {
	if width <= 0 {
		return ""
	}
	if max <= 0 {
		return c.Faint.Render(strings.Repeat("░", width))
	}
	var b strings.Builder
	used := 0
	for _, s := range c.Comp {
		v := s.Pick(comps)
		if v <= 0 {
			continue
		}
		cells := int(float64(v) / float64(max) * float64(width))
		if cells == 0 {
			cells = 1
		}
		if used+cells > width {
			cells = width - used
		}
		if cells <= 0 {
			break
		}
		b.WriteString(s.Style().Render(strings.Repeat("█", cells)))
		used += cells
	}
	if used < width {
		b.WriteString(c.Faint.Render(strings.Repeat("░", width-used)))
	}
	return b.String()
}

// CompLegend renders a one-line legend of the four series (glyph + label) in
// their colors, joined by the given separator-styled middot. Used in chart/panel
// titles so the trend's colors are always labeled.
func (c Ctx) CompLegend() string {
	parts := make([]string, 0, len(c.Comp))
	for _, s := range c.Comp {
		parts = append(parts, s.Style().Render(s.Glyph+" "+s.Short))
	}
	return strings.Join(parts, " ")
}
