package tui

import (
	"context"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
)

// reload re-queries the store for whatever the active view needs and rebuilds
// that view's data struct, including the QUERYING detail leg for the current
// selection. Background flights (loadCmd) and direct test calls use it. After
// loading, pane focus is re-applied so the ring lands.
func (m *Model) reload() {
	m.reloadWith(false)
}

// applyReload is the UI-thread twin used by handleDataLoaded: the base queries
// replay the flight's keys against the warm cache, but the detail leg runs the
// cache-only sync twins — a selection moved mid-flight must arm detailWanted
// (converted to a debounced background load by scheduleDetail), never run a
// synchronous store query at apply time (issue #4 residual).
func (m *Model) applyReload() {
	m.reloadWith(true)
}

func (m *Model) reloadWith(cacheOnlyDetail bool) {
	m.err = nil
	switch m.view {
	case ViewOverview:
		m.loadOverview() // its scrub reprice (syncScrub) is cache-only already
	case ViewByTool:
		m.loadByToolBase()
		if m.err == nil {
			if cacheOnlyDetail {
				m.syncByToolDetail()
			} else {
				m.loadByToolDetail()
			}
		}
	case ViewByModel:
		m.loadByModelBase()
		if m.err == nil {
			if cacheOnlyDetail {
				m.syncByModelDetail()
			} else {
				m.loadByModelDetail()
			}
		}
	case ViewBrowse:
		m.loadBrowseBase()
		if m.err == nil {
			if cacheOnlyDetail {
				m.syncBrowsePreview()
			} else {
				m.loadBrowsePreview()
			}
		}
	}
	m.applyPaneFocus()
}

// loadOverview populates the KPI strip, hero timeline series and side by-tool
// bars. When the scrub crosshair is pinned, the KPI tiles + side bars re-price
// to the scrubbed bucket via syncScrub (called after the base load).
func (m *Model) loadOverview() {
	now := m.qnow()
	tot, err := m.data.Totals(m.qctx(), now, m.span(), m.crumbs)
	if err != nil {
		m.err = err
		return
	}
	byTool, err := m.data.GroupBy(m.qctx(), now, m.span(), m.crumbs, "tool", SortTotal)
	if err != nil {
		m.err = err
		return
	}
	tl, dim, err := m.data.Timeline(m.qctx(), now, m.span(), m.crumbs)
	if err != nil {
		m.err = err
		return
	}
	m.tlData.Buckets = tl.Buckets
	m.tlData.Dim = dim
	m.clampScrub()

	// Prewarm every scrub position in ONE [dim, tool] grouped query: the
	// per-bucket by-tool compositions land in m.scrubComp, so a scrub sweep is
	// fully local — no windowed per-bucket queries, warm or cold.
	comp, err := m.data.GroupByDims(m.qctx(), now, m.span(), m.crumbs, []string{dim, "tool"})
	if err != nil {
		m.err = err
		return
	}
	m.scrubComp = buildScrubComp(tl.Buckets, comp.Buckets, dim)

	m.overview = views.OverviewData{
		Totals:      tot,
		Prev:        m.prevTotals(),
		ByTool:      filterBuckets(byTool.Buckets, "tool", m.filter),
		Timeline:    tl.Buckets,
		TimelineDim: dim,
		RangeLbl:    m.spanLabel(),
		ActivePane:  views.PaneOverviewHero,
		Cursor:      m.scrubIndex,
		Pinned:      m.scrubPinned,
	}
	if m.scrubPinned {
		m.syncScrub()
	}
}

// loadByToolBase builds the by-tool bars (no detail leg — reloadWith picks the
// querying or cache-only detail twin).
func (m *Model) loadByToolBase() {
	s, err := m.data.GroupBy(m.qctx(), m.qnow(), m.span(), m.crumbs, "tool", m.sort)
	if err != nil {
		m.err = err
		return
	}
	rows := filterBuckets(s.Buckets, "tool", m.filter)
	m.byTool.Rows = rows
	m.byTool.Grand = grandOf(m.qctx(), m.data, m.qnow(), m.span(), m.crumbs, rows)
	m.byTool.RangeLbl = m.spanLabel()
	m.byTool.ActivePane = views.PaneByXBars
	m.byTool.Copilot = m.copilotState(rows)
	if m.byTool.Selected >= len(rows) {
		m.byTool.Selected = 0
	}
}

