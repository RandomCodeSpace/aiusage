package views

import (
	"strconv"
	"sync"
	"time"

	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// HeroMemo caches the Overview's rendered braille hero chart and KPI sparkline
// rows across frames, so a message that does not change the data — the 2s sys
// tick, spinner frames, scrub steps — never rebuilds them inside View(). The
// root model owns one instance behind a pointer so it survives Bubble Tea's
// value-copied Model; all access happens on the UI thread, the mutex just makes
// that contract enforced rather than assumed.
//
// Keying (issue #4, 1f): the dataset identity is the APPLIED load generation
// plus the timeline slice identity (every load applies a freshly copied slice,
// so a same-generation re-apply after a cache rebuild still re-keys); the chart
// additionally keys on its (w, h). The key deliberately EXCLUDES the scrub
// index — the chart is built once and the now/scrub highlight is applied to the
// finished canvas as a post-pass (paintTrendHighlights), so a scrub step
// re-renders at most the canvas string, never the braille — and excludes the
// sys gauges, which render outside the memoized panes. All cached renders drop
// together whenever the dataset identity changes (per-generation clearing), so
// the maps stay bounded by one view's scrub positions + sparkline set.
type HeroMemo struct {
	mu sync.Mutex

	// Dataset identity: applied load generation + timeline slice identity.
	gen uint64
	ptr *store.Bucket
	n   int

	// Built chart (no highlights) and the geometry it was built for.
	dim   string
	w, h  int
	chart *timeserieslinechart.Model
	times []time.Time

	frames map[int]string    // scrub index (-1 = unpinned) → rendered chart body
	sparks map[string]string // comp key + width → rendered sparkline row

	builds int // braille chart constructions (test seam for the rebuild contract)
}

// NewHeroMemo returns an empty render memo.
func NewHeroMemo() *HeroMemo {
	return &HeroMemo{frames: map[int]string{}, sparks: map[string]string{}}
}

// Builds reports how many times the braille chart has been constructed. Tests
// use it to prove that re-renders with unchanged data never rebuild the chart.
func (m *HeroMemo) Builds() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.builds
}

// syncIdentity drops every cached render when the dataset identity changed.
// Caller holds mu.
func (m *HeroMemo) syncIdentity(gen uint64, buckets []store.Bucket) {
	var ptr *store.Bucket
	if len(buckets) > 0 {
		ptr = &buckets[0]
	}
	if gen == m.gen && ptr == m.ptr && len(buckets) == m.n {
		return
	}
	m.gen, m.ptr, m.n = gen, ptr, len(buckets)
	m.chart = nil
	m.times = nil
	m.frames = map[int]string{}
	m.sparks = map[string]string{}
}

// chartBody returns the hero chart body for (gen, buckets, dim, w, h) with the
// now/scrub highlight for scrubIdx applied, building the braille chart only
// when the dataset or geometry changed. Highlighting paints the built canvas,
// renders, then restores it, so the retained chart always stays clean.
// ok=false when the buckets carry no parseable time keys (the caller renders
// its empty frame).
func (m *HeroMemo) chartBody(c Ctx, gen uint64, buckets []store.Bucket, dim string, w, h, scrubIdx int) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncIdentity(gen, buckets)
	if m.chart == nil || m.dim != dim || m.w != w || m.h != h {
		chart, times, ok := buildTrendChart(c, buckets, dim, w, h)
		if !ok {
			return "", false
		}
		m.chart, m.times = chart, times
		m.dim, m.w, m.h = dim, w, h
		m.frames = map[int]string{}
		m.builds++
	}
	key := scrubIdx
	if key < 0 || key >= len(m.times) {
		key = -1
	}
	if s, ok := m.frames[key]; ok {
		return s, true
	}
	paintTrendHighlights(c, m.chart, m.times, key)
	s := fitHeight(c.mark(zoneHero, m.chart.View()), h)
	clearTrendHighlights(m.chart, m.times, key)
	m.frames[key] = s
	return s, true
}

// spark returns the memoized sparkline row for one KPI series at width w,
// invoking build only on a miss. The series derives solely from the timeline,
// so it is stable for the lifetime of an applied dataset.
func (m *HeroMemo) spark(gen uint64, buckets []store.Bucket, key string, w int, build func() string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncIdentity(gen, buckets)
	k := key + "|" + strconv.Itoa(w)
	if s, ok := m.sparks[k]; ok {
		return s
	}
	s := build()
	m.sparks[k] = s
	return s
}
