package tui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// The workspace owns presentation and navigation only. Aggregates still pass
// through Data's background-load/cache handoff; widgets never query the store.
type usageWorkspace struct {
	appliedFilter      string
	returnFromActivity bool
	appliedGroup       string
	appliedSpan        Span
	codeSeq            uint64
	group              string
	restoreSelected    string
	rows               []store.Bucket
	cursor, top        int
	chart              UsageMetric
	trendMemo          *workspaceTrendMemo
	history            []workspacePlace
	appliedScope       []Crumb
	appliedRange       string
	overlay            string
	chooser            string
	content            string
	viewport           viewport.Model
	menu               []workspaceAction
	menuCursor         int
	suggestions        []UsageSuggestion
	suggestionContext  UsageInsightContext
	suggestionCursor   int
	suggestionReturn   bool
}

type workspacePlace struct {
	group, selected, filter string
	crumbs                  []Crumb
	sort                    Sort
	top                     int
}

type workspaceAction struct{ label, action string }

func newUsageWorkspace() usageWorkspace {
	return usageWorkspace{group: "model", chart: UsageMetricCost, trendMemo: &workspaceTrendMemo{}, viewport: viewport.New()}
}

func (m Model) workspaceGroup() string {
	if m.workspace.appliedGroup != "" {
		return m.workspace.appliedGroup
	}
	return m.workspaceRequestedGroup()
}

func (m Model) workspaceRequestedGroup() string {
	if m.workspace.group == "" {
		return "model"
	}
	return m.workspace.group
}

func workspaceGroupDims(group string) []string {
	if group == "session" {
		return []string{"session", "tool", "project"}
	}
	return []string{group}
}

func workspaceIdentity(b store.Bucket, group string) string {
	// Lengths prevent ambiguous separators in recorded session/project names.
	var s strings.Builder
	for _, dim := range []string{group, "tool", "project"} {
		v := b.Keys[dim]
		fmt.Fprintf(&s, "%d:%s", len(v), v)
	}
	return s.String()
}

func (m Model) workspaceSelected() (store.Bucket, bool) {
	if m.workspace.cursor < 0 || m.workspace.cursor >= len(m.workspace.rows) {
		return store.Bucket{}, false
	}
	return m.workspace.rows[m.workspace.cursor], true
}

func (m *Model) loadWorkspace(cacheOnly bool) {
	if m.err != nil {
		return
	}
	group := m.workspaceRequestedGroup()
	dims := workspaceGroupDims(group)
	f := m.data.filterFor(m.qctx(), m.qnow(), m.span(), m.crumbs, dims)
	var s *store.Summary
	if cacheOnly {
		var ok bool
		s, ok = m.data.cachedSummary(f)
		if !ok {
			m.detailWanted = true
			return
		}
	} else {
		var err error
		s, err = m.data.GroupByDims(m.qctx(), m.qnow(), m.span(), m.crumbs, dims)
		if err != nil {
			m.err = err
			return
		}
	}
	selected := ""
	if b, ok := m.workspaceSelected(); ok && m.workspace.appliedGroup == group {
		selected = workspaceIdentity(b, group)
	}
	if m.workspace.restoreSelected != "" {
		selected = m.workspace.restoreSelected
		m.workspace.restoreSelected = ""
	}
	rows := slices.Clone(filterBuckets(s.Buckets, group, m.filter))
	sortBuckets(rows, group, m.sort)
	m.workspace.rows = rows
	m.workspace.cursor = min(m.workspace.cursor, max(0, len(rows)-1))
	for i, b := range rows {
		if workspaceIdentity(b, group) == selected {
			m.workspace.cursor = i
			break
		}
	}
	m.workspace.appliedScope = slices.Clone(m.crumbs)
	m.workspace.appliedFilter = m.filter
	start := "All history"
	if !f.Since.IsZero() {
		start = f.Since.Format("Jan 2, 2006")
	}
	end := f.Until
	if end.IsZero() {
		end = m.qnow()
	}
	m.workspace.appliedRange = start + " – " + end.Format("Jan 2, 2006 15:04")
	m.workspace.appliedGroup = group
	m.workspace.appliedSpan = m.span()
	m.rowChosen = false
}

