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

// sysHistoryLimit retains two minutes at the normal sampling cadence. History
// is session-local and independent of the selected usage range.
const sysHistoryLimit = 60

// sysHistory holds CPU, memory, and disk fractions in the same fixed order as
// sysGauges. A missing reading clears only that series so the renderer cannot
// draw a line across an unknown interval.
type sysHistory [3][]float64

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
		m.sysHistory = appendSysHistory(m.sysHistory, msg.snap)
	}
	return m, sysTickCmd(m.mon)
}

// appendSysHistory returns a fresh history value. Model is copied by Bubble
// Tea, so cloning each short series avoids mutating an older model through a
// shared slice backing array.
func appendSysHistory(history sysHistory, snap sysmon.Snapshot) sysHistory {
	gauges := [3]sysmon.Gauge{snap.CPU, snap.Mem, snap.Disk}
	for i, gauge := range gauges {
		if !gauge.Known {
			history[i] = nil
			continue
		}

		start := 0
		if len(history[i]) >= sysHistoryLimit {
			start = len(history[i]) - sysHistoryLimit + 1
		}
		next := make([]float64, 0, min(sysHistoryLimit, len(history[i])-start+1))
		next = append(next, history[i][start:]...)
		next = append(next, gauge.Frac)
		history[i] = next
	}
	return history
}

// sysGauges maps the latest sysmon snapshot into the view-layer gauge list the
// Overview strip renders, in fixed CPU/mem/disk order. Before the first sample
// every gauge is unknown and the strip renders its placeholder rather than bars
// at an invented zero.
func (m Model) sysGauges() []views.SysGauge {
	s := m.sys
	return []views.SysGauge{
		{Label: "cpu", Frac: s.CPU.Frac, Text: s.CPU.Text, Known: s.CPU.Known, History: m.sysHistory[0]},
		{Label: "mem", Frac: s.Mem.Frac, Text: s.Mem.Text, Known: s.Mem.Known, History: m.sysHistory[1]},
		{Label: "disk", Frac: s.Disk.Frac, Text: s.Disk.Text, Known: s.Disk.Known, History: m.sysHistory[2]},
	}
}
