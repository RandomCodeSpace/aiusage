package views

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
)

// zeroAdaptive is the unset compat.AdaptiveColor. Rendering with one panics
// inside lipgloss v2, so every optional color is compared against it first.
var zeroAdaptive compat.AdaptiveColor

// surface.go is the visual design language (issue #20, resolved on #22). It
// holds the five primitives every surface composes from — the elevation ladder,
// the chip vocabulary, titled rules, the width-invariant focus bar and the
// eighth-block glyph ramp — so a view never invents its own treatment.
//
// The rules, in one place:
//
//  1. TITLED RULES, not box captions. A pane announces itself with a focus-bar
//     slot, its name, and a hairline that runs to the pane edge.
//  2. A 4-STEP ELEVATION LADDER carries depth (see Elevation below).
//  3. PAINTED BLOCKS, not bordered boxes. Panels are padded, painted regions.
//  4. WIDTH-INVARIANT FOCUS BARS. Focus costs the same two cells at every size.
//  5. A CHIP VOCABULARY for tabs, ranges, states and actions.
//
// Borders retreat to the outer app frame only.
//
// Monochrome is a hard invariant (mono_test.go): paint is invisible with SGR
// stripped, so every one of these primitives carries its meaning in the glyph
// channel first — the focus bar, the chip markers, the rule, the glyph+word
// pairs — and uses color only to reinforce it.

// Elevation is a step on the 4-step background ladder. Higher steps read as
// closer to the reader.
type Elevation int

const (
	// ElevGround (L0) is the app ground: the outer frame's interior, the gutters
	// between cards, and every chart canvas. Charts stay on the ground plane on
	// purpose — ntcharts renders its canvas one cell at a time with per-cell
	// styles, so a panel background cannot reach the cells between the braille
	// without a per-cell repaint pass on every scrub.
	ElevGround Elevation = iota
	// ElevCard (L1) is the resting floor of every reading surface: KPI tiles,
	// entity rows, the browse table, side panels.
	ElevCard
	// ElevRaised (L2) is the focused pane's floor and the selected row block.
	ElevRaised
	// ElevChip (L3) is the top step: chips, the active tab, table header bands.
	ElevChip
)

// elevCount is the number of steps on the ladder.
const elevCount = 4

// FocusBar is the width-invariant focus marker. It is exactly one cell wide at
// every pane size and always renders with a trailing space, so a focused and an
// unfocused title occupy identical columns and nothing reflows when focus moves.
const FocusBar = "▌"

// focusSlot is the blank stand-in for FocusBar on an unfocused surface.
const focusSlot = " "

// Chevron is the drill affordance: the glyph a row wears when pressing it
// descends a level (issue #24). It is the same chevron the breadcrumb separates
// with and the range chip brackets with, so "deeper" reads identically wherever
// it appears. It is a glyph and not an underline on purpose — in a terminal an
// underline reads as "URL", never as "button".
const Chevron = "›"

// ruleGlyph is the hairline a titled rule draws to the pane edge.
const ruleGlyph = "─"

// eighthRamp is the left-to-right partial-cell ramp, U+258F..U+2588 (1/8 .. 8/8
// of a cell). It is what de-pixelates every bar: a bar whose exact length is
// 12.4 cells renders twelve full cells plus a two-eighths cap instead of being
// rounded to a whole cell. Index 0 is "no cell at all".
var eighthRamp = [9]string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"}

// eighthCells splits an exact (fractional) bar length into whole cells plus the
// partial cap glyph for the remainder. cap is "" when the remainder is under
// one eighth — the honest rendering of "nothing more to draw".
func eighthCells(exact float64, width int) (full int, cap string) {
	if exact <= 0 || width <= 0 {
		return 0, ""
	}
	if exact > float64(width) {
		exact = float64(width)
	}
	full = int(exact)
	if full >= width {
		return width, ""
	}
	eighths := int((exact-float64(full))*8 + 0.5)
	if eighths <= 0 {
		return full, ""
	}
	if eighths >= 8 {
		return full + 1, ""
	}
	return full, eighthRamp[eighths]
}

// On returns a copy of the context whose reusable styles are painted at the
// given elevation. A panel calls it once and everything it renders below sits
// on the same step, which is what keeps a painted block from tearing into
// stripes at the first nested SGR reset.
func (c Ctx) On(e Elevation) Ctx {
	bg := c.ElevColor(e)
	if bg == nil {
		return c
	}
	c.BG = bg
	c.PanelTitle = c.PanelTitle.Background(bg)
	c.Stat = c.Stat.Background(bg)
	c.StatLabel = c.StatLabel.Background(bg)
	c.Subtle = c.Subtle.Background(bg)
	c.Number = c.Number.Background(bg)
	c.Faint = c.Faint.Background(bg)
	return c
}

// ElevColor returns the ladder color for a step, or nil when the caller built a
// partial context (headless view tests) and nothing should be painted.
func (c Ctx) ElevColor(e Elevation) color.Color {
	if e < 0 || int(e) >= elevCount {
		return nil
	}
	return c.Elev[e]
}

// Fill returns a style painted at the given elevation.
func (c Ctx) Fill(e Elevation) lipgloss.Style {
	if bg := c.ElevColor(e); bg != nil {
		return lipgloss.NewStyle().Background(bg)
	}
	return lipgloss.NewStyle()
}

// fg returns a foreground style that keeps the context's current elevation, so
// an ad-hoc colored run inside a painted block does not punch a hole in it.
func (c Ctx) fg(col color.Color) lipgloss.Style {
	s := lipgloss.NewStyle().Foreground(col)
	if c.BG != nil {
		s = s.Background(c.BG)
	}
	return s
}