// Apply overview and comparison as one snapshot. A cache eviction between a
// flight and its delivery must retain the complete previous picture.
func (m *Model) loadOverviewSnapshot(cacheOnly bool) {
	if m.classicOverview {
		m.loadOverviewWith(cacheOnly)
		return
	}
	next := *m
	next.detailWanted = false
	next.loadWorkspaceOverview(cacheOnly)
	if next.err == nil && !next.detailWanted {
		next.loadWorkspace(cacheOnly)
	}
	if next.err != nil || next.detailWanted {
		m.err = next.err
		m.detailWanted = next.detailWanted
		return
	}
	m.overview, m.tlData, m.scrubComp = next.overview, next.tlData, next.scrubComp
	m.workspace = next.workspace
	m.err = nil
	m.detailWanted = false
	m.rowChosen = false
}

// The workspace needs the timeline's totals and the comparison rows. Legacy
// by-harness, scrub-composition and previous-window summaries belong to the
// classic view and are loaded only when that view is opened.
func (m *Model) loadWorkspaceOverview(cacheOnly bool) {
	var timeline *store.Summary
	var dim string
	var err error
	if cacheOnly {
		var ok bool
		timeline, dim, ok = m.data.TimelineCached(m.qnow(), m.span(), m.crumbs)
		if !ok {
			m.detailWanted = true
			return
		}
	} else {
		timeline, dim, err = m.data.Timeline(m.qctx(), m.qnow(), m.span(), m.crumbs)
	}
	if err != nil {
		m.err = err
		return
	}
	m.tlData = views.TimelineData{Buckets: timeline.Buckets, Dim: dim}
	m.overview = views.OverviewData{Totals: timeline.Totals, Timeline: timeline.Buckets, TimelineDim: dim, RangeLbl: m.spanLabel()}
	m.scrubComp = nil
	m.err = nil
}

func workspaceGroupLabel(group string) string {
	switch group {
	case "tool":
		return "Harness"
	case "model":
		return "Models"
	case "provider":
		return "Providers"
	case "project":
		return "Projects"
	default:
		return "Sessions"
	}
}

func workspaceName(b store.Bucket, group string) string {
	if v := b.Keys[group]; v != "" {
		return v
	}
	return "Unknown " + group
}

func (m Model) workspaceContext(selected bool) UsageInsightContext {
	scope := slices.Clone(m.workspace.appliedScope)
	c := UsageInsightContext{Totals: m.overview.Totals, Timeline: m.tlData.Buckets,
		Scope: scope, RangeLabel: m.workspace.appliedRange, Stale: m.fresh != FreshLive}
	// Contributors must partition the complete scope, even while the table's
	// text filter hides rows. Omit them rather than implying partial rows sum up.
	if m.workspace.appliedFilter == "" {
		c.Contributors = m.workspace.rows
	}
	if selected {
		if b, ok := m.workspaceSelected(); ok {
			c.Totals, c.Timeline, c.Contributors = b, nil, nil
			c.Scope = workspaceScope(scope, b, m.workspaceGroup())
		}
	}
	var labels []string
	for _, cr := range c.Scope {
		v := cr.Value
		if v == "" {
			v = "unknown"
		}
		labels = append(labels, cr.Dim+": "+v)
	}
	c.ScopeLabel = strings.Join(labels, " / ")
	if c.ScopeLabel == "" {
		c.ScopeLabel = "All usage"
	}
	return c
}

func workspaceScope(scope []Crumb, b store.Bucket, group string) []Crumb {
	out := slices.Clone(scope)
	add := func(dim string) {
		for _, c := range out {
			if c.Dim == dim {
				return
			}
		}
		out = append(out, Crumb{Dim: dim, Value: b.Keys[dim]})
	}
	add(group)
	if group == "session" {
		add("tool")
		add("project")
	}
	return out
}

