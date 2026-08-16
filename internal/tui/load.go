package tui

import (
	"context"
	"slices"
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
	case ViewActivity:
		// No detail twin: the Activity detail card projects the selected row, so
		// there is nothing for a selection move to load (see setSelection).
		m.loadActivity()
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
		Timeline:    tl.Buckets,
		TimelineDim: dim,
		RangeLbl:    m.spanLabel(),
		ActivePane:  views.PaneOverviewHero,
		Cursor:      m.scrubIndex,
		Pinned:      m.scrubPinned,
	}
	m.setOverviewTools(filterBuckets(byTool.Buckets, "tool", m.filter), tot.Total)
	if m.scrubPinned {
		m.syncScrub()
	}
}

// setOverviewTools installs the Overview side card's by-tool rows with the long
// tail folded against the same threshold the By-Tool tab uses. grand is the
// denominator the card is a composition OF — the full-range total normally, and
// the scrubbed bucket's total while the crosshair is pinned — so a tool that is
// minor over a week but dominated one hour surfaces when you scrub to that hour.
func (m *Model) setOverviewTools(rows []store.Bucket, grand int64) {
	f := foldMinorTools(rows, grand, false)
	m.overview.ByTool = f.Rows
	m.overview.ByToolFold = f.Index
	m.overview.ByToolFoldCount = f.Count
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
	// The unfolded list is retained so expanding the fold is a pure re-render:
	// the toggle changes what is SHOWN, not what was asked for, and dispatching
	// a load for it would put a store query behind a keypress that has no new
	// question to ask.
	m.byToolAll = rows
	// A load always lands collapsed. The fold is a reading aid for the list in
	// front of you, and the list is replaced by every load — carrying an
	// expansion across a range change would leave it open over a different
	// window's tail, which is a different set of tools.
	m.byTool.FoldOpen = false
	m.byTool.Grand = grandOf(m.qctx(), m.data, m.qnow(), m.span(), m.crumbs, rows)
	m.applyByToolFold()
	m.byTool.RangeLbl = m.spanLabel()
	m.byTool.ActivePane = views.PaneByXBars
	m.byTool.Copilot = m.copilotState(rows)
	if m.byTool.Selected >= len(m.byTool.Rows) {
		m.byTool.Selected = 0
	}
}

// applyByToolFold recomputes the displayed rows from the retained unfolded list
// and the current expansion state. It is the ONE place the fold is applied, so
// the toggle and the load can never disagree about which tools are minor.
func (m *Model) applyByToolFold() {
	f := foldMinorTools(m.byToolAll, m.byTool.Grand, m.byTool.FoldOpen)
	m.byTool.Rows = f.Rows
	m.byTool.FoldIndex = f.Index
	m.byTool.FoldCount = f.Count
}

// toggleByToolFold expands or collapses the long tail and re-selects the fold
// row, which has moved by nothing but must stay under the cursor: the reader
// pressed it, and a toggle that dropped the selection would make a second press
// impossible without hunting for the row again. Returns false when the current
// selection is not the fold row, which is what tells drill() to fall through to
// its real drill.
func (m *Model) toggleByToolFold() bool {
	if m.byTool.FoldIndex < 0 || m.byTool.Selected != m.byTool.FoldIndex {
		return false
	}
	m.byTool.FoldOpen = !m.byTool.FoldOpen
	m.applyByToolFold()
	m.byTool.Selected = m.byTool.FoldIndex
	// The detail card follows the selection, and the fold row's card is built
	// from the row itself — no query, warm or cold.
	m.syncByToolDetail()
	return true
}

