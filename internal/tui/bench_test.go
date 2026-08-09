package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// benchModel builds a loaded 120x40 Overview model over fakeData with a pinned
// clock. Cache keys are quantized to bucket granularity and every query of a
// load generation resolves the same clock (Model.loadNow), so the warm path
// stays warm; pinning data.now keeps the benchmark hermetic across hour/day
// boundaries anyway.
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
//
// ACCEPTED BASELINE (issue #41): ~19 allocs/op, ~2.4 KB/op. It drifted 37 -> 50
// across the perf and freshness waves without anyone deciding it should; the
// two cheap causes were reclaimed (cacheKey assembles into a stack scratch
// instead of a strings.Builder over two Format strings; sortBuckets /
// sortTimeline use the generic stable sort instead of the reflect-based one),
// and the 19 that remain are each buying something and are recorded rather than
// chased:
//
//	5  the cache-key strings themselves - one per distinct query of the load
//	6  copySummary's per-caller copy x3 - the fix for the UI thread and the
//	   load goroutine sorting one shared cached slice
//	5  buildScrubComp's per-bucket compositions - what makes a scrub sweep
//	   cost zero queries
//	3  the one-element GroupBy slice in each filter
//
// Measure the next drift against this list, not against a fresh guess.
func BenchmarkReload(b *testing.B) {
	m := benchModel(b)
	b.ReportAllocs()
	for b.Loop() {
		m.reload()
	}
}

// BenchmarkScrubStep measures one pinned scrub step on warm data: the KPI
// re-price projects from the timeline bucket and the side bars read the
// prewarmed composition, so this is pure in-memory work — no store, no cache.
func BenchmarkScrubStep(b *testing.B) {
	m := benchModel(b)
	m.scrubPinned = true
	dir := 1
	b.ReportAllocs()
	for b.Loop() {
		if m.scrubIndex <= 0 {
			dir = 1
		} else if m.scrubIndex >= len(m.tlData.Buckets)-1 {
			dir = -1
		}
		m.scrubBy(dir)
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