func (m Model) renderWorkspace(w, h int) string {
	// Direct range controls stay alongside the data at every supported size.
	toolbar := m.workspaceRangeBar(w)
	if m.workspace.chooser != "" {
		toolbar += "\n" + m.workspaceChoices(w)
	}
	bodyH := max(1, h-lipgloss.Height(toolbar))
	g := views.WorkspaceGeometry(w, bodyH, len(m.workspace.rows))
	d := views.WorkspaceData{Totals: m.overview.Totals, Timeline: m.tlData.Buckets, RowCount: len(m.workspace.rows),
		Table: m.workspaceTable(g.TableW, g.TableH), Detail: m.workspaceDetail(g.DetailW, g.DetailH),
		Machine: views.SysStrip(m.vctx, m.sysGauges(), w),
	}
	if g.Side {
		d.Trend = m.renderWorkspaceTrend(g.ChartW, g.ChartH)
		d.Suggestion = m.workspaceSuggestion(g.SuggestionW, g.SuggestionH)
	}
	return lipgloss.JoinVertical(lipgloss.Left, toolbar, views.Workspace(m.vctx, d, w, bodyH))
}

// Value choices expand in place so the data and range remain visible. Brackets
// mark the applied value; the arrow marks keyboard focus before Enter applies it.
func (m Model) workspaceChoices(w int) string {
	line := m.th.Title.Render(m.workspace.chooser + ":")
	var lines []string
	for i, a := range m.workspace.menu {
		label := a.label
		if m.workspaceChoiceApplied(a.action) {
			label = "[" + label + "]"
		}
		style := m.th.Crumb
		if i == m.workspace.menuCursor {
			label = "›" + label
			style = m.th.CrumbActive
		}
		button := m.zoneMark(fmt.Sprintf("workspace-choice-%d", i), style.Render(label))
		if lipgloss.Width(line)+1+lipgloss.Width(button) > w {
			lines = append(lines, line)
			line = button
		} else {
			line += " " + button
		}
	}
	close := m.zoneMark("workspace-choice-dismiss", m.th.Subtle.Render("×"))
	if lipgloss.Width(line)+2 > w {
		lines = append(lines, line)
		line = close
	} else {
		line += " " + close
	}
	return strings.Join(append(lines, line), "\n")
}

func (m Model) workspaceChoiceApplied(action string) bool {
	return action == "group:"+m.workspaceGroup() || action == fmt.Sprintf("sort:%d", m.sort) || action == "chart:"+string(m.workspace.chart)
}

func (m Model) workspaceRangeBar(w int) string {
	parts := []string{"Range"}
	for i, r := range []Range{RangeToday, Range7d, Range30d, RangeAll} {
		label := []string{"Today", "7d", "30d", "All"}[i]
		if m.rng == r {
			label = "[" + label + "]"
			label = m.th.CrumbActive.Render(label)
		}
		parts = append(parts, m.zoneMark(fmt.Sprintf("workspace-range-%d", r), label))
	}
	parts = append(parts, m.zoneMark("workspace-menu-more", m.th.CrumbActive.Render("More")))
	if w >= 90 {
		parts = append(parts, m.th.Subtle.Render(m.workspace.appliedRange))
	}
	return m.clampBlock(strings.Join(parts, "  "), w, 1)
}

