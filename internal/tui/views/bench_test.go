package views

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// bench_test.go measures the Overview's sys resource strip. It is the one row on
// the screen rebuilt on the 2s sysmon tick rather than on a data refresh, so its
// per-render cost is paid continuously for as long as the TUI is open, and a
// string that changes every tick also defeats the renderer's unchanged-line skip.
//
// The rolling heat variant of this row (one styled cell per sample) was reverted
// for exactly that cost; BenchmarkSysStripHeatCells keeps the removed treatment
// measurable at the same geometry, so a future attempt at it can be compared
// against the bar instead of guessed at.

// benchSysWidth is a representative Overview content width. With three gauges it
// yields a 32-cell gauge cell and a 10-cell fill each.
const (
	benchSysWidth = 100
	benchSysFill  = 10 // per-gauge fill width at benchSysWidth
	benchSysCount = 3  // cpu / mem / disk
)

func benchSysGauges() []SysGauge {
	return []SysGauge{
		{Label: "cpu", Frac: 0.38, Text: "0.8/2 cpu", Known: true},
		{Label: "mem", Frac: 0.72, Text: "2.9G/4.0G", Known: true},
		{Label: "disk", Frac: 0.93, Text: "28G/30G", Known: true},
	}
}

var benchSysRow string

// BenchmarkSysStrip measures one full sys strip row: three instantaneous bar
// gauges at benchSysWidth, the whole string the Overview rebuilds per sys tick.
func BenchmarkSysStrip(b *testing.B) {
	c := sysTestCtx()
	g := benchSysGauges()
	b.ReportAllocs()
	for b.Loop() {
		benchSysRow = SysStrip(c, g, benchSysWidth)
	}
}

// BenchmarkSysStripHeatCells measures the fills alone as the reverted rolling
// strip drew them: one individually styled cell per sample, over a full sample
// window, at the same per-gauge width the bar occupies. The bar renders each
// fill in two Render calls whatever the width; this pays one per cell.
func BenchmarkSysStripHeatCells(b *testing.B) {
	c := sysTestCtx()
	// A full ring (sysRingCap in package tui) is the steady state after eight
	// minutes of ticks; the strip shows the newest benchSysFill of it.
	hist := make([]float64, 240)
	for i := range hist {
		hist[i] = 0.38
	}
	ink := func(v float64) lipgloss.Style { return c.fg(gaugeColor(c, v)) }
	b.ReportAllocs()
	for b.Loop() {
		for i := 0; i < benchSysCount; i++ {
			benchSysRow = heatStrip(c, hist, benchSysFill, 1, ink)
		}
	}
}
