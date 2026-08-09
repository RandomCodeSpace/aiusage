package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// benchModel builds a loaded 120x40 Overview model over fakeData with a pinned
// clock: filterFor keys the cache on now() at second precision, so a rolling
// clock would silently turn the warm path cold mid-run. prevTotals still reads
// the real time.Now(), but with fakeData its once-per-second cache miss is an
// in-memory call, not I/O.
func benchModel(b *testing.B) Model {
	b.Helper()
	m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db"})
	fixed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.Local)
	m.data.now = func() time.Time { return fixed }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return loadOnce(tm.(Model))
}

// BenchmarkReload measures a warm reload of the Overview view: every query
// hits the in-memory cache, so this is the pure recompute/sort/view-model
// rebuild cost that runs on the UI thread today.
func BenchmarkReload(b *testing.B) {
	m := benchModel(b)
	b.ReportAllocs()
	for b.Loop() {
		m.reload()
	}
}

var benchFrame string

// BenchmarkView measures one full View render of the loaded Overview
// dashboard (chrome + hero braille chart + KPI strip + side bars) at 120x40.
func BenchmarkView(b *testing.B) {
	m := benchModel(b)
	b.ReportAllocs()
	for b.Loop() {
		benchFrame = m.View().Content
	}
}
