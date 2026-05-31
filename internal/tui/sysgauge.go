package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"aiusage/internal/tui/views"
)

// sysgauge.go drives the container resource gauges (CPU/mem/disk for the current
// pod) shown as a compact strip on the Overview tab. It samples on its own short
// ticker — separate from the 10s data-refresh poll — because CPU is a rate that
// needs two closely-spaced samples to read meaningfully. Sampling is a handful
// of small cgroup file reads, so it runs synchronously in the tick handler.

// sysInterval is the resource-gauge sample cadence. Short enough that the CPU
// gauge feels live, long enough to be negligible overhead.
const sysInterval = 2 * time.Second

// sysTickMsg fires every sysInterval to re-sample container resource usage.
type sysTickMsg struct{}

// sysTickCmd schedules the next resource-gauge sample.
func sysTickCmd() tea.Cmd {
	return tea.Tick(sysInterval, func(time.Time) tea.Msg { return sysTickMsg{} })
}

// handleSysTick samples the container's CPU/memory/disk usage and re-arms the
// ticker. It always re-arms so the strip stays live for the session's lifetime.
func (m Model) handleSysTick() (tea.Model, tea.Cmd) {
	if m.mon != nil {
		m.sys = m.mon.Sample()
	}
	return m, sysTickCmd()
}

// sysGauges maps the latest sysmon snapshot into the view-layer gauge list the
// Overview strip renders, in fixed CPU/mem/disk order. Returns nil before the
// first sample so the strip renders a thin placeholder rather than empty bars.
func (m Model) sysGauges() []views.SysGauge {
	s := m.sys
	return []views.SysGauge{
		{Label: "cpu", Frac: s.CPU.Frac, Text: s.CPU.Text, Known: s.CPU.Known},
		{Label: "mem", Frac: s.Mem.Frac, Text: s.Mem.Text, Known: s.Mem.Known},
		{Label: "disk", Frac: s.Disk.Frac, Text: s.Disk.Text, Known: s.Disk.Known},
	}
}
