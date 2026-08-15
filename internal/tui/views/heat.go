package views

import (
	"math"
	"strings"

	"charm.land/lipgloss/v2"
)

// heat.go owns the heat-map VOCABULARY the Overview screen speaks: the six-rung
// intensity ramp candidate D established (ticket #65), the track mark that keeps
// an idle sample distinct from a missing one, and the one-row strip the KPI
// tiles and the degraded trend bodies are drawn with.
//
// One ramp, one honesty ladder. The hero's lanes paint these same rungs onto an
// ntcharts canvas (trendrender.go, drawHeatLanes) while everything else paints
// them into a string; a second, slightly different ramp next to the first would
// make identical ink mean two things on one screen, which is the whole reason
// this file exists rather than a helper per caller.

// heatRamp is the intensity ladder: six rungs built from four BMP shade and
// block glyphs plus two SGR attributes. It is the ONLY channel - a lane carries
// its value in ink alone, never in a height - so it has to carry the magnitude
// on its own. Magnitude survives in the glyph without any color at all -
// termgraph's calendar model - so a 16-color terminal, NO_COLOR and grayscale
// all keep light, medium, dark, full.
var heatRamp = [...]struct {
	glyph rune
	faint bool
	bold  bool
}{
	{'░', true, false},
	{'░', false, false},
	{'▒', false, false},
	{'▓', false, false},
	{'█', false, false},
	{'█', false, true},
}

// heatTrack marks a cell whose sample exists and is zero. It is deliberately NOT
// the bottom rung of the ramp: an idle sample and a missing one are different
// facts, so the track shows where the series runs and a gap leaves it blank. It
// sits below the first rung in density, which keeps the whole ladder monotone:
// hole, track, then the six rungs.
const heatTrack = '·'

// heatRungFor maps a fraction of the strip's peak onto a rung. Any non-zero
// value reaches at least the first rung - the smallest-honest-mark rule - and a
// value at the peak reaches the last, which is what makes scaling legible rather
// than merely true.
func heatRungFor(f float64) int {
	if f <= 0 {
		return -1
	}
	r := int(math.Ceil(f * float64(len(heatRamp))))
	if r < 1 {
		r = 1
	}
	if r > len(heatRamp) {
		r = len(heatRamp)
	}
	return r - 1
}

// heatInk is the ramp style for a fraction of the peak: the shade or block glyph
// plus whichever SGR attributes that rung carries. Any non-zero fraction reaches
// the first rung, so an active sample is never painted as an idle one - the
// smallest honest mark, now that there is no height left to quantize away.
func heatInk(base lipgloss.Style, frac float64) (rune, lipgloss.Style) {
	rung := heatRamp[heatRungFor(frac)]
	s := base
	if rung.faint {
		s = s.Faint(true)
	}
	if rung.bold {
		s = s.Bold(true)
	}
	return rung.glyph, s
}

// heatPeak is the largest value in vals: the scale a SELF-scaled strip reads
// against. A series whose own maximum is the top rung is the right reading for a
// KPI tile or a trend lane, where the question is shape over time; a series with
// an absolute ceiling would pass that ceiling instead, since self-scaling would
// paint its quietest window as hot.
func heatPeak(vals []float64) float64 {
	peak := 0.0
	for _, v := range vals {
		if v > peak {
			peak = v
		}
	}
	return peak
}

// heatConstInk is the ink of a series whose COLOR says nothing about the value:
// every rung is drawn in the series' own color and intensity alone carries the
// magnitude. A series whose color is a second reading of the same number passes
// its own function instead.
func heatConstInk(s lipgloss.Style) func(float64) lipgloss.Style {
	return func(float64) lipgloss.Style { return s }
}

// heatStrip renders vals as ONE row of heat cells, exactly w cells wide, newest
// at the RIGHT.
//
// peak is the value that reaches the top rung: heatPeak(vals) for a self-scaled
// series, 1 for an absolute 0..1 scale. ink picks the base style for a sample's
// RAW value, so color may carry a second reading while the ramp carries the
// magnitude; the glyph alone still says it under NO_COLOR.
//
// The honesty ladder is candidate D's, unchanged. A sample that does not exist
// is a HOLE - painted background, no mark - and holes fall on the OLD side,
// because a strip wider than its history is missing the past, not the present. A
// sample that exists and is zero gets the track mark. Everything else gets a
// rung.
func heatStrip(c Ctx, vals []float64, w int, peak float64, ink func(v float64) lipgloss.Style) string {
	if w < 1 {
		return ""
	}
	// More samples than cells: show the newest w of them rather than compressing
	// the series into the row, which would put two facts in one cell.
	if len(vals) > w {
		vals = vals[len(vals)-w:]
	}
	var b strings.Builder
	if hole := w - len(vals); hole > 0 {
		b.WriteString(c.pad(hole))
	}
	for _, v := range vals {
		if v <= 0 || peak <= 0 {
			b.WriteString(c.Faint.Render(string(heatTrack)))
			continue
		}
		glyph, style := heatInk(ink(v), v/peak)
		b.WriteString(style.Render(string(glyph)))
	}
	return b.String()
}
