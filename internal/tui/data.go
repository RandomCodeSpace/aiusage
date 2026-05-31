package tui

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"aiusage/internal/model"
	"aiusage/internal/store"
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
func (r Range) Window(now time.Time) (since, until time.Time) {
	switch r {
	case RangeToday:
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), time.Time{}
	case Range7d:
		return now.AddDate(0, 0, -7), time.Time{}
	case Range30d:
		return now.AddDate(0, 0, -30), time.Time{}
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
// interface (not *store.SQLite) so tests can substitute a fake.
type DataSource interface {
	Summarize(ctx context.Context, f store.Filter) (*store.Summary, error)
	ListEvents(ctx context.Context, f store.Filter) ([]model.UsageEvent, error)
}

// compile-time guarantee that a *store.Store satisfies DataSource.
var _ DataSource = (store.Store)(nil)

// Data wraps a DataSource with a tiny one-entry-per-key cache keyed on the
// resolved query so repeated renders within a frame avoid re-hitting SQLite.
//
// The two cache maps are guarded by mu: a background load tea.Cmd warms the
// cache off the UI thread (running the same queries reload() will), so reads
// from Update/View and writes from the load goroutine must not race. The load
// path is also serialised by an in-flight flag in the model, so at most one
// goroutine writes at a time; mu makes that contract enforced rather than
// assumed.
type Data struct {
	src   DataSource
	now   func() time.Time
	mu    sync.Mutex
	cache map[string]*store.Summary
	evCab map[string][]model.UsageEvent
}

// NewData builds a Data over src.
func NewData(src DataSource) *Data {
	return &Data{
		src:   src,
		now:   time.Now,
		cache: map[string]*store.Summary{},
		evCab: map[string][]model.UsageEvent{},
	}
}

// Invalidate clears the cache (used on refresh).
func (d *Data) Invalidate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache = map[string]*store.Summary{}
	d.evCab = map[string][]model.UsageEvent{}
}

// filterFor builds a store.Filter from a range, drill stack and group-by dims.
func (d *Data) filterFor(r Range, crumbs []Crumb, groupBy []string) store.Filter {
	since, until := r.Window(d.now())
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

// summarize runs a cached Summarize.
func (d *Data) summarize(f store.Filter) (*store.Summary, error) {
	k := cacheKey(f)
	d.mu.Lock()
	s, ok := d.cache[k]
	d.mu.Unlock()
	if ok {
		return s, nil
	}
	s, err := d.src.Summarize(context.Background(), f)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.cache[k] = s
	d.mu.Unlock()
	return s, nil
}

// Totals returns the grand-total bucket for the current range and drill stack.
func (d *Data) Totals(r Range, crumbs []Crumb) (store.Bucket, error) {
	s, err := d.summarize(d.filterFor(r, crumbs, nil))
	if err != nil {
		return store.Bucket{}, err
	}
	return s.Totals, nil
}

// GroupBy returns the summary grouped by a single dimension under the current
// range and drill stack, sorted per the sort mode.
func (d *Data) GroupBy(r Range, crumbs []Crumb, dim string, srt Sort) (*store.Summary, error) {
	s, err := d.summarize(d.filterFor(r, crumbs, []string{dim}))
	if err != nil {
		return nil, err
	}
	sortBuckets(s.Buckets, dim, srt)
	return s, nil
}

// DrillDim returns the grouping dimension for a given drill depth (0-based).
func DrillDim(depth int) (string, bool) {
	if depth < 0 || depth >= len(drillDims) {
		return "", false
	}
	return drillDims[depth], true
}

// Timeline returns per-day (or per-hour for short ranges) buckets across the
// current range, ascending by time.
func (d *Data) Timeline(r Range, crumbs []Crumb) (*store.Summary, string, error) {
	dim := "day"
	if r == RangeToday {
		dim = "hour"
	}
	s, err := d.summarize(d.filterFor(r, crumbs, []string{dim}))
	if err != nil {
		return nil, dim, err
	}
	sort.SliceStable(s.Buckets, func(i, j int) bool {
		return s.Buckets[i].Keys[dim] < s.Buckets[j].Keys[dim]
	})
	return s, dim, nil
}

// GroupByWindow returns the summary grouped by dim, restricted to an explicit
// [since,until) window (used by the scrub crosshair to re-price the side panels
// for a single bucket). It reuses the same query cache as the rest of the layer.
func (d *Data) GroupByWindow(since, until time.Time, crumbs []Crumb, dim string, srt Sort) (*store.Summary, error) {
	f := store.Filter{Since: since, Until: until, GroupBy: []string{dim}}
	for _, c := range crumbs {
		applyCrumb(&f, c)
	}
	s, err := d.summarize(f)
	if err != nil {
		return nil, err
	}
	sortBuckets(s.Buckets, dim, srt)
	return s, nil
}

// Events returns raw events for the current range and drill stack (cached).
func (d *Data) Events(r Range, crumbs []Crumb) ([]model.UsageEvent, error) {
	f := d.filterFor(r, crumbs, nil)
	k := cacheKey(f) + "#ev"
	d.mu.Lock()
	e, ok := d.evCab[k]
	d.mu.Unlock()
	if ok {
		return e, nil
	}
	e, err := d.src.ListEvents(context.Background(), f)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.evCab[k] = e
	d.mu.Unlock()
	return e, nil
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