func (m Model) workspaceTable(w, h int) string {
	if w < 1 || h < 1 {
		return ""
	}
	header := m.zoneMark("workspace-menu-group", m.th.CrumbActive.Render(workspaceGroupLabel(m.workspaceGroup())+" ▾")) + "  " +
		m.zoneMark("workspace-menu-sort", "Sort: "+m.sort.Label()+" ▾")
	if h == 1 {
		return m.clampBlock(header, w, h)
	}
	if len(m.workspace.rows) == 0 {
		return m.clampBlock(header+"\nNo matching usage", w, h)
	}
	rowH := max(1, h-2)
	top := min(m.workspace.top, max(0, len(m.workspace.rows)-rowH))
	if m.workspace.cursor < top {
		top = m.workspace.cursor
	}
	if m.workspace.cursor >= top+rowH {
		top = m.workspace.cursor - rowH + 1
	}
	header += fmt.Sprintf("  %d–%d/%d", top+1, min(len(m.workspace.rows), top+rowH), len(m.workspace.rows))
	cols := []table.Column{{Title: workspaceGroupLabel(m.workspaceGroup()), Width: max(3, w-16)}, {Title: "Cost", Width: 12}}
	if w >= 65 {
		cols = []table.Column{{Title: workspaceGroupLabel(m.workspaceGroup()), Width: w - 44}, {Title: "Input", Width: 8}, {Title: "Output", Width: 8}, {Title: "Cache", Width: 8}, {Title: "Cost", Width: 10}}
	}
	rows := make([]table.Row, 0, rowH)
	for i := top; i < min(len(m.workspace.rows), top+rowH); i++ {
		b := m.workspace.rows[i]
		name := "  " + workspaceName(b, m.workspaceGroup())
		if i == m.workspace.cursor {
			name = "› " + workspaceName(b, m.workspaceGroup())
		}
		cost := workspaceCost(b)
		row := table.Row{name, cost}
		if len(cols) == 5 {
			row = table.Row{name, HumanizeTokens(b.Input), HumanizeTokens(b.Output), humanizeCache(b), cost}
		}
		rows = append(rows, row)
	}
	st := table.DefaultStyles()
	st.Header = lipgloss.NewStyle().Bold(true).Foreground(m.th.Muted).Padding(0, 1)
	st.Cell = lipgloss.NewStyle().Padding(0, 1)
	st.Selected = lipgloss.NewStyle().Bold(true).Foreground(m.th.Accent)
	// Configure styles before height, so Bubbles budgets the final header once.
	// Setting each property after construction re-renders every visible row.
	t := table.New(table.WithColumns(cols), table.WithRows(rows), table.WithStyles(st), table.WithWidth(w), table.WithHeight(rowH+1), table.WithFocused(true))
	if m.workspace.cursor != top {
		t.SetCursor(m.workspace.cursor - top)
	}
	lines := strings.Split(t.View(), "\n")
	for i := 1; i < len(lines) && i <= len(rows); i++ {
		lines[i] = m.zoneMark(fmt.Sprintf("workspace-row-%d", top+i-1), lines[i])
	}
	return m.clampBlock(header+"\n"+strings.Join(lines, "\n"), w, h)
}

func humanizeCache(b store.Bucket) string {
	n := usageMetricNumber(UsageMetricCache, b)
	if n.IsInt64() {
		return HumanizeTokens(n.Int64())
	}
	// The combined read/write count can exceed int64 even though each source
	// counter fits. Keep exact arithmetic in the inspector and compact the label.
	v, _ := n.Float64()
	return trimDecimal(v/1_000_000_000_000) + "T"
}

func workspaceCost(b store.Bucket) string {
	if b.Events == 0 || b.UnpricedEvents >= b.Events {
		return "Unknown"
	}
	s := model.FormatCost(b.CostMicroUSD, b.ComputedCostEvents > 0, true)
	if b.UnpricedEvents > 0 {
		s = "≥ " + s
	}
	return s
}

func (m Model) workspaceDetail(w, h int) string {
	if h < 1 || w < 1 {
		return ""
	}
	b, ok := m.workspaceSelected()
	if !ok {
		return "Selection\nSelect a row to inspect its usage."
	}
	lines := []string{
		m.zoneMark("workspace-details", m.th.CrumbActive.Render("› "+workspaceName(b, m.workspaceGroup())+" · Details")),
		"Cost " + workspaceCost(b) + " · " + fmt.Sprint(b.Events) + " events",
		"Input " + HumanizeTokens(b.Input) + " · Output " + HumanizeTokens(b.Output),
		"Cache read " + HumanizeTokens(b.CacheRead) + " · write " + HumanizeTokens(b.CacheCreation),
		"Total " + HumanizeTokens(b.Total) + " · provider counter",
		usagePricingCoverage(b),
	}
	if m.workspaceGroup() == "model" {
		lines = append(lines, "Model-attributed code changes unavailable")
	}
	lines = append(lines, m.zoneMark("workspace-open", "Enter / Open sessions")+"  "+m.zoneMark("workspace-suggestions", "Tips"))
	return m.clampBlock(strings.Join(lines, "\n"), w, h)
}