// loadByToolDetail loads the selected tool's daily trend, querying the store —
// background flights and warm apply-side reloads only. The distinct-session
// count comes straight off the selected row (store-level COUNT DISTINCT), so
// no per-session bucket materialization happens anywhere.
func (m *Model) loadByToolDetail() {
	b, ok := m.selectedByToolBucket()
	if !ok {
		m.byTool.SelTrend = nil
		m.byTool.SelTrendErr = false
		m.byTool.SelSessions = 0
		return
	}
	m.byTool.SelSessions = b.Sessions
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: "tool", Value: b.Keys["tool"]})
	trend, _, err := m.data.Timeline(m.qctx(), m.qnow(), m.span(), crumbs)
	if err != nil {
		// Honest per-pane failure: the detail renders "query failed", never an
		// ambiguous blank (or a silently held stale trend).
		m.byTool.SelTrend = nil
		m.byTool.SelTrendErr = true
		return
	}
	m.byTool.SelTrend = trend.Buckets
	m.byTool.SelTrendErr = false
}

// syncByToolDetail is the cache-only twin of loadByToolDetail for the UI
// thread: selection moves reprice from cache; a miss keeps the previous trend
// on screen and requests a debounced background load (detail.go).
func (m *Model) syncByToolDetail() {
	b, ok := m.selectedByToolBucket()
	if !ok {
		m.byTool.SelTrend = nil
		m.byTool.SelTrendErr = false
		m.byTool.SelSessions = 0
		return
	}
	m.byTool.SelSessions = b.Sessions
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: "tool", Value: b.Keys["tool"]})
	if trend, _, ok := m.data.TimelineCached(m.qnow(), m.span(), crumbs); ok {
		m.byTool.SelTrend = trend.Buckets
		m.byTool.SelTrendErr = false
	} else {
		m.detailWanted = true
	}
}

// loadByModelBase builds the by-model bars colored by owning tool (no detail
// leg — reloadWith picks the querying or cache-only detail twin).
func (m *Model) loadByModelBase() {
	s, err := m.data.GroupBy(m.qctx(), m.qnow(), m.span(), m.crumbs, "model", m.sort)
	if err != nil {
		m.err = err
		return
	}
	rows := filterBuckets(s.Buckets, "model", m.filter)
	m.byModel.Rows = rows
	m.byModel.Grand = grandOf(m.qctx(), m.data, m.qnow(), m.span(), m.crumbs, rows)
	m.byModel.RangeLbl = m.spanLabel()
	m.byModel.ActivePane = views.PaneByXBars
	m.byModel.ModelTool = m.modelOwners()
	if m.byModel.Selected >= len(rows) {
		m.byModel.Selected = 0
	}
}

// loadByModelDetail loads the selected model's daily trend (querying;
// background flights and warm apply-side reloads only).
func (m *Model) loadByModelDetail() {
	b, ok := m.selectedByModelBucket()
	if !ok {
		m.byModel.SelTrend = nil
		m.byModel.SelTrendErr = false
		return
	}
	mdl := b.Keys["model"]
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: "model", Value: mdl})
	trend, _, err := m.data.Timeline(m.qctx(), m.qnow(), m.span(), crumbs)
	if err != nil {
		m.byModel.SelTrend = nil
		m.byModel.SelTrendErr = true
		return
	}
	m.byModel.SelTrend = trend.Buckets
	m.byModel.SelTrendErr = false
}

// syncByModelDetail is the cache-only twin of loadByModelDetail (see
// syncByToolDetail).
func (m *Model) syncByModelDetail() {
	b, ok := m.selectedByModelBucket()
	if !ok {
		m.byModel.SelTrend = nil
		m.byModel.SelTrendErr = false
		return
	}
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: "model", Value: b.Keys["model"]})
	if trend, _, ok := m.data.TimelineCached(m.qnow(), m.span(), crumbs); ok {
		m.byModel.SelTrend = trend.Buckets
		m.byModel.SelTrendErr = false
	} else {
		m.detailWanted = true
	}
}

// loadBrowseBase builds the drill list at the current depth (no preview leg —
// reloadWith picks the querying or cache-only preview twin).
func (m *Model) loadBrowseBase() {
	dim, ok := DrillDim(len(m.crumbs))
	if !ok {
		dim = drillDims[len(drillDims)-1]
	}
	s, err := m.data.GroupBy(m.qctx(), m.qnow(), m.span(), m.crumbs, dim, m.sort)
	if err != nil {
		m.err = err
		return
	}
	rows := filterBuckets(s.Buckets, dim, m.filter)
	m.browse.SetData(m.vctx, dim, rows, grandOf(m.qctx(), m.data, m.qnow(), m.span(), m.crumbs, rows))
	m.layout()
}

