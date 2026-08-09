package tui

import (
	"container/list"
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// Range is a selectable reporting window cycled with the `t` key.
type Range int

const (
	RangeToday Range = iota
	Range7d
	Range30d
	RangeAll
)

var rangeOrder = []Range{RangeToday, Range7d, Range30d, RangeAll}

// Label returns the human label for a range.
func (r Range) Label() string {
	switch r {
	case RangeToday:
		return "today"
	case Range7d:
		return "7d"
	case Range30d:
		return "30d"
	default:
		return "all"
	}
}

// Next cycles to the following range (wrapping).
func (r Range) Next() Range {
	for i, v := range rangeOrder {
		if v == r {
			return rangeOrder[(i+1)%len(rangeOrder)]
		}
	}
	return RangeToday
}

// Key is the stable string used to persist a range across launches (distinct
// from Label, which is for display — though they happen to match today).
func (r Range) Key() string { return r.Label() }

// RangeFromKey parses a persisted range key, reporting ok=false for an unknown
// value so the caller can fall back to its default.
func RangeFromKey(k string) (Range, bool) {
	for _, v := range rangeOrder {
		if v.Key() == k {
			return v, true
		}
	}
	return RangeToday, false
}

// Window resolves the range to a [since, until) pair in local time. A zero since
// means open-ended (all). until is always "now" (open upper bound stored as
// zero so the store treats it as open).
//
// DECIDED (issue #4, 1a): 7d/30d are bucket-aligned windows — the last N local
// calendar days INCLUDING today — not rolling 168h/720h spans. This matches how
// the day-bucketed chart displays the data and makes the derived cache keys
// stable within a day instead of changing every wall-clock second. The
// arithmetic is local-midnight time.Date in now's location, never
// time.Truncate: Truncate aligns to UTC epoch boundaries and would reintroduce
// the issue #1 timezone bug behind stable keys.
func (r Range) Window(now time.Time) (since, until time.Time) {
	y, m, d := now.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch r {
	case RangeToday:
		return midnight, time.Time{}
	case Range7d:
		return midnight.AddDate(0, 0, -6), time.Time{}
	case Range30d:
		return midnight.AddDate(0, 0, -29), time.Time{}
	default:
		return time.Time{}, time.Time{}
	}
}

// Sort is a selectable ordering for Browse rows, cycled with the `s` key.
type Sort int

const (
	SortTotal Sort = iota
	SortEvents
	SortName
)

var sortOrder = []Sort{SortTotal, SortEvents, SortName}

// Label returns the human label for a sort mode.
func (s Sort) Label() string {
	switch s {
	case SortTotal:
		return "total"
	case SortEvents:
		return "events"
	default:
		return "name"
	}
}

// Next cycles to the following sort mode (wrapping).
func (s Sort) Next() Sort {
	for i, v := range sortOrder {
		if v == s {
			return sortOrder[(i+1)%len(sortOrder)]
		}
	}
	return SortTotal
}

// drillDims is the Browse drill order: each level groups by one dimension and a
// drill on a row appends a Filter on that dimension before descending.
var drillDims = []string{"tool", "model", "project", "session"}

// Crumb is one entry on the drill-down stack: the dimension we drilled on and
// the value chosen.
type Crumb struct {
	Dim   string
	Value string
}

// DataSource is the read-only query surface the TUI needs from a store. It is an
// interface (not *store.SQLite) so tests can substitute a fake. It is Summarize
// only: the dashboard renders aggregates exclusively, never raw event lists.
type DataSource interface {
	Summarize(ctx context.Context, f store.Filter) (*store.Summary, error)
}

// compile-time guarantee that a *store.Store satisfies DataSource.
var _ DataSource = (store.Store)(nil)

// summaryCacheCap bounds the query cache (issue #4, 1g — it grew without bound
// as quantized windows rolled over and drill paths accumulated). FLOOR: the
// capacity must exceed the number of distinct cache keys one load generation
// can touch — every view's background flight and its apply-side reload replay
// the SAME keys (the warm handoff), so evicting within a generation silently
// sends the apply-side reload back to synchronous SQLite on the UI thread. A
// single view load touches ≤ ~6 keys and a full navigation sweep a few tens;
// 512 leaves an order of magnitude of headroom while still bounding a
// long-running dashboard.
const summaryCacheCap = 512

// summaryCache is a minimal LRU over cache keys. Not safe for concurrent use on
// its own — every access happens under Data.mu.
type summaryCache struct {
	cap int
	ll  *list.List               // front = most recently used
	m   map[string]*list.Element // key → element holding *summaryEntry
}

type summaryEntry struct {
	key string
	sum *store.Summary
}

func newSummaryCache(capacity int) *summaryCache {
	return &summaryCache{cap: capacity, ll: list.New(), m: make(map[string]*list.Element)}
}

func (c *summaryCache) get(k string) (*store.Summary, bool) {
	e, ok := c.m[k]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(e)
	return e.Value.(*summaryEntry).sum, true
}

func (c *summaryCache) put(k string, s *store.Summary) {
	if e, ok := c.m[k]; ok {
		c.ll.MoveToFront(e)
		e.Value.(*summaryEntry).sum = s
		return
	}
	c.m[k] = c.ll.PushFront(&summaryEntry{key: k, sum: s})
	if c.ll.Len() > c.cap {
		e := c.ll.Back()
		c.ll.Remove(e)
		delete(c.m, e.Value.(*summaryEntry).key)
	}
}

// Data wraps a DataSource with a small LRU cache keyed on the resolved query so
// repeated renders within a frame avoid re-hitting SQLite.
//
// The cache is guarded by mu: a background load tea.Cmd warms the cache off the
// UI thread (running the same queries reload() will), so reads from Update/View
// and writes from the load goroutine must not race. The load path is also
// serialised by an in-flight flag in the model, so at most one goroutine writes
// at a time; mu makes that contract enforced rather than assumed.
//
// Cached summaries are immutable once inserted: the UI thread and the load
// goroutine can hold the same *store.Summary at the same time, so accessors
// that sort do so on a per-call copy (copySummary), never on the cached slice.
type Data struct {
	src   DataSource
	now   func() time.Time
	mu    sync.Mutex
	cache *summaryCache
}

// NewData builds a Data over src.
func NewData(src DataSource) *Data {
	return &Data{
		src:   src,
		now:   time.Now,
		cache: newSummaryCache(summaryCacheCap),
	}
}

// Invalidate clears the cache (used on refresh). Swapping the cache instance —
// rather than deleting keys — is load-bearing: in-flight loads write into the
// instance they captured before querying, so their post-invalidation writes
// land in the abandoned cache and are discarded.
func (d *Data) Invalidate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache = newSummaryCache(summaryCacheCap)
}