func (m Model) workspaceSuggestion(w, h int) string {
	s := BuildUsageSuggestions(m.workspaceContext(true))
	text := "Suggestions · local rules\nNo supported suggestion for this selection."
	if len(s) > 0 {
		text = "Suggestions · local rules\n" + s[0].Title + "\n" + s[0].Evidence + "\nOpen evidence and inspect →"
	}
	return m.zoneMark("workspace-suggestion-card", lipgloss.NewStyle().Width(w).MaxHeight(h).Render(text))
}

func (m *Model) openWorkspaceText(title, content string) {
	m.workspace.chooser = ""
	m.workspace.codeSeq++
	m.workspace.suggestions = nil
	m.workspace.overlay = title
	m.workspace.content = content
	m.workspace.menu = nil
	m.workspace.viewport = viewport.New(viewport.WithWidth(max(1, m.frameW()-4)), viewport.WithHeight(max(1, m.frameH()-4)))
	m.workspace.viewport.SetContent(lipgloss.NewStyle().Width(max(1, m.frameW()-4)).Render(content))
}

func (m *Model) workspaceRelayout() {
	if m.workspace.overlay == "" || m.workspace.menu != nil {
		return
	}
	m.workspace.viewport.SetWidth(max(1, m.frameW()-4))
	m.workspace.viewport.SetHeight(max(1, m.frameH()-4))
	m.workspace.viewport.SetContent(lipgloss.NewStyle().Width(max(1, m.frameW()-4)).Render(m.workspace.content))
}

func (m Model) renderWorkspaceOverlay() string {
	w, h := m.frameW(), m.frameH()
	close := m.zoneMark("workspace-close", m.th.CrumbActive.Render("← Back"))
	head := close + "  " + m.th.Title.Render(m.workspace.overlay)
	if m.workspace.menu != nil {
		cols := []table.Column{{Title: "Choose an action", Width: max(1, w-6)}}
		n := max(1, h-4)
		top := max(0, m.workspace.menuCursor-n+1)
		var rows []table.Row
		for _, a := range m.workspace.menu[top:min(len(m.workspace.menu), top+n)] {
			rows = append(rows, table.Row{a.label})
		}
		st := table.DefaultStyles()
		st.Selected = lipgloss.NewStyle().Bold(true).Foreground(m.th.Accent)
		st.Header = lipgloss.NewStyle().Foreground(m.th.Muted).Padding(0, 1)
		t := table.New(table.WithColumns(cols), table.WithRows(rows), table.WithStyles(st), table.WithHeight(n+1), table.WithWidth(w-2), table.WithFocused(true))
		if m.workspace.menuCursor != top {
			t.SetCursor(m.workspace.menuCursor - top)
		}
		lines := strings.Split(t.View(), "\n")
		for i := 1; i < len(lines) && i <= len(rows); i++ {
			lines[i] = m.zoneMark(fmt.Sprintf("workspace-action-%d", top+i-1), lines[i])
		}
		return m.scan(m.appFrame(m.clampFrame(head + "\n" + strings.Join(lines, "\n") + "\n↑↓ Select · Enter Open · Esc Back")))
	}
	v := m.workspace.viewport

	foot := m.zoneMark("workspace-scroll-up", "↑ Up") + "  " + m.zoneMark("workspace-scroll-down", "↓ Down")
	body := lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(m.th.Border).Width(w - 2).Height(h - 4).Render(v.View())
	return m.scan(m.appFrame(m.clampFrame(head + "\n" + body + "\n" + foot)))
}