// loadBrowsePreview loads the selected Browse row's daily trend into the
// preview pane, querying the store (background flights and warm apply-side
// reloads only).
func (m *Model) loadBrowsePreview() {
	if m.view != ViewBrowse {
		return
	}
	dim := m.browse.Dim()
	val, ok := m.browse.SelectedValue()
	if !ok {
		m.browse.SetPreview(nil)
		m.browse.SetPreviewErr(false)
		return
	}
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: dim, Value: val})
	trend, _, err := m.data.Timeline(m.qctx(), m.qnow(), m.span(), crumbs)
	if err != nil {
		m.browse.SetPreview(nil)
		m.browse.SetPreviewErr(true)
		return
	}
	m.browse.SetPreview(trend.Buckets)
	m.browse.SetPreviewErr(false)
}

// syncBrowsePreview is the cache-only twin of loadBrowsePreview for UI-thread
// cursor moves: a miss keeps the previous preview and requests a debounced
// background load (detail.go).
func (m *Model) syncBrowsePreview() {
	if m.view != ViewBrowse {
		return
	}
	dim := m.browse.Dim()
	val, ok := m.browse.SelectedValue()
	if !ok {
		m.browse.SetPreview(nil)
		m.browse.SetPreviewErr(false)
		return
	}
	crumbs := append(cloneCrumbs(m.crumbs), Crumb{Dim: dim, Value: val})
	if trend, _, ok := m.data.TimelineCached(m.qnow(), m.span(), crumbs); ok {
		m.browse.SetPreview(trend.Buckets)
		m.browse.SetPreviewErr(false)
	} else {
		m.detailWanted = true
	}
}