// pad renders n painted spaces. Separators between styled runs have to be
// painted too, or the block tears at every gap.
func (c Ctx) pad(n int) string {
	if n <= 0 {
		return ""
	}
	if c.BG == nil {
		return strings.Repeat(" ", n)
	}
	return lipgloss.NewStyle().Background(c.BG).Render(strings.Repeat(" ", n))
}

// fitParts joins already-styled parts with sep, dropping them from the right
// until the line fits w display columns. Styled strings must never go through
// Ctx.Truncate: that helper counts runes, and every SGR byte counts as one, so
// a painted separator alone can eat most of the budget.
func (c Ctx) fitParts(parts []string, sep string, w int) string {
	line := strings.Join(parts, sep)
	for lipgloss.Width(line) > w && len(parts) > 1 {
		parts = parts[:len(parts)-1]
		line = strings.Join(parts, sep)
	}
	return line
}

// FocusMark renders the focus slot: the bar when focused, a blank cell when
// not. Always one cell — the caller adds the separating space.
func (c Ctx) FocusMark(focused bool) string {
	if focused {
		return c.fg(c.AccentColor).Render(FocusBar)
	}
	return c.pad(1)
}

// DrillMark renders the drill slot: the chevron on a row a press descends, a
// blank cell on one that does not. Like FocusMark it is exactly one cell at
// every width, so a list never reflows between drill levels — the deepest level
// keeps the columns the shallower ones had, it just stops drawing chevrons.
//
// The chevron is deliberately quiet (Subtle, not accent): the accent channel
// belongs to focus and selection, and an affordance that shouts on every row
// would drown them.
func (c Ctx) DrillMark(drillable bool) string {
	if drillable {
		return c.Subtle.Render(Chevron)
	}
	return c.pad(1)
}

// Rule renders a titled rule: an already-composed head, then a hairline out to
// w. It replaces the bordered box caption — the rule is what tells the eye how
// far the pane reaches, and it survives monochrome where paint does not. The
// head is composed by the caller (titleChip carries the focus slot).
func (c Ctx) Rule(head string, w int) string { return c.RuleBetween(head, "", w) }

// RuleBetween renders a titled rule with a right-hand readout: head, a hairline,
// then tail flush against w. The hero's pane headers use it to hang their SCALE
// readout off the end of the rule.
func (c Ctx) RuleBetween(head, tail string, w int) string {
	used := lipgloss.Width(head) + lipgloss.Width(tail)
	if tail != "" {
		used++ // the space before the tail
	}
	fill := w - used - 1 // the space after the head
	if fill < 1 {
		if tail == "" {
			return head
		}
		return head + c.pad(1) + tail
	}
	rule := c.Faint.Render(strings.Repeat(ruleGlyph, fill))
	if tail == "" {
		return head + c.pad(1) + rule
	}
	return head + c.pad(1) + rule + c.pad(1) + tail
}

// ChipTone selects a chip's step on the elevation ladder.
type ChipTone int

const (
	// ChipCard is a resting chip (an inactive tab).
	ChipCard ChipTone = iota
	// ChipTop is the default chip: range, state and action chips.
	ChipTop
	// ChipAccent is the one selected chip on a strip (the active tab). It
	// inverts: accent ground, app ground ink.
	ChipAccent
)

// Chip renders one chip of the vocabulary: an optional width-invariant marker
// slot, then the body, inside one cell of padding on each side, painted at the
// tone's elevation.
//
// The marker slot is the monochrome channel for selection: an active chip wears
// FocusBar, an inactive one a blank cell of the same width, so a strip of chips
// never reflows when the selection moves. Pass marked=false and slot=false for
// chips that are never selected (state, range, action).
func (c Ctx) Chip(tone ChipTone, fg color.Color, slot, marked bool, body string) string {
	st := c.chipStyle(tone, fg)
	lead := ""
	if slot {
		lead = focusSlot + " "
		if marked {
			lead = FocusBar + " "
		}
	}
	return st.Render(" " + lead + body + " ")
}

// chipStyle resolves a tone to its painted style.
func (c Ctx) chipStyle(tone ChipTone, fg color.Color) lipgloss.Style {
	switch tone {
	case ChipAccent:
		// The one selected chip inverts. A zero-value compat.AdaptiveColor panics
		// inside lipgloss v2 Render, so a partial (headless) context falls back to
		// bold-only — the marker slot still carries the selection.
		st := lipgloss.NewStyle().Bold(true)
		if c.AccentColor != (zeroAdaptive) {
			st = st.Background(c.AccentColor)
			if g := c.ElevColor(ElevGround); g != nil {
				st = st.Foreground(g)
			}
		}
		return st
	case ChipCard:
		return c.applyFg(c.Fill(ElevCard), fg)
	default:
		return c.applyFg(c.Fill(ElevChip), fg)
	}
}

// applyFg sets a foreground on a style when one was supplied.
func (c Ctx) applyFg(s lipgloss.Style, fg color.Color) lipgloss.Style {
	if fg != nil {
		return s.Foreground(fg)
	}
	return s
}

// Block returns the painted panel style for a surface at the given elevation:
// uniform padding where a border used to be, so the frame geometry is unchanged
// (lipgloss v2 sizes are border- AND padding-inclusive) while the box itself is
// gone. blockPadY/blockPadX are the design's uniform card padding.
func (c Ctx) Block(e Elevation) lipgloss.Style {
	return c.Fill(e).Padding(blockPadY, blockPadX)
}

// Uniform card padding. A card costs the same 4 columns and 2 rows the rounded
// border used to, so every width/height budget in the views is untouched.
const (
	blockPadY = 1
	blockPadX = 2
)
