package tui

import (
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// reload re-queries the store for whatever the active view needs and rebuilds
// that view's data struct. After loading, pane focus is re-applied so the ring
// lands.
func (m *Model) reload() {
	m.err = nil
	switch m.view {
	case ViewOverview:
		m.loadOverview()
	case ViewByTool:
		m.loadByTool()
	case ViewByModel:
		m.loadByModel()
	case ViewBrowse:
		m.loadBrowse()
	}
	m.applyPaneFocus()
}

// loadOverview populates the KPI strip, hero timeline series and side by-tool
// bars. When the scrub crosshair is pinned, the KPI tiles + side bars re-price
// to the scrubbed bucket via syncScrub (called after the base load).
func (m *Model) loadOverview() {
	tot, err := m.data.Totals(m.rng, m.crumbs)
	if err != nil {
		m.err = err
		return
	}
	byTool, err := m.data.GroupBy(m.rng, m.crumbs, "tool", SortTotal)
	if err != nil {
		m.err = err
		return
	}
	tl, dim, err := m.data.Timeline(m.rng, m.crumbs)
	if err != nil {
		m.err = err
		return
	}
	m.tlData.Buckets = tl.Buckets
	m.tlData.Dim = dim
	m.clampScrub()

	m.overview = views.OverviewData{
		Totals:      tot,
		Prev:        m.prevTotals(),
		ByTool:      filterBuckets(byTool.Buckets, "tool", m.filter),
		Timeline:    tl.Buckets,
		TimelineDim: dim,
		RangeLbl:    m.rng.Label(),
		ActivePane:  views.PaneOverviewHero,
	}
	if m.scrubPinned {
		m.syncScrub()
	}
}

// loadByTool builds the by-tool bars + selected-tool detail.
func (m *Model) loadByTool() {
	s, err := m.data.GroupBy(m.rng, m.crumbs, "tool", m.sort)
	if err != nil {
		m.err = err
		return
	}
	rows := filterBuckets(s.Buckets, "tool", m.filter)
	m.byTool.Rows = rows
	m.byTool.Grand = grandOf(m.data, m.rng, m.crumbs, rows)
	m.byTool.RangeLbl = m.rng.Label()
	m.byTool.ActivePane = views.PaneByXBars
	m.byTool.CopilotAbsent = copilotAbsent(rows)
	if m.byTool.Selected >= len(rows) {
		m.byTool.Selected = 0
	}
	m.loadByToolDetail()
}

// loadByToolDetail loads the selected tool's daily trend + distinct sessions.
func (m *Model) loadByToolDetail() {
	b, ok := m.selectedByToolBucket()
	if !ok {
		m.byTool.SelTrend = nil
		m.byTool.SelSessions = 0
		return
	}
	tool := b.Keys["tool"]
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: "tool", Value: tool})
	trend, _, err := m.data.Timeline(m.rng, crumbs)
	if err == nil {
		m.byTool.SelTrend = trend.Buckets
	}
	m.byTool.SelSessions = m.distinctSessions(crumbs)
}

// loadByModel builds the by-model bars (colored by owning tool) + detail.
func (m *Model) loadByModel() {
	s, err := m.data.GroupBy(m.rng, m.crumbs, "model", m.sort)
	if err != nil {
		m.err = err
		return
	}
	rows := filterBuckets(s.Buckets, "model", m.filter)
	m.byModel.Rows = rows
	m.byModel.Grand = grandOf(m.data, m.rng, m.crumbs, rows)
	m.byModel.RangeLbl = m.rng.Label()
	m.byModel.ActivePane = views.PaneByXBars
	m.byModel.ModelTool = m.modelOwners()
	if m.byModel.Selected >= len(rows) {
		m.byModel.Selected = 0
	}
	m.loadByModelDetail()
}

// loadByModelDetail loads the selected model's daily trend.
func (m *Model) loadByModelDetail() {
	b, ok := m.selectedByModelBucket()
	if !ok {
		m.byModel.SelTrend = nil
		return
	}
	mdl := b.Keys["model"]
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: "model", Value: mdl})
	trend, _, err := m.data.Timeline(m.rng, crumbs)
	if err == nil {
		m.byModel.SelTrend = trend.Buckets
	}
}

