package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/internal/sysmon"
	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// sysgauge.go drives the container resource gauges (CPU/mem/disk for the current
// pod) shown as a compact strip on the Overview tab. It samples on its own short
// ticker — separate from the 10s data-refresh poll — because CPU is a rate that
// needs two closely-spaced samples to read meaningfully. Sampling runs INSIDE
// the tick Cmd, off the UI thread: it includes a syscall.Statfs on the workspace
// path, and a hung network/FUSE mount must stall the tick goroutine, never
// Update. The render memo's key also excludes the sys snapshot, so a fresh
// sample never rebuilds the hero chart.

// sysInterval is the resource-gauge sample cadence. Short enough that the CPU
// gauge feels live, long enough to be negligible overhead.
const sysInterval = 2 * time.Second

// sysTickMsg delivers one background resource sample every sysInterval.
type sysTickMsg struct{ snap sysmon.Snapshot }

// sysTickCmd schedules the next resource-gauge sample and takes it inside the
// Cmd goroutine, so the sample's file reads and Statfs never run in Update.
func sysTickCmd(mon *sysmon.Monitor) tea.Cmd {
	return tea.Tick(sysInterval, func(time.Time) tea.Msg {
		var s sysmon.Snapshot
		if mon != nil {
			s = mon.Sample()
		}
		return sysTickMsg{snap: s}
	})
}

// handleSysTick stores the background sample and re-arms the ticker. It always
// re-arms so the strip stays live for the session's lifetime.
func (m Model) handleSysTick(msg sysTickMsg) (tea.Model, tea.Cmd) {
	if m.mon != nil {
		m.sys = msg.snap
	}
	return m, sysTickCmd(m.mon)
}

// sysGauges maps the latest sysmon snapshot into the view-layer gauge list the
// Overview strip renders, in fixed CPU/mem/disk order. Before the first sample
// every gauge is unknown and the strip renders its placeholder rather than bars
// at an invented zero.
func (m Model) sysGauges() []views.SysGauge {
	s := m.sys
	return []views.SysGauge{
		{Label: "cpu", Frac: s.CPU.Frac, Text: s.CPU.Text, Known: s.CPU.Known},
		{Label: "mem", Frac: s.Mem.Frac, Text: s.Mem.Text, Known: s.Mem.Known},
		{Label: "disk", Frac: s.Disk.Frac, Text: s.Disk.Text, Known: s.Disk.Known},
	}
}