// syncScrub re-prices the Overview KPI tiles + side bars to the scrubbed bucket
// (or back to full-range when unpinned). Also updates the timeline
// cursor/readout state. It NEVER queries: pinned repricing projects the KPI
// totals from the timeline bucket and reads the side bars from the prewarmed
// per-bucket composition (scrubComp), so a scrub sweep of any length touches
// neither the store nor the cache. The unpinned spring-back reads full-range
// summaries from cache only; a miss (possible only between an Invalidate and
// its flight landing) keeps the previous numbers and requests a debounced
// reprice (detail.go).
func (m *Model) syncScrub() {
	n := len(m.tlData.Buckets)
	m.tlData.Cursor = m.scrubIndex
	m.tlData.Pinned = m.scrubPinned
	m.tlData.TopTool = m.topToolAt(m.scrubIndex)
	m.tlData.Focused = true

	if m.view != ViewOverview {
		return
	}
	m.overview.Cursor = m.scrubIndex
	m.overview.Pinned = m.scrubPinned
	if !m.scrubPinned || n == 0 || m.scrubIndex < 0 || m.scrubIndex >= n {
		// Spring back to full-range totals (and hide the crosshair even if a stale
		// pin survived a timeline shrink — the KPIs below show full-range data).
		m.overview.Pinned = false
		m.overview.ScrubLabel = ""
		now := m.qnow()
		tot, okT := m.data.TotalsCached(now, m.span(), m.crumbs)
		prev, okP := m.prevTotalsCached()
		byTool, okB := m.data.GroupByCached(now, m.span(), m.crumbs, "tool", SortTotal)
		if !okT || !okP || !okB {
			m.detailWanted = true
			return
		}
		m.overview.Totals = tot
		m.overview.Prev = prev
		m.overview.ByTool = filterBuckets(byTool.Buckets, "tool", m.filter)
		return
	}

	b := m.tlData.Buckets[m.scrubIndex]
	m.overview.Totals = bucketTotalsFromBucket(b)
	m.overview.ScrubLabel = views.BucketTimestamp(b, m.tlData.Dim)
	if m.scrubIndex > 0 {
		m.overview.Prev = bucketTotalsFromBucket(m.tlData.Buckets[m.scrubIndex-1])
	} else {
		m.overview.Prev = store.Bucket{}
	}
	if m.scrubIndex < len(m.scrubComp) {
		m.overview.ByTool = filterBuckets(m.scrubComp[m.scrubIndex], "tool", m.filter)
	} else {
		// Composition misaligned with the timeline (defensive; a load rebuilds
		// both together) — keep the previous bars, reprice when the flight lands.
		m.detailWanted = true
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

// prevWindow resolves the previous-period comparison window for delta chips.
// ok=false for open-ended ranges (all), which have no prior period.
//
// Windows derive from the load-generation clock (qnow), quantized to bucket
// granularity so their cache keys stay stable: 7d/30d compare against the prior
// N local calendar days (AddDate keeps local midnights across DST); today
// compares against the same-length tail of yesterday, with "now" rounded down
// to the hour so the key moves once per hour, not per second. A STEPPED window
// is already a whole closed calendar span, so its comparison period is simply
// the span before it — including for today, where the partial-day tail logic
// would understate a full past day.
func (m *Model) prevWindow() (since, until time.Time, ok bool) {
	now := m.qnow()
	sp := m.span()
	cur, _ := sp.Window(now)
	if cur.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	if sp.Step < 0 {
		prev := Span{R: sp.R, Step: sp.Step - 1}
		ps, pu := prev.Window(now)
		return ps, pu, true
	}
	switch m.rng {
	case Range7d:
		return cur.AddDate(0, 0, -7), cur, true
	case Range30d:
		return cur.AddDate(0, 0, -30), cur, true
	default: // RangeToday
		y, mo, d := now.Date()
		end := time.Date(y, mo, d, now.Hour(), 0, 0, 0, now.Location())
		return cur.Add(-end.Sub(cur)), cur, true
	}
}

// prevTotals returns the grand total for the immediately-prior equal-length
// period (querying; background flights and warm apply-side reloads only). It
// reads the store's own Totals aggregate off one ungrouped Summarize — no
// in-memory summing of grouped rows.
func (m *Model) prevTotals() store.Bucket {
	since, until, ok := m.prevWindow()
	if !ok {
		return store.Bucket{}
	}
	b, err := m.data.WindowTotals(m.qctx(), since, until, m.crumbs)
	if err != nil {
		return store.Bucket{}
	}
	return b
}

// prevTotalsCached is the cache-only twin of prevTotals. Open-ended ranges
// report ok=true with a zero bucket — no prior period exists, so there is
// nothing to query.
func (m *Model) prevTotalsCached() (store.Bucket, bool) {
	since, until, ok := m.prevWindow()
	if !ok {
		return store.Bucket{}, true
	}
	return m.data.WindowTotalsCached(since, until, m.crumbs)
}

// buildScrubComp reduces one [dim, tool] grouped summary into per-timeline-
// bucket by-tool compositions, index-aligned with the timeline. Membership is
// exact by construction: the composition rows carry the same strftime bucket
// keys as the timeline itself, so no window arithmetic (and none of its
// week/year edge cases) is involved.
func buildScrubComp(timeline, comp []store.Bucket, dim string) [][]store.Bucket {
	byKey := make(map[string][]store.Bucket, len(timeline))
	for _, b := range comp {
		k := b.Keys[dim]
		byKey[k] = append(byKey[k], b)
	}
	out := make([][]store.Bucket, len(timeline))
	for i, tb := range timeline {
		rows := byKey[tb.Keys[dim]]
		sortBuckets(rows, "tool", SortTotal)
		out[i] = rows
	}
	return out
}

// topToolAt returns the dominant tool at a timeline bucket index from the
// prewarmed composition — no store access.
func (m *Model) topToolAt(idx int) string {
	if idx < 0 || idx >= len(m.scrubComp) || len(m.scrubComp[idx]) == 0 {
		return ""
	}
	return m.scrubComp[idx][0].Keys["tool"]
}

// modelOwners maps each model id to its dominant owning tool, reduced in
// memory from ONE Summarize grouped by [model, tool] (replaces the per-model
// N+1). Ties keep the store's tool order (lexicographic), matching the old
// stable-sorted first-bucket pick.
func (m *Model) modelOwners() map[string]string {
	out := map[string]string{}
	s, err := m.data.GroupByDims(m.qctx(), m.qnow(), m.span(), m.crumbs, []string{"model", "tool"})
	if err != nil {
		return out
	}
	best := map[string]int64{}
	for _, b := range s.Buckets {
		mdl := b.Keys["model"]
		if top, seen := best[mdl]; !seen || b.Total > top {
			best[mdl] = b.Total
			out[mdl] = b.Keys["tool"]
		}
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
func grandOf(ctx context.Context, d *Data, now time.Time, sp Span, crumbs []Crumb, rows []store.Bucket) int64 {
	var sum int64
	for _, b := range rows {
		sum += b.Total
	}
	tot, err := d.Totals(ctx, now, sp, crumbs)
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

// copilotState resolves the By-Tool copilot footnote state. Whether a data
// SOURCE exists comes from startup adapter discovery (Options.Sources — the
// signal `doctor` prints), never from the loaded rows: a range with no copilot
// usage is not evidence that copilot is unconfigured (issue #44). The rows only
// decide the second question, whether that source produced anything in the
// range currently on screen.
func (m Model) copilotState(rows []store.Bucket) views.CopilotSourceState {
	n, known := m.sources[model.ToolCopilot]
	if !known {
		return views.CopilotUnknown
	}
	if n == 0 {
		return views.CopilotNoSource
	}
	for _, b := range rows {
		if b.Keys["tool"] == model.ToolCopilot && b.Total > 0 {
			return views.CopilotActive
		}
	}
	return views.CopilotIdle
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
