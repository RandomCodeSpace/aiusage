package views

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// sysgauge.go renders the compact container-resource strip (CPU / memory / disk)
// shown at the top of the Overview tab. Each gauge is a labelled horizontal bar
// colored by utilisation — green when healthy, amber when busy, red when near
// the container's limit — so the user can read pressure at a glance without a
// detailed view. Values come from the root model's sysmon snapshot; this file
// only formats them.

// SysGauge is one resource reading for the strip: a 0..1 fill, a short readout
// (e.g. "1.2G/4.0G"), and Known=false when the value is not yet available (CPU
// before its second sample), which renders a muted placeholder.
type SysGauge struct {
	Label string
	Frac  float64
	Text  string
	Known bool
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
		minCell = 12 // smallest readable gauge cell ("cpu ▕██▏ 38%")
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
func gaugeColor(c Ctx, frac float64) lipgloss.TerminalColor {
	switch {
	case frac >= gaugeWarnFrac:
		return c.WarnColor
	case frac >= gaugeBusyFrac:
		return c.NowColor
	default:
		return c.GoodColor
	}
}

// sysGaugeCell renders one gauge to exactly cellW cells: "label ▕███░░▏ 42%",
// appending the readout text when there is spare room. Unknown gauges show a
// faint empty bar and a "…" placeholder.
func sysGaugeCell(c Ctx, g SysGauge, cellW int) string {
	label := padRightLocal(g.Label, 4)

	// Fixed cost: label(4) + space + brackets(2) + space + pct(4).
	const fixed = 4 + 1 + 2 + 1 + 4
	// Reserve readout space only when the cell is comfortably wide.
	readout := ""
	if g.Known && g.Text != "" && cellW >= fixed+len(g.Text)+2 {
		readout = " " + g.Text
	}
	barInner := cellW - fixed - lipgloss.Width(readout)
	if barInner < 3 {
		barInner = 3
	}

	var bar, pct string
	if !g.Known {
		bar = c.Faint.Render(strings.Repeat("░", barInner))
		pct = c.Faint.Render("  …")
	} else {
		filled := int(g.Frac*float64(barInner) + 0.5)
		if filled > barInner {
			filled = barInner
		}
		col := gaugeColor(c, g.Frac)
		bar = lipgloss.NewStyle().Foreground(col).Render(strings.Repeat("█", filled)) +
			c.Faint.Render(strings.Repeat("░", barInner-filled))
		pct = lipgloss.NewStyle().Foreground(col).Render(padLeftLocal(strconv.Itoa(int(g.Frac*100+0.5))+"%", 4))
	}

	cell := c.StatLabel.Render(label) + " " +
		c.Faint.Render("▕") + bar + c.Faint.Render("▏") + " " + pct +
		c.Subtle.Render(readout)

	// Pad/clamp to exactly cellW so the row never drifts or wraps.
	if w := lipgloss.Width(cell); w < cellW {
		cell += strings.Repeat(" ", cellW-w)
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
