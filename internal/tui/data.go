package tui

import (
	"cmp"
	"container/list"
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
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

// spanDays is the range's width in whole local calendar days — the unit one
// press of [ / ] moves the window by. Zero means the range has no finite span
// (all) and therefore cannot be stepped.
func (r Range) spanDays() int {
	switch r {
	case RangeToday:
		return 1
	case Range7d:
		return 7
	case Range30d:
		return 30
	default:
		return 0
	}
}

// Steppable reports whether the range has a finite span the [ / ] keys can
// shift.
func (r Range) Steppable() bool { return r.spanDays() > 0 }

// spanLabel names the range's width for a stepped window, where "today" would
// be a lie.
func (r Range) spanLabel() string {
	if r == RangeToday {
		return "1d"
	}
	return r.Label()
}

// Span is the reporting window the dashboard is showing: a Range plus how many
// whole calendar spans it sits BEHIND the live window. Step 0 is the live
// window ("now"), -1 the span before it, and so on; Step is never positive
// because stepping stops at the present. Every query keys off a Span, so a
// stepped window is just another (stable, quantized) cache key — no separate
// query path, no UI-thread work.
type Span struct {
	R    Range
	Step int
}

// Window resolves the span to a [since, until) pair in local time. The live
// window keeps the range's own open upper bound; a stepped window is a CLOSED
// calendar span computed by local-midnight arithmetic off the same quantized
// origin (issue #4, 1a), so its bucket boundaries — and therefore its cache
// keys — line up exactly with the live window's.
func (s Span) Window(now time.Time) (since, until time.Time) {
	base, open := s.R.Window(now)
	days := s.R.spanDays()
	if s.Step >= 0 || days == 0 {
		return base, open
	}
	n := -s.Step
	return base.AddDate(0, 0, -days*n), base.AddDate(0, 0, -days*(n-1))
}

// Label names the window: the plain range label while live, and the span width
// plus the window's first local day once stepped ("7d @ 2026-07-27"), so a
// stepped view can never be read as "now". It is the widest of labelForms.
func (s Span) Label(now time.Time) string { return s.labelForms(now)[0] }

// labelForms names the window in every form the header may show, widest first.
// Every form is COMPLETE on its own: a narrow terminal picks a shorter name
// instead of truncating a longer one, so a dangling fragment like "2026-07",
// which reads as a date but is not one, can never reach the screen. The list
// always holds at least one form.
//
// The rungs below the full date are:
//
//   - month-day ("7d @ 07-27"), offered ONLY while the window starts in the
//     current year, where the dropped year cannot be misread;
//   - the step offset ("7d-2" = two whole 7d windows before the live one),
//     which is the floor: it is exact at any width because the step count and
//     the span width resolve the window against the same clock the live one
//     uses, and it costs the same handful of cells whatever the date is.
//
// A live window has one form, its range label, and never names a day.
func (s Span) labelForms(now time.Time) []string {
	if s.Step >= 0 || !s.R.Steppable() {
		return []string{s.R.Label()}
	}
	since, _ := s.Window(now)
	width := s.R.spanLabel()
	forms := []string{width + " @ " + since.Format("2006-01-02")}
	if since.Year() == now.Year() {
		forms = append(forms, width+" @ "+since.Format("01-02"))
	}
	// Step is negative, so Itoa supplies the sign: "7d" + "-2".
	return append(forms, width+strconv.Itoa(s.Step))
}

// Sort is a selectable ordering for Browse rows, cycled with the `s` key.
type Sort int

const (
	SortTotal Sort = iota
	SortEvents
	SortName
	// SortCost orders by stamped cost. It exists because the Activity tab ranks
	// invocations by spend and the sort key is shared state — one cycle drives
	// every view, so a mode a view needs has to be on the cycle.
	SortCost
)

var sortOrder = []Sort{SortTotal, SortEvents, SortName, SortCost}

// Label returns the human label for a sort mode.
func (s Sort) Label() string {
	switch s {
	case SortTotal:
		return "total"
	case SortEvents:
		return "events"
	case SortCost:
		return "cost"
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
// interface (not *store.SQLite) so tests can substitute a fake. It is aggregates
// only: the dashboard renders summaries exclusively, never raw event lists —
// which is why ListEvents / ListActivity are absent from both ledgers' halves.
type DataSource interface {
	Summarize(ctx context.Context, f store.Filter) (*store.Summary, error)

	// SummarizeActivity and TopActivity are the activity ledger's half, read by
	// the Activity tab. They are on the same interface rather than an optional
	// one a type assertion sniffs for: a source that cannot answer them would
	// leave that tab rendering "no rows" over a populated ledger, and a missing
	// method is a compile error where a failed assertion is a silent lie.
	SummarizeActivity(ctx context.Context, f store.ActivityFilter) (*store.ActivitySummary, error)
	TopActivity(ctx context.Context, f store.ActivityFilter, by store.ActivityOrder, limit int) ([]store.ActivityBucket, error)

	// SummarizeTurnContext and TopTurnContext are the turn-context table's half,
	// read by the Activity tab's five non-call pivots. They are on this
	// interface for the same reason the activity pair is, and they take the
	// dimension the way the store does — as a REQUIRED argument the caller
	// cannot leave unset, so a pivot is always exactly one partition.
	SummarizeTurnContext(ctx context.Context, dim model.TurnDimension, f store.ActivityFilter) (*store.TurnContextSummary, error)
	TopTurnContext(ctx context.Context, dim model.TurnDimension, f store.ActivityFilter, by store.ActivityOrder, limit int) ([]store.TurnContextBucket, error)
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

// lru is a minimal LRU over cache keys. Not safe for concurrent use on its own —
// every access happens under Data.mu. It is generic over the cached value
// because the two ledgers answer with different shapes (a usage summary, an
// activity ranking) and a cache holding `any` would hand every reader a type
// assertion to get wrong.
type lru[T any] struct {
	cap int
	ll  *list.List               // front = most recently used
	m   map[string]*list.Element // key → element holding *lruEntry[T]
}

type lruEntry[T any] struct {
	key string
	val T
}

func newLRU[T any](capacity int) *lru[T] {
	return &lru[T]{cap: capacity, ll: list.New(), m: make(map[string]*list.Element)}
}

func (c *lru[T]) get(k string) (T, bool) {
	e, ok := c.m[k]
	if !ok {
		var zero T
		return zero, false
	}
	c.ll.MoveToFront(e)
	return e.Value.(*lruEntry[T]).val, true
}

func (c *lru[T]) put(k string, v T) {
	if e, ok := c.m[k]; ok {
		c.ll.MoveToFront(e)
		e.Value.(*lruEntry[T]).val = v
		return
	}
	c.m[k] = c.ll.PushFront(&lruEntry[T]{key: k, val: v})
	if c.ll.Len() > c.cap {
		e := c.ll.Back()
		c.ll.Remove(e)
		delete(c.m, e.Value.(*lruEntry[T]).key)
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
	cache *lru[*store.Summary]
	// act caches the activity ledger's grouped summaries and rank is its
	// ranking twin. They are separate instances, not one cache of `any`: the
	// two answer different questions and their keys are built differently (a
	// ranking key carries the metric and the cap).
	act  *lru[*store.ActivitySummary]
	rank *lru[[]store.ActivityBucket]
	// tctx and tcRank are the turn-context table's pair. They are separate
	// instances again, and their keys carry the DIMENSION: the same window
	// grouped the same way along two axes is two different answers over the same
	// dollars, and one cache holding both under one key would serve a skill
	// ranking to an agent pivot — the exact cross-partition confusion the store
	// refuses to express in SQL, reintroduced in memory.
	tctx   *lru[*store.TurnContextSummary]
	tcRank *lru[[]store.TurnContextBucket]
}

// NewData builds a Data over src.
func NewData(src DataSource) *Data {
	return &Data{
		src:    src,
		now:    time.Now,
		cache:  newLRU[*store.Summary](summaryCacheCap),
		act:    newLRU[*store.ActivitySummary](summaryCacheCap),
		rank:   newLRU[[]store.ActivityBucket](summaryCacheCap),
		tctx:   newLRU[*store.TurnContextSummary](summaryCacheCap),
		tcRank: newLRU[[]store.TurnContextBucket](summaryCacheCap),
	}
}

// Invalidate clears the caches (used on refresh). Swapping the cache instances —
// rather than deleting keys — is load-bearing: in-flight loads write into the
// instance they captured before querying, so their post-invalidation writes
// land in the abandoned cache and are discarded.
func (d *Data) Invalidate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache = newLRU[*store.Summary](summaryCacheCap)
	d.act = newLRU[*store.ActivitySummary](summaryCacheCap)
	d.rank = newLRU[[]store.ActivityBucket](summaryCacheCap)
	d.tctx = newLRU[*store.TurnContextSummary](summaryCacheCap)
	d.tcRank = newLRU[[]store.TurnContextBucket](summaryCacheCap)
}

// filterFor builds a store.Filter from a range, drill stack and group-by dims.
// now is the caller's load-generation clock (Model.qnow): every query of one
// load generation resolves the same instant, so a flight dispatched just before
// a day/hour boundary and its apply-side reload just after still derive
// identical windows — and therefore identical cache keys.
func (d *Data) filterFor(now time.Time, sp Span, crumbs []Crumb, groupBy []string) store.Filter {
	since, until := sp.Window(now)
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

// cacheKeyBuf is the scratch size cacheKey assembles into: two RFC3339
// timestamps (25 bytes each at worst), six separators and room for the drill
// values a realistic key carries. It is a stack array rather than a
// strings.Builder because cacheKey runs five times per reload and the builder's
// growth steps plus the two intermediate Format strings were, together, nearly
// half of that reload's allocations (issue #41). Overflowing this is correct,
// just one heap growth.
const cacheKeyBuf = 192

// cacheKey derives a stable string key from a filter. The layout is
// since|until|groupBy|tools|models|projects|sessions with each list
// comma-joined — byte for byte what the strings.Builder form produced, since a
// changed key would silently miss every warm entry a load generation depends
// on.
func cacheKey(f store.Filter) string {
	var scratch [cacheKeyBuf]byte
	b := scratch[:0]
	b = f.Since.AppendFormat(b, time.RFC3339)
	b = append(b, '|')
	b = f.Until.AppendFormat(b, time.RFC3339)
	for _, list := range [...][]string{f.GroupBy, f.Tools, f.Models, f.Projects, f.Sessions} {
		b = append(b, '|')
		for i, v := range list {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, v...)
		}
	}
	return string(b)
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

// summarize runs a cached Summarize under ctx: the load generation's
// cancellation signal (Model.qctx). A cache hit is served whatever ctx says —
// it costs nothing and a superseded flight's warm answers are still correct —
// but a MISS on a cancelled context returns immediately instead of opening a
// full-ledger aggregation whose result is already destined for the bin. That
// check is the stage boundary a superseded flight bails at even when the source
// itself ignores cancellation; a source that honours it (the real store, via
// sqlite3_interrupt) also aborts the query already running.
func (d *Data) summarize(ctx context.Context, f store.Filter) (*store.Summary, error) {
	k := cacheKey(f)
	d.mu.Lock()
	cache := d.cache
	s, ok := cache.get(k)
	d.mu.Unlock()
	if ok {
		return s, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := d.src.Summarize(ctx, f)
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
func (d *Data) Totals(ctx context.Context, now time.Time, sp Span, crumbs []Crumb) (store.Bucket, error) {
	s, err := d.summarize(ctx, d.filterFor(now, sp, crumbs, nil))
	if err != nil {
		return store.Bucket{}, err
	}
	return s.Totals, nil
}

// TotalsCached is the cache-only twin of Totals.
func (d *Data) TotalsCached(now time.Time, sp Span, crumbs []Crumb) (store.Bucket, bool) {
	s, ok := d.cachedSummary(d.filterFor(now, sp, crumbs, nil))
	if !ok {
		return store.Bucket{}, false
	}
	return s.Totals, true
}

// GroupBy returns the summary grouped by a single dimension under the current
// range and drill stack, sorted per the sort mode.
func (d *Data) GroupBy(ctx context.Context, now time.Time, sp Span, crumbs []Crumb, dim string, srt Sort) (*store.Summary, error) {
	s, err := d.summarize(ctx, d.filterFor(now, sp, crumbs, []string{dim}))
	if err != nil {
		return nil, err
	}
	s = copySummary(s)
	sortBuckets(s.Buckets, dim, srt)
	return s, nil
}

// GroupByCached is the cache-only twin of GroupBy.
func (d *Data) GroupByCached(now time.Time, sp Span, crumbs []Crumb, dim string, srt Sort) (*store.Summary, bool) {
	s, ok := d.cachedSummary(d.filterFor(now, sp, crumbs, []string{dim}))
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
func (d *Data) GroupByDims(ctx context.Context, now time.Time, sp Span, crumbs []Crumb, dims []string) (*store.Summary, error) {
	s, err := d.summarize(ctx, d.filterFor(now, sp, crumbs, dims))
	if err != nil {
		return nil, err
	}
	return copySummary(s), nil
}

// activityFilterFor builds a store.ActivityFilter from the same window + drill
// stack every usage query keys off, so the Activity tab shows the invocations
// that happened inside exactly the window the rest of the dashboard is showing.
// The crumb dimensions the activity ledger has no column for are dropped by
// applyActivityCrumb rather than silently ignored — see there.
func (d *Data) activityFilterFor(now time.Time, sp Span, crumbs []Crumb, groupBy []string) store.ActivityFilter {
	since, until := sp.Window(now)
	f := store.ActivityFilter{Since: since, Until: until, GroupBy: groupBy}
	for _, c := range crumbs {
		applyActivityCrumb(&f, c)
	}
	return f
}

// applyActivityCrumb appends a drill crumb's value to the matching activity
// filter dimension. Every dimension the Browse drill can produce (tool, model,
// project, session) is a column on activity_events too, so a crumb carried onto
// this tab always narrows it — an unknown dimension would leave the filter wide
// open, which is why the drill order and this switch have to stay in step.
func applyActivityCrumb(f *store.ActivityFilter, c Crumb) {
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

// activityKey derives a stable cache key from an activity filter, with the same
// layout rules as cacheKey: two timestamps then each list comma-joined. The
// dimension order matters (it is the grouping), so the lists are never sorted.
func activityKey(f store.ActivityFilter) string {
	var scratch [cacheKeyBuf]byte
	b := scratch[:0]
	b = f.Since.AppendFormat(b, time.RFC3339)
	b = append(b, '|')
	b = f.Until.AppendFormat(b, time.RFC3339)
	for _, list := range [...][]string{f.GroupBy, f.Tools, f.Kinds, f.Names, f.Projects, f.Sessions, f.Models} {
		b = append(b, '|')
		for i, v := range list {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, v...)
		}
	}
	return string(b)
}

// ActivityGroup returns the activity summary grouped by dims for the current
// window and drill stack. Like summarize it serves a cache hit whatever ctx
// says and bails on a MISS under a cancelled context, so a superseded flight
// stops at this stage boundary instead of opening a join over both ledgers
// nobody will read.
func (d *Data) ActivityGroup(ctx context.Context, now time.Time, sp Span, crumbs []Crumb, dims []string) (*store.ActivitySummary, error) {
	f := d.activityFilterFor(now, sp, crumbs, dims)
	k := activityKey(f)
	d.mu.Lock()
	cache := d.act
	s, ok := cache.get(k)
	d.mu.Unlock()
	if ok {
		return s, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := d.src.SummarizeActivity(ctx, f)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	cache.put(k, s) // the instance captured before the query (see summarize)
	d.mu.Unlock()
	return s, nil
}

// ActivityRank returns the top `limit` activity buckets grouped by dims and
// ranked by one metric, ordering and capping in SQL. The metric and the cap are
// part of the cache key: two rankings of the same window by different metrics
// are different answers, and the cap decides which rows came back.
func (d *Data) ActivityRank(ctx context.Context, now time.Time, sp Span, crumbs []Crumb, dims []string, by store.ActivityOrder, limit int) ([]store.ActivityBucket, error) {
	f := d.activityFilterFor(now, sp, crumbs, dims)
	k := activityKey(f) + "|" + string(by) + "|" + strconv.Itoa(limit)
	d.mu.Lock()
	cache := d.rank
	rows, ok := cache.get(k)
	d.mu.Unlock()
	if ok {
		return rows, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := d.src.TopActivity(ctx, f, by, limit)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	cache.put(k, rows)
	d.mu.Unlock()
	return rows, nil
}

// turnContextKey derives a stable cache key for one dimension's slice of the
// turn-context table. The DIMENSION IS THE FIRST FIELD OF THE KEY, and it is not
// optional: every other component of the key is identical between two pivots of
// the same window, so a key that omitted it would hand a skill query's rows to
// an agent query and call it a cache hit.
func turnContextKey(dim model.TurnDimension, f store.ActivityFilter) string {
	return string(dim) + "|" + activityKey(f)
}

// TurnContextGroup returns one dimension's grouped turn-context summary for the
// current window and drill stack. Like summarize it serves a cache hit whatever
// ctx says and bails on a MISS under a cancelled context, so a superseded flight
// stops at this stage boundary rather than opening a join nobody will read.
func (d *Data) TurnContextGroup(ctx context.Context, now time.Time, sp Span, crumbs []Crumb, dim model.TurnDimension, dims []string) (*store.TurnContextSummary, error) {
	f := d.turnContextFilterFor(now, sp, crumbs, dims)
	k := turnContextKey(dim, f)
	d.mu.Lock()
	cache := d.tctx
	s, ok := cache.get(k)
	d.mu.Unlock()
	if ok {
		return s, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := d.src.SummarizeTurnContext(ctx, dim, f)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	cache.put(k, s) // the instance captured before the query (see summarize)
	d.mu.Unlock()
	return s, nil
}

// TurnContextRank returns the top `limit` buckets of one dimension, ranked by
// one metric, ordering and capping in SQL. The metric and the cap join the
// dimension in the key for the same reason they do in ActivityRank: two
// rankings of one window by different metrics are different answers, and the
// cap decides which rows came back.
func (d *Data) TurnContextRank(ctx context.Context, now time.Time, sp Span, crumbs []Crumb, dim model.TurnDimension, dims []string, by store.ActivityOrder, limit int) ([]store.TurnContextBucket, error) {
	f := d.turnContextFilterFor(now, sp, crumbs, dims)
	k := turnContextKey(dim, f) + "|" + string(by) + "|" + strconv.Itoa(limit)
	d.mu.Lock()
	cache := d.tcRank
	rows, ok := cache.get(k)
	d.mu.Unlock()
	if ok {
		return rows, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := d.src.TopTurnContext(ctx, dim, f, by, limit)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	cache.put(k, rows)
	d.mu.Unlock()
	return rows, nil
}

// turnContextFilterFor builds the filter for a turn-context query. It is
// activityFilterFor minus the two dimensions the table refuses — Kinds and
// Names select activity CALLS and the store errors on either — which the drill
// stack cannot produce anyway; applyActivityCrumb only ever sets tool, model,
// project and session, every one of which is a column on usage_turn_context.
func (d *Data) turnContextFilterFor(now time.Time, sp Span, crumbs []Crumb, groupBy []string) store.ActivityFilter {
	return d.activityFilterFor(now, sp, crumbs, groupBy)
}

// activityOrder maps the shared sort mode onto the activity ledger's ranking
// metric. "events" is calls (one activity row IS one invocation), "total" is
// attributed tokens and "cost" is attributed cost. "name" has no SQL ranking:
// it takes the call ranking and re-orders the returned page alphabetically, so
// the rows on screen are still the busiest ones, just listed by name.
func activityOrder(s Sort) store.ActivityOrder {
	switch s {
	case SortEvents:
		return store.ActivityByCalls
	case SortCost:
		return store.ActivityByCost
	case SortTotal:
		return store.ActivityByTokens
	default:
		return store.ActivityByCalls
	}
}

// activityOrderLabel names the metric a ranking is ordered by, for the panel
// title. It says what the list IS, which the sort chip alone does not: "name"
// ranks by calls and then sorts the page.
//
// The count metric is named by the PIVOT. store.ActivityByCalls is one order
// constant, but it ranks activity rows by calls and turn contexts by TURNS —
// there is no per-call row in that table to count — and a title reading "by
// calls" over a list of turn counts would misname every number under it.
func activityOrderLabel(s Sort, p ActivityPivot) string {
	switch s {
	case SortEvents:
		if p != PivotCalls {
			return "turns"
		}
		return "calls"
	case SortCost:
		return "cost"
	case SortTotal:
		return "tokens"
	default:
		return "name"
	}
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
	slices.SortStableFunc(b, func(x, y store.Bucket) int {
		return strings.Compare(x.Keys[dim], y.Keys[dim])
	})
}

// Timeline returns per-day (or per-hour for short ranges) buckets across the
// current range, ascending by time.
func (d *Data) Timeline(ctx context.Context, now time.Time, sp Span, crumbs []Crumb) (*store.Summary, string, error) {
	dim := timelineDim(sp.R)
	s, err := d.summarize(ctx, d.filterFor(now, sp, crumbs, []string{dim}))
	if err != nil {
		return nil, dim, err
	}
	s = copySummary(s)
	sortTimeline(s.Buckets, dim)
	return s, dim, nil
}

// TimelineCached is the cache-only twin of Timeline.
func (d *Data) TimelineCached(now time.Time, sp Span, crumbs []Crumb) (*store.Summary, string, bool) {
	dim := timelineDim(sp.R)
	s, ok := d.cachedSummary(d.filterFor(now, sp, crumbs, []string{dim}))
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
func (d *Data) WindowTotals(ctx context.Context, since, until time.Time, crumbs []Crumb) (store.Bucket, error) {
	s, err := d.summarize(ctx, windowTotalsFilter(since, until, crumbs))
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
//
// slices.SortStableFunc rather than sort.SliceStable: both are stable, so ties
// keep the store's order either way, but the reflect-based form allocated a
// Swapper and a boxed slice header per call — and a warm reload sorts four
// times (issue #41).
func sortBuckets(b []store.Bucket, dim string, srt Sort) {
	switch srt {
	case SortName:
		slices.SortStableFunc(b, func(x, y store.Bucket) int {
			return strings.Compare(x.Keys[dim], y.Keys[dim])
		})
	case SortEvents:
		slices.SortStableFunc(b, func(x, y store.Bucket) int {
			return cmp.Compare(y.Events, x.Events)
		})
	case SortCost:
		slices.SortStableFunc(b, func(x, y store.Bucket) int {
			return cmp.Compare(y.CostMicroUSD, x.CostMicroUSD)
		})
	default:
		slices.SortStableFunc(b, func(x, y store.Bucket) int {
			return cmp.Compare(y.Total, x.Total)
		})
	}
}