// loadBrowse builds the drill list at the current depth + preview trend.
func (m *Model) loadBrowse() {
	dim, ok := DrillDim(len(m.crumbs))
	if !ok {
		dim = drillDims[len(drillDims)-1]
	}
	s, err := m.data.GroupBy(m.rng, m.crumbs, dim, m.sort)
	if err != nil {
		m.err = err
		return
	}
	rows := filterBuckets(s.Buckets, dim, m.filter)
	m.browse.SetData(m.vctx, dim, rows, grandOf(m.data, m.rng, m.crumbs, rows))
	m.layout()
	m.syncBrowsePreview()
}

// syncBrowsePreview loads the selected Browse row's daily trend into the
// preview pane.
func (m *Model) syncBrowsePreview() {
	if m.view != ViewBrowse {
		return
	}
	dim := m.browse.Dim()
	val, ok := m.browse.SelectedValue()
	if !ok {
		m.browse.SetPreview(nil)
		return
	}
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: dim, Value: val})
	trend, _, err := m.data.Timeline(m.rng, crumbs)
	if err == nil {
		m.browse.SetPreview(trend.Buckets)
	}
}

// syncScrub re-prices the Overview KPI tiles + side bars to the scrubbed bucket
// (or back to full-range when unpinned), via a windowed Summarize that reuses
// the data cache. Also updates the timeline cursor/readout state.
func (m *Model) syncScrub() {
	n := len(m.tlData.Buckets)
	m.tlData.Cursor = m.scrubIndex
	m.tlData.Pinned = m.scrubPinned
	m.tlData.TopTool = m.topToolAt(m.scrubIndex)
	m.tlData.Focused = true

	if m.view != ViewOverview {
		return
	}
	if !m.scrubPinned || n == 0 || m.scrubIndex < 0 || m.scrubIndex >= n {
		// Spring back to full-range totals.
		tot, _ := m.data.Totals(m.rng, m.crumbs)
		m.overview.Totals = tot
		m.overview.Prev = m.prevTotals()
		m.overview.ScrubLabel = ""
		if byTool, err := m.data.GroupBy(m.rng, m.crumbs, "tool", SortTotal); err == nil {
			m.overview.ByTool = filterBuckets(byTool.Buckets, "tool", m.filter)
		}
		return
	}

	b := m.tlData.Buckets[m.scrubIndex]
	since, until := m.bucketWindow(b)
	m.overview.Totals = bucketTotalsFromBucket(b)
	m.overview.ScrubLabel = views.BucketTimestamp(b, m.tlData.Dim)
	if m.scrubIndex > 0 {
		m.overview.Prev = bucketTotalsFromBucket(m.tlData.Buckets[m.scrubIndex-1])
	} else {
		m.overview.Prev = store.Bucket{}
	}
	if s, err := m.data.GroupByWindow(since, until, m.crumbs, "tool", SortTotal); err == nil {
		m.overview.ByTool = filterBuckets(s.Buckets, "tool", m.filter)
	}
}

// clampScrub keeps the scrub index within the current timeline bounds.
func (m *Model) clampScrub() {
	n := len(m.tlData.Buckets)
	if n == 0 {
		m.scrubIndex = 0
		return
	}
	if m.scrubIndex >= n {
		m.scrubIndex = n - 1
	}
	if m.scrubIndex < 0 {
		m.scrubIndex = 0
	}
}

// prevTotals returns the grand total for the immediately-prior equal-length
// period, for delta chips. Open-ended ranges (all) have no prior period. The
// store treats a zero Until as "now", so we resolve it to now() to size the
// prior window.
func (m *Model) prevTotals() store.Bucket {
	now := time.Now()
	since, until := m.rng.Window(now)
	if since.IsZero() {
		return store.Bucket{}
	}
	if until.IsZero() {
		until = now
	}
	span := until.Sub(since)
	prevSince := since.Add(-span)
	s, err := m.data.GroupByWindow(prevSince, since, m.crumbs, "tool", SortTotal)
	if err != nil {
		return store.Bucket{}
	}
	var b store.Bucket
	for _, x := range s.Buckets {
		b.Events += x.Events
		b.Input += x.Input
		b.Output += x.Output
		b.Reasoning += x.Reasoning
		b.CacheCreation += x.CacheCreation
		b.CacheRead += x.CacheRead
		b.Total += x.Total
	}
	return b
}

