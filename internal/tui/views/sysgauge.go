package views

import (
	"image/color"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// sysgauge.go renders the compact container-resource strip (CPU / memory / disk)
// shown at the top of the Overview tab. Each gauge pairs its current value with
// a horizontal utilization bar. Green, amber, and red mark healthy, busy, and
// critical readings. Values come from the root model's sysmon snapshot; this
// file only formats them.

// SysGauge is one resource reading for the strip: a 0..1 fill, a short readout
// (e.g. "1.2G/4.0G"), and Known=false when the value is not yet available (CPU
// before its second sample), which renders a muted placeholder.
type SysGauge struct {
	Label string
	Frac  float64
	Text  string
	Known bool
	// History remains part of the sampling contract. The live strip deliberately
	// renders Frac instead so its bar answers current pressure at a glance.
	History []float64
}

// utilisation thresholds for the gauge color (fraction of the container limit).
const (
	gaugeWarnFrac = 0.95 // red at/above this
	gaugeBusyFrac = 0.85 // amber at/above this
)

// SysStrip renders the resource gauges within width. Wide layouts use three
// two-line columns. Narrow layouts stack one compact line per reading, keeping
// every current value and its utilization bar instead of dropping the strip.
func SysStrip(c Ctx, gauges []SysGauge, width int) string {
	const (
		gutter      = 2
		wideCellMin = 22
		rowMin      = 8 // "cpu  38%"
	)
	n := len(gauges)
	if n == 0 || width < rowMin {
		return ""
	}
	if width < wideCellMin*n+gutter*(n-1) {
		rows := make([]string, 0, n)
		for _, gauge := range gauges {
			rows = append(rows, sysGaugeRow(c, gauge, width))
		}
		return strings.Join(rows, "\n")
	}

	segs := make([]string, 0, n)
	for i, gauge := range gauges {
		cellW := (i+1)*(width-gutter*(n-1))/n - i*(width-gutter*(n-1))/n
		segs = append(segs, sysGaugeCell(c, gauge, cellW))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, intersperseGaugeCells(segs, c.pad(gutter))...)
}

func intersperseGaugeCells(cells []string, gutter string) []string {
	if len(cells) < 2 {
		return cells
	}
	parts := make([]string, 0, len(cells)*2-1)
	for i, cell := range cells {
		if i > 0 {
			parts = append(parts, gutter)
		}
		parts = append(parts, cell)
	}
	return parts
}

// gaugeStyle keeps the label and value on the same semantic channel as the bar.
func gaugeStyle(c Ctx, frac float64) lipgloss.Style {
	return c.fg(gaugeColor(c, frac)).Bold(true)
}

// gaugeColor maps utilization to the existing healthy, live, and warning colors.
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

// sysGaugeCell renders a wide two-line cell: exact current reading first, then
// the live 0..100% bar. Unknown gauges retain the existing "…" placeholder.
func sysGaugeCell(c Ctx, g SysGauge, cellW int) string {
	reading := gaugeReading(c, g)
	readout := ""
	if g.Known && g.Text != "" {
		avail := cellW - lipgloss.Width(reading) - 1
		if avail >= 4 {
			readout = gaugeNoteStyle(c).Render(" " + ansi.Truncate(g.Text, avail-1, "…"))
		}
	}
	line := fitGaugeLine(c, reading+readout, cellW)
	return line + "\n" + machineGaugeBar(c, g, cellW)
}

// sysGaugeRow is the narrow composition. Capacity text yields before the
// current percentage, then the plot uses whatever columns remain.
func sysGaugeRow(c Ctx, g SysGauge, width int) string {
	reading := gaugeReading(c, g)
	plotW := width - lipgloss.Width(reading) - 1
	readout := ""
	if g.Known && g.Text != "" && plotW >= 10 {
		readoutW := min(12, plotW-4)
		readout = gaugeNoteStyle(c).Render(" " + ansi.Truncate(g.Text, readoutW-1, "…"))
		plotW -= lipgloss.Width(readout)
	}
	if plotW < 2 {
		return fitGaugeLine(c, reading+readout, width)
	}
	return fitGaugeLine(c, reading+c.pad(1)+machineGaugeBar(c, g, plotW)+readout, width)
}

func gaugeNoteStyle(c Ctx) lipgloss.Style {
	return c.Subtle.Italic(true)
}

func gaugeReading(c Ctx, g SysGauge) string {
	if !g.Known {
		label := c.StatLabel.Bold(true).Render(padRightLocal(g.Label, 4))
		return label + c.Faint.Render("   …")
	}
	style := gaugeStyle(c, g.Frac)
	label := style.Render(padRightLocal(g.Label, 4))
	pct := strconv.Itoa(int(normalGaugeFrac(g.Frac)*100+0.5)) + "%"
	return label + style.Render(padLeftLocal(pct, 4))
}

func machineGaugeBar(c Ctx, g SysGauge, width int) string {
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return c.Faint.Render("░")
	}

	inner := width - 2
	boundary := c.Faint.Render("▕")
	if !g.Known {
		return boundary + c.Faint.Render(strings.Repeat("░", inner)) + c.Faint.Render("▏")
	}

	bar := progress.New(
		progress.WithWidth(inner),
		progress.WithFillCharacters(progress.DefaultFullCharFullBlock, progress.DefaultEmptyCharBlock),
		progress.WithColors(gaugeColor(c, g.Frac)),
		progress.WithoutPercentage(),
	)
	bar.EmptyColor = c.Faint.GetForeground()
	return boundary + bar.ViewAs(normalGaugeFrac(g.Frac)) + c.Faint.Render("▏")
}

func normalGaugeFrac(frac float64) float64 {
	if frac < 0 {
		return 0
	}
	if frac > 1 {
		return 1
	}
	return frac
}

func fitGaugeLine(c Ctx, line string, width int) string {
	line = ansi.Truncate(line, width, "")
	if w := lipgloss.Width(line); w < width {
		line += c.pad(width - w)
	}
	return line
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
