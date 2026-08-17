package views

import (
	"strconv"
	"sync"

	"github.com/RandomCodeSpace/aiusage/store"
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
// so a same-generation re-apply after a cache rebuild still re-keys); the body
// additionally keys on its (frame kind, dim, w, h) — the kind rather than the
// mode, because one mode renders two different braille bodies (the detented
// panes above the two-pane floor, the log chart below it). The key deliberately
// EXCLUDES the scrub index — the frame is built once and the highlight is applied
// to the finished canvases as a post-pass (paintTrendHighlights), so a scrub
// step re-renders at most the body string, never the braille — and excludes the
// sys gauges, which render outside the memoized panes. All cached renders drop
// together whenever the dataset identity changes (per-generation clearing), so
// the maps stay bounded by one view's scrub positions + sparkline set.
type HeroMemo struct {
	mu sync.Mutex

	// Dataset identity: applied load generation + timeline slice identity.
	gen uint64
	ptr *store.Bucket
	n   int

	// Built body (no highlights) and the key it was built for.
	dim   string
	kind  heroFrameKind
	w, h  int
	built *heroFrame

	// lock deliberately OUTLIVES the dataset identity: the detented axes must
	// hold their step across refreshes, and re-quantizing on every applied
	// generation is exactly the axis breathing the detent exists to stop.
	lock detentLock

	frames map[int]string    // scrub index (-1 = unpinned) → rendered body
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
	m.built = nil
	m.frames = map[int]string{}
	m.sparks = map[string]string{}
}

// frame returns the hero body for (gen, buckets, dim, kind, w, h) with the
// now/scrub highlight for scrubIdx applied, building the braille panes only
// when the dataset, kind or geometry changed. Highlighting paints the built
// canvases, renders, then restores them, so the retained frame always stays
// clean. ok=false when the body cannot be built (unparseable time keys, no
// drawable segments) and the caller must fall back.
func (m *HeroMemo) frame(c Ctx, gen uint64, buckets []store.Bucket, dim string, kind heroFrameKind, w, h, scrubIdx int) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncIdentity(gen, buckets)
	if m.built == nil || m.dim != dim || m.kind != kind || m.w != w || m.h != h {
		f, ok := buildHeroFrame(c, buckets, dim, kind, w, h, &m.lock)
		if !ok {
			return "", false
		}
		m.built = f
		m.dim, m.kind, m.w, m.h = dim, kind, w, h
		m.frames = map[int]string{}
		m.builds++
	}
	key := scrubIdx
	if key < 0 || key >= len(m.built.times) {
		key = -1
	}
	if s, ok := m.frames[key]; ok {
		return s, true
	}
	s := m.built.render(c, key)
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