// bucketWindow resolves a timeline bucket to a [since,until) window for windowed
// re-pricing during scrub.
func (m *Model) bucketWindow(b store.Bucket) (since, until time.Time) {
	t, ok := views.ParseBucketTime(b.Keys[m.tlData.Dim], m.tlData.Dim)
	if !ok {
		return time.Time{}, time.Time{}
	}
	switch m.tlData.Dim {
	case "hour":
		return t, t.Add(time.Hour)
	case "week":
		return t, t.AddDate(0, 0, 7)
	case "month":
		return t, t.AddDate(0, 1, 0)
	default: // day
		return t, t.AddDate(0, 0, 1)
	}
}

// topToolAt returns the dominant tool at a timeline bucket index, querying the
// windowed by-tool composition (cached).
func (m *Model) topToolAt(idx int) string {
	if idx < 0 || idx >= len(m.tlData.Buckets) {
		return ""
	}
	since, until := m.bucketWindow(m.tlData.Buckets[idx])
	if since.IsZero() {
		return ""
	}
	s, err := m.data.GroupByWindow(since, until, m.crumbs, "tool", SortTotal)
	if err != nil || len(s.Buckets) == 0 {
		return ""
	}
	return s.Buckets[0].Keys["tool"]
}

// distinctSessions counts distinct sessions matching the crumbs by grouping on
// session under the current range.
func (m *Model) distinctSessions(crumbs []Crumb) int64 {
	s, err := m.data.GroupBy(m.rng, crumbs, "session", SortTotal)
	if err != nil {
		return 0
	}
	return int64(len(s.Buckets))
}

// modelOwners maps each model id to its dominant owning tool via a single
// model×tool grouping.
func (m *Model) modelOwners() map[string]string {
	out := map[string]string{}
	s, err := m.data.GroupBy(m.rng, m.crumbs, "model", SortTotal)
	if err != nil {
		return out
	}
	// For each model, find the owning tool by a windowed model+tool query is
	// heavy; instead group by tool within each model's crumb. Cheap enough for a
	// handful of models.
	for _, b := range s.Buckets {
		mdl := b.Keys["model"]
		crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: "model", Value: mdl})
		bt, err := m.data.GroupBy(m.rng, crumbs, "tool", SortTotal)
		if err != nil || len(bt.Buckets) == 0 {
			continue
		}
		out[mdl] = bt.Buckets[0].Keys["tool"]
	}
	return out
}

// bucketTotalsFromBucket projects a timeline bucket into a grand-total bucket
// shape for KPI re-pricing.
func bucketTotalsFromBucket(b store.Bucket) store.Bucket {
	return store.Bucket{
		Events:        b.Events,
		Input:         b.Input,
		Output:        b.Output,
		Reasoning:     b.Reasoning,
		CacheCreation: b.CacheCreation,
		CacheRead:     b.CacheRead,
		Total:         b.Total,
	}
}

// grandOf returns the denominator for share %: the larger of the store's
// grand-total bucket and the sum of the visible rows. Taking the max keeps
// share ≤ 100% even when filtering hides rows or a provider's grand total
// double-counts differently from the per-group totals.
func grandOf(d *Data, r Range, crumbs []Crumb, rows []store.Bucket) int64 {
	var sum int64
	for _, b := range rows {
		sum += b.Total
	}
	tot, err := d.Totals(r, crumbs)
	if err != nil {
		return sum
	}
	if tot.Total > sum {
		return tot.Total
	}
	return sum
}

// cloneCrumbs returns a copy so appending a transient crumb never mutates the
// model's drill stack.
func cloneCrumbs(c []Crumb) []Crumb {
	out := make([]Crumb, len(c))
	copy(out, c)
	return out
}

// copilotAbsent reports whether copilot has zero recorded usage among the rows
// (triggers the OTEL note). Returns false if copilot has any total.
func copilotAbsent(rows []store.Bucket) bool {
	for _, b := range rows {
		if b.Keys["tool"] == model.ToolCopilot && b.Total > 0 {
			return false
		}
	}
	return true
}

// filterBuckets keeps buckets whose dim value contains the (case-insensitive)
// filter substring. An empty filter is a no-op.
func filterBuckets(b []store.Bucket, dim, filter string) []store.Bucket {
	if filter == "" {
		return b
	}
	lf := strings.ToLower(filter)
	out := make([]store.Bucket, 0, len(b))
	for _, x := range b {
		if strings.Contains(strings.ToLower(x.Keys[dim]), lf) {
			out = append(out, x)
		}
	}
	return out
}
