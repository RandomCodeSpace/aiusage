package views

import (
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// sysgauge.go renders the compact container-resource strip (CPU / memory / disk)
// shown at the top of the Overview tab. Each gauge is a rolling HEAT STRIP — one
// cell per sample, oldest left, newest right, btop's arrangement — beside the
// live percentage, so the user reads both the current pressure and how it got
// there. Values come from the root model's sysmon snapshot and its sample
// history; this file only formats them.
//
// The strip speaks the same heat vocabulary as the hero's lanes (heat.go), with
// one deliberate difference: utilisation has an absolute ceiling, so these cells
// are read against 1 and never self-scaled. A quiet machine must look quiet —
// self-scaling would paint an idle CPU's 3% peak as a full black cell.
// Color remains the second reading of the same number (green healthy, amber
// busy, red critical); the ramp alone still carries it under NO_COLOR.

// SysGauge is one resource reading for the strip: a 0..1 fill, a short readout
// (e.g. "1.2G/4.0G"), the rolling sample history behind it, and Known=false when
// the value is not yet available (CPU before its second sample), which renders a
// muted placeholder.
type SysGauge struct {
	Label string
	Frac  float64
	Text  string
	Known bool
	// History is the utilisation window, OLDEST first, one entry per sample
	// tick; the newest entry is the reading Frac states. Fewer entries than the
	// strip has cells is the normal state of a young session and renders as
	// holes on the old side — the view never invents a sample it was not given.
	History []float64
}

// utilisation thresholds for the gauge color (fraction of the container limit).
const (
	gaugeWarnFrac = 0.90 // red at/above this
	gaugeBusyFrac = 0.70 // amber at/above this
)

// SysStrip renders the resource gauges as one row sized to width. Returns "" when
// there is nothing to show or the row cannot fit every gauge at a readable
// minimum — degrading to no strip rather than overflowing the layout.
func SysStrip(c Ctx, gauges []SysGauge, width int) string {
	const (
		gutter  = 2
		minCell = 12 // smallest readable gauge cell ("cpu ▕▒█▏ 38%")
	)
	n := len(gauges)
	if n == 0 {
		return ""
	}
	// Bail when even the minimum-size cells plus gutters would not fit. This
	// guarantees the floor below never pushes the row past width (the previous
	// code floored cellW to minCell unconditionally, overflowing on narrow rows).
	if width < minCell*n+gutter*(n-1) {
		return ""
	}
	cellW := (width - gutter*(n-1)) / n // >= minCell given the guard above
	segs := make([]string, 0, n)
	for _, g := range gauges {
		segs = append(segs, sysGaugeCell(c, g, cellW))
	}
	return strings.Join(segs, strings.Repeat(" ", gutter))
}

// gaugeColor maps a utilisation fraction to the healthy/busy/critical color.
func gaugeColor(c Ctx, frac float64) color.Color {
	switch {
	case frac >= gaugeWarnFrac:
		return c.WarnColor
	case frac >= gaugeBusyFrac:
		return c.NowColor
	default:
		return c.GoodColor
	}
}

// sysGaugeCell renders one gauge to exactly cellW cells: "label ▕░▒▓██▏ 42%",
// appending the readout text when there is spare room. The brackets mark the
// strip's extent, which a history that has not filled it yet needs — an empty
// cell inside them is a sample not taken, not a quiet one. Unknown gauges show
// an empty strip and a "…" placeholder.
func sysGaugeCell(c Ctx, g SysGauge, cellW int) string {
	label := padRightLocal(g.Label, 4)

	// Fixed cost: label(4) + space + brackets(2) + space + pct(4).
	const fixed = 4 + 1 + 2 + 1 + 4
	// Reserve readout space only when the cell is comfortably wide.
	readout := ""
	if g.Known && g.Text != "" && cellW >= fixed+len(g.Text)+2 {
		readout = " " + g.Text
	}
	stripW := cellW - fixed - lipgloss.Width(readout)
	if stripW < 3 {
		stripW = 3
	}

	var strip, pct string
	if !g.Known {
		// Nothing has been read yet, so nothing is painted: an unreadable gauge is
		// a hole, exactly as a bucket with no data is a hole in the hero's lanes.
		// A faint track across the strip would claim samples were taken and idle.
		strip = c.pad(stripW)
		pct = c.Faint.Render("  …")
	} else {
		// Absolute scale: the fraction maps straight onto the ramp, so 90% looks
		// hot whatever its neighbours did. The per-sample color keeps the strip's
		// history honest about when the machine was in trouble, rather than
		// painting the whole window at the current reading's severity.
		strip = heatStrip(c, g.History, stripW, 1, func(v float64) lipgloss.Style {
			return c.fg(gaugeColor(c, v))
		})
		pct = c.fg(gaugeColor(c, g.Frac)).Render(padLeftLocal(strconv.Itoa(int(g.Frac*100+0.5))+"%", 4))
	}

	cell := c.StatLabel.Render(label) + c.pad(1) +
		c.Faint.Render("▕") + strip + c.Faint.Render("▏") + c.pad(1) + pct +
		c.Subtle.Render(readout)

	// Pad/clamp to exactly cellW so the row never drifts or wraps.
	if w := lipgloss.Width(cell); w < cellW {
		cell += c.pad(cellW - w)
	}
	return lipgloss.NewStyle().MaxWidth(cellW).Render(cell)
}

// padLeftLocal is a tiny ASCII left-pad for the gauge percentage column, kept
// local so this file does not depend on the Ctx number-formatting funcs.
// (padRightLocal is shared from chartstyle.go.)
func padLeftLocal(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}