// filterFor builds a store.Filter from a range, drill stack and group-by dims.
// now is the caller's load-generation clock (Model.qnow): every query of one
// load generation resolves the same instant, so a flight dispatched just before
// a day/hour boundary and its apply-side reload just after still derive
// identical windows — and therefore identical cache keys.
func (d *Data) filterFor(now time.Time, r Range, crumbs []Crumb, groupBy []string) store.Filter {
	since, until := r.Window(now)
	f := store.Filter{Since: since, Until: until, GroupBy: groupBy}
	for _, c := range crumbs {
		applyCrumb(&f, c)
	}
	return f
}

// applyCrumb appends a drill crumb's value to the matching filter dimension.
func applyCrumb(f *store.Filter, c Crumb) {
	switch c.Dim {
	case "tool":
		f.Tools = append(f.Tools, c.Value)
	case "model":
		f.Models = append(f.Models, c.Value)
	case "project":
		f.Projects = append(f.Projects, c.Value)
	case "session":
		f.Sessions = append(f.Sessions, c.Value)
	}
}

// cacheKey derives a stable string key from a filter.
func cacheKey(f store.Filter) string {
	var b strings.Builder
	b.WriteString(f.Since.Format(time.RFC3339))
	b.WriteByte('|')
	b.WriteString(f.Until.Format(time.RFC3339))
	b.WriteByte('|')
	b.WriteString(strings.Join(f.GroupBy, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(f.Tools, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(f.Models, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(f.Projects, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(f.Sessions, ","))
	return b.String()
}

// cachedSummary returns the cached summary for f, if present. It never
// queries: UI-thread paths use it to stay off SQLite entirely and fall back to
// a debounced background load on a miss (detail.go).
func (d *Data) cachedSummary(f store.Filter) (*store.Summary, bool) {
	d.mu.Lock()
	s, ok := d.cache.get(cacheKey(f))
	d.mu.Unlock()
	return s, ok
}

// summarize runs a cached Summarize.
func (d *Data) summarize(f store.Filter) (*store.Summary, error) {
	k := cacheKey(f)
	d.mu.Lock()
	cache := d.cache
	s, ok := cache.get(k)
	d.mu.Unlock()
	if ok {
		return s, nil
	}
	s, err := d.src.Summarize(context.Background(), f)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	// Write into the cache captured BEFORE the query ran: if Invalidate swapped
	// it mid-flight, this lands in the abandoned instance and is discarded — an
	// in-flight load must never repollute a freshly-invalidated cache with
	// pre-invalidation query results.
	cache.put(k, s)
	d.mu.Unlock()
	return s, nil
}

// Totals returns the grand-total bucket for the current range and drill stack.
func (d *Data) Totals(now time.Time, r Range, crumbs []Crumb) (store.Bucket, error) {
	s, err := d.summarize(d.filterFor(now, r, crumbs, nil))
	if err != nil {
		return store.Bucket{}, err
	}
	return s.Totals, nil
}

// TotalsCached is the cache-only twin of Totals.
func (d *Data) TotalsCached(now time.Time, r Range, crumbs []Crumb) (store.Bucket, bool) {
	s, ok := d.cachedSummary(d.filterFor(now, r, crumbs, nil))
	if !ok {
		return store.Bucket{}, false
	}
	return s.Totals, true
}

// GroupBy returns the summary grouped by a single dimension under the current
// range and drill stack, sorted per the sort mode.
func (d *Data) GroupBy(now time.Time, r Range, crumbs []Crumb, dim string, srt Sort) (*store.Summary, error) {
	s, err := d.summarize(d.filterFor(now, r, crumbs, []string{dim}))
	if err != nil {
		return nil, err
	}
	s = copySummary(s)
	sortBuckets(s.Buckets, dim, srt)
	return s, nil
}

// GroupByCached is the cache-only twin of GroupBy.
func (d *Data) GroupByCached(now time.Time, r Range, crumbs []Crumb, dim string, srt Sort) (*store.Summary, bool) {
	s, ok := d.cachedSummary(d.filterFor(now, r, crumbs, []string{dim}))
	if !ok {
		return nil, false
	}
	s = copySummary(s)
	sortBuckets(s.Buckets, dim, srt)
	return s, true
}

// GroupByDims returns the summary grouped by multiple dimensions under the
// current range and drill stack, in the store's key order. One grouped query
// replaces a per-key N+1 (model owners, per-bucket scrub compositions).
func (d *Data) GroupByDims(now time.Time, r Range, crumbs []Crumb, dims []string) (*store.Summary, error) {
	s, err := d.summarize(d.filterFor(now, r, crumbs, dims))
	if err != nil {
		return nil, err
	}
	return copySummary(s), nil
}

// DrillDim returns the grouping dimension for a given drill depth (0-based).
func DrillDim(depth int) (string, bool) {
	if depth < 0 || depth >= len(drillDims) {
		return "", false
	}
	return drillDims[depth], true
}

// timelineDim maps a range to its timeline bucket granularity.
func timelineDim(r Range) string {
	if r == RangeToday {
		return "hour"
	}
	return "day"
}

// sortTimeline orders timeline buckets ascending by their (lexically sortable)
// time key.
func sortTimeline(b []store.Bucket, dim string) {
	sort.SliceStable(b, func(i, j int) bool {
		return b[i].Keys[dim] < b[j].Keys[dim]
	})
}

// Timeline returns per-day (or per-hour for short ranges) buckets across the
// current range, ascending by time.
func (d *Data) Timeline(now time.Time, r Range, crumbs []Crumb) (*store.Summary, string, error) {
	dim := timelineDim(r)
	s, err := d.summarize(d.filterFor(now, r, crumbs, []string{dim}))
	if err != nil {
		return nil, dim, err
	}
	s = copySummary(s)
	sortTimeline(s.Buckets, dim)
	return s, dim, nil
}

// TimelineCached is the cache-only twin of Timeline.
func (d *Data) TimelineCached(now time.Time, r Range, crumbs []Crumb) (*store.Summary, string, bool) {
	dim := timelineDim(r)
	s, ok := d.cachedSummary(d.filterFor(now, r, crumbs, []string{dim}))
	if !ok {
		return nil, dim, false
	}
	s = copySummary(s)
	sortTimeline(s.Buckets, dim)
	return s, dim, true
}

// windowTotalsFilter builds the ungrouped filter for an explicit [since,until)
// window under the drill stack (shared by WindowTotals and its cached twin so
// both derive identical cache keys).
func windowTotalsFilter(since, until time.Time, crumbs []Crumb) store.Filter {
	f := store.Filter{Since: since, Until: until}
	for _, c := range crumbs {
		applyCrumb(&f, c)
	}
	return f
}

// WindowTotals returns the grand-total bucket for an explicit [since,until)
// window (the previous-period delta chips): one ungrouped Summarize whose
// Totals is the store-level aggregate — no in-memory summing of grouped rows,
// and Sessions is the real COUNT(DISTINCT) instead of a zero placeholder.
func (d *Data) WindowTotals(since, until time.Time, crumbs []Crumb) (store.Bucket, error) {
	s, err := d.summarize(windowTotalsFilter(since, until, crumbs))
	if err != nil {
		return store.Bucket{}, err
	}
	return s.Totals, nil
}

// WindowTotalsCached is the cache-only twin of WindowTotals.
func (d *Data) WindowTotalsCached(since, until time.Time, crumbs []Crumb) (store.Bucket, bool) {
	s, ok := d.cachedSummary(windowTotalsFilter(since, until, crumbs))
	if !ok {
		return store.Bucket{}, false
	}
	return s.Totals, true
}

// copySummary returns a shallow copy of s with its own Buckets slice, so the
// caller can sort (and hold) the result without mutating the cached summary.
// Cache entries are shared between the UI thread and the background load
// goroutine — sorting them in place raced — and per-caller copies also stop a
// later re-sort of the same key under a different mode from silently reordering
// rows an earlier caller still displays. Bucket contents (including the Keys
// maps) stay shared; nothing writes to them after Summarize builds them.
func copySummary(s *store.Summary) *store.Summary {
	out := *s
	out.Buckets = append([]store.Bucket(nil), s.Buckets...)
	return &out
}

// sortBuckets orders buckets in place by the chosen sort mode. Default ordering
// is descending total so the largest consumers surface first.
func sortBuckets(b []store.Bucket, dim string, srt Sort) {
	switch srt {
	case SortName:
		sort.SliceStable(b, func(i, j int) bool {
			return b[i].Keys[dim] < b[j].Keys[dim]
		})
	case SortEvents:
		sort.SliceStable(b, func(i, j int) bool {
			return b[i].Events > b[j].Events
		})
	default:
		sort.SliceStable(b, func(i, j int) bool {
			return b[i].Total > b[j].Total
		})
	}
}