// loadByToolDetail loads the selected tool's daily trend, querying the store —
// background flights and warm apply-side reloads only. The distinct-session
// count comes straight off the selected row (store-level COUNT DISTINCT), so
// no per-session bucket materialization happens anywhere.
func (m *Model) loadByToolDetail() {
	b, ok := m.selectedByToolBucket()
	// The fold row names no tool, so there is no trend to query for it: its card
	// is built from its own summed numbers. Querying with its empty key would
	// filter the ledger to tool='' and draw a flat line as if the tail were idle.
	if m.byToolFoldSelected() {
		m.byTool.SelTrend = nil
		m.byTool.SelTrendErr = false
		m.byTool.SelSessions = 0
		return
	}
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
	if m.byToolFoldSelected() {
		m.byTool.SelTrend = nil
		m.byTool.SelTrendErr = false
		m.byTool.SelSessions = 0
		return
	}
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

// activityRankLimit caps the ranked page the Activity tab holds. The name
// vocabulary is a long tail — every mcp__server__tool id an agent has ever
// called is its own row — and the panel windows to what fits on screen anyway,
// so the cap only bounds what is carried and re-sorted in memory. It is stated
// in the panel's "1-12/143" readout, which is what keeps a capped list from
// reading as the whole list.
const activityRankLimit = 200

// activityRankDims groups the ranking by the identity of an invocation: its
// name, what kind of thing it is, and which agent CLI ran it. The tool belongs
// in the key because the same name is invoked by more than one CLI ("Bash" is
// claude-code's and opencode's), and merging those would attribute one tool's
// calls to another's cost.
var activityRankDims = []string{"name", "kind", "tool"}

// turnContextRankDims groups a turn-context ranking by the value and the tool
// that reported it. The tool is in the key for the same reason it is in
// activityRankDims: the same agent name run under two harnesses is two facts,
// and merging them would attribute one harness's turns to another's cost. It is
// also what feeds the coverage note — a value's rows say which tool produced
// them.
var turnContextRankDims = []string{"value", "tool"}

// loadActivity builds the Activity tab: the ranked invocation page, the kind
// breakdown (which also carries the grand total the honesty footnote quantifies
// against) and the per-bucket call counts behind the summary heat strip. Three
// queries, all through the ctx-aware cached path, none of them repeated when
// the tab is revisited inside one load generation.
func (m *Model) loadActivity() {
	if dim, ok := m.pivot.Dimension(); ok {
		m.loadTurnContext(dim)
		return
	}
	now := m.qnow()
	sp := m.span()

	rows, err := m.data.ActivityRank(m.qctx(), now, sp, m.crumbs, activityRankDims, activityOrder(m.sort), activityRankLimit)
	if err != nil {
		m.err = err
		return
	}
	kinds, err := m.data.ActivityGroup(m.qctx(), now, sp, m.crumbs, []string{"kind"})
	if err != nil {
		m.err = err
		return
	}
	dim := timelineDim(sp.R)
	calls, err := m.data.ActivityGroup(m.qctx(), now, sp, m.crumbs, []string{dim})
	if err != nil {
		m.err = err
		return
	}

	rows = filterActivity(rows, m.filter)
	if m.sort == SortName {
		// The ranking metric decided WHICH rows came back (the cap is applied in
		// SQL); the name mode only decides the order they are listed in. Sorting
		// the page here rather than asking for a different one keeps that honest —
		// the rows on screen are still the busiest ones.
		rows = sortActivityByName(rows)
	}

	m.activity = views.ActivityData{
		Rows:       rows,
		Kinds:      kinds.Buckets,
		Calls:      sortActivityBuckets(calls.Buckets, dim),
		CallsDim:   dim,
		Totals:     kinds.Totals,
		Selected:   m.activity.Selected,
		RangeLbl:   m.spanLabel(),
		OrderLbl:   activityOrderLabel(m.sort, PivotCalls),
		Limit:      activityRankLimit,
		ActivePane: views.PaneActivityRank,
	}
	m.clampActivitySelection()
}

// loadTurnContext builds the Activity tab for ONE turn-context dimension: the
// ranked page of values, the same window grouped by tool (which is both the
// coverage note and the grand total), and the per-bucket turn counts behind the
// summary heat strip.
//
// Three queries, mirroring the calls pivot's three, and EVERY one of them names
// the same dim. There is no path here that reads two dimensions, which is what
// makes the tab's numbers a partition rather than a sum — see the store's
// SummarizeTurnContext contract.
func (m *Model) loadTurnContext(dim model.TurnDimension) {
	now := m.qnow()
	sp := m.span()

	rows, err := m.data.TurnContextRank(m.qctx(), now, sp, m.crumbs, dim, turnContextRankDims,
		activityOrder(m.sort), activityRankLimit)
	if err != nil {
		m.err = err
		return
	}
	tools, err := m.data.TurnContextGroup(m.qctx(), now, sp, m.crumbs, dim, []string{"tool"})
	if err != nil {
		m.err = err
		return
	}
	bdim := timelineDim(sp.R)
	buckets, err := m.data.TurnContextGroup(m.qctx(), now, sp, m.crumbs, dim, []string{bdim})
	if err != nil {
		m.err = err
		return
	}

	rows = filterTurnContext(rows, m.filter)
	if m.sort == SortName {
		// Same contract as the calls pivot: the metric decided WHICH rows came
		// back (the cap is applied in SQL) and the name mode only decides the
		// order they are listed in, so the rows on screen are still the biggest.
		rows = sortTurnContextByName(rows)
	}

	m.activity = views.ActivityData{
		Pivot:      string(dim),
		CtxRows:    rows,
		CtxTools:   tools.Buckets,
		CtxBuckets: sortTurnContextBuckets(buckets.Buckets, bdim),
		CtxTotals:  tools.Totals,
		CallsDim:   bdim,
		Selected:   m.activity.Selected,
		RangeLbl:   m.spanLabel(),
		OrderLbl:   activityOrderLabel(m.sort, m.pivot),
		Limit:      activityRankLimit,
		ActivePane: views.PaneActivityRank,
	}
	m.clampActivitySelection()
}

// clampActivitySelection keeps the cursor inside whichever page the active
// pivot loaded. It reads the view's own RowCount rather than one of the two row
// slices: a pivot switch replaces the populated slice with the other one, and a
// clamp against the wrong field would leave the cursor past the end of the list
// now on screen.
func (m *Model) clampActivitySelection() {
	if n := m.activity.RowCount(); m.activity.Selected >= n || m.activity.Selected < 0 {
		m.activity.Selected = 0
	}
}

// filterTurnContext keeps rows whose context VALUE contains the
// (case-insensitive) filter substring — the same contract filterActivity
// applies to an invocation name.
func filterTurnContext(rows []store.TurnContextBucket, filter string) []store.TurnContextBucket {
	if filter == "" {
		return rows
	}
	lf := strings.ToLower(filter)
	out := make([]store.TurnContextBucket, 0, len(rows))
	for _, b := range rows {
		if strings.Contains(strings.ToLower(b.Keys["value"]), lf) {
			out = append(out, b)
		}
	}
	return out
}

// sortTurnContextByName returns the page ordered by value, then tool, on a copy:
// the slice handed back by TurnContextRank is the cached one, and the cache is
// shared with the UI thread.
func sortTurnContextByName(rows []store.TurnContextBucket) []store.TurnContextBucket {
	out := append([]store.TurnContextBucket(nil), rows...)
	slices.SortStableFunc(out, func(x, y store.TurnContextBucket) int {
		if c := strings.Compare(x.Keys["value"], y.Keys["value"]); c != 0 {
			return c
		}
		return strings.Compare(x.Keys["tool"], y.Keys["tool"])
	})
	return out
}

// sortTurnContextBuckets orders time-keyed turn-context buckets ascending by
// their (lexically sortable) bucket key, on a copy — same cache-sharing contract
// as sortTurnContextByName.
func sortTurnContextBuckets(rows []store.TurnContextBucket, dim string) []store.TurnContextBucket {
	out := append([]store.TurnContextBucket(nil), rows...)
	slices.SortStableFunc(out, func(x, y store.TurnContextBucket) int {
		return strings.Compare(x.Keys[dim], y.Keys[dim])
	})
	return out
}

// filterActivity keeps rows whose invocation name contains the
// (case-insensitive) filter substring — the same contract filterBuckets applies
// to a grouping dimension.
func filterActivity(rows []store.ActivityBucket, filter string) []store.ActivityBucket {
	if filter == "" {
		return rows
	}
	lf := strings.ToLower(filter)
	out := make([]store.ActivityBucket, 0, len(rows))
	for _, b := range rows {
		if strings.Contains(strings.ToLower(b.Keys["name"]), lf) {
			out = append(out, b)
		}
	}
	return out
}

// sortActivityByName returns the page ordered by invocation name, then tool, on
// a copy: the slice handed back by ActivityRank is the cached one, and the
// cache is shared with the UI thread.
func sortActivityByName(rows []store.ActivityBucket) []store.ActivityBucket {
	out := append([]store.ActivityBucket(nil), rows...)
	slices.SortStableFunc(out, func(x, y store.ActivityBucket) int {
		if c := strings.Compare(x.Keys["name"], y.Keys["name"]); c != 0 {
			return c
		}
		return strings.Compare(x.Keys["tool"], y.Keys["tool"])
	})
	return out
}

// sortActivityBuckets orders time-keyed activity buckets ascending by their
// (lexically sortable) bucket key, on a copy — same cache-sharing contract as
// sortActivityByName.
func sortActivityBuckets(rows []store.ActivityBucket, dim string) []store.ActivityBucket {
	out := append([]store.ActivityBucket(nil), rows...)
	slices.SortStableFunc(out, func(x, y store.ActivityBucket) int {
		return strings.Compare(x.Keys[dim], y.Keys[dim])
	})
	return out
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
		m.setOverviewTools(filterBuckets(byTool.Buckets, "tool", m.filter), tot.Total)
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
		m.setOverviewTools(filterBuckets(m.scrubComp[m.scrubIndex], "tool", m.filter), b.Total)
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
