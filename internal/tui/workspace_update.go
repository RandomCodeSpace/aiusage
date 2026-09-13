package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/store"
)

func (m *Model) workspaceMove(delta int) {
	m.workspace.cursor = max(0, min(len(m.workspace.rows)-1, m.workspace.cursor+delta))
	m.rowChosen = false
}

func (m *Model) workspaceSave() {
	p := workspacePlace{group: m.workspaceGroup(), crumbs: slices.Clone(m.crumbs), filter: m.filter, sort: m.sort, top: m.workspace.top}
	if b, ok := m.workspaceSelected(); ok {
		p.selected = workspaceIdentity(b, p.group)
	}
	m.workspace.history = append(slices.Clone(m.workspace.history), p)
}

func (m Model) workspaceBack() (Model, tea.Cmd) {
	if m.workspace.chooser != "" {
		m.workspace.chooser = ""
		m.workspace.menu = nil
		return m, nil
	}
	if m.workspace.overlay != "" {
		if m.workspace.suggestionReturn {
			m.workspace.overlay = "Suggestions"
			m.workspace.content = ""
			m.workspace.menu = workspaceSuggestionActions(m.workspace.suggestions)
			m.workspace.menuCursor = m.workspace.suggestionCursor
			m.workspace.suggestionReturn = false
			return m, nil
		}
		m.workspace.overlay = ""
		m.workspace.menu = nil
		m.workspace.suggestions = nil
		return m, nil
	}
	if n := len(m.workspace.history); n > 0 {
		p := m.workspace.history[n-1]
		m.workspace.history = slices.Clone(m.workspace.history[:n-1])
		m.workspace.group = p.group
		m.crumbs = slices.Clone(p.crumbs)
		m.filter = p.filter
		m.sort = p.sort
		m.workspace.top = p.top
		m.workspace.restoreSelected = p.selected
		return m, m.startLoad()
	}
	if len(m.crumbs) > 0 {
		m.crumbs = nil
		m.workspace.group = "model"
		return m, m.startLoad()
	}
	return m, nil
}

func (m Model) workspaceOpen() (Model, tea.Cmd) {
	if m.fresh == FreshCutIn {
		return m, nil
	}
	b, ok := m.workspaceSelected()
	if !ok {
		return m, nil
	}
	if m.workspaceGroup() == "session" {
		return m.workspaceDetails()
	}
	m.workspaceSave()
	m.crumbs = workspaceScope(m.workspace.appliedScope, b, m.workspaceGroup())
	m.rng = m.workspace.appliedSpan.R
	m.step = m.workspace.appliedSpan.Step
	m.syncStepKeys()
	if m.workspaceGroup() == "tool" || m.workspaceGroup() == "provider" {
		m.workspace.group = "model"
	} else {
		m.workspace.group = "session"
	}
	m.filter = ""
	m.workspace.cursor = 0
	m.workspace.top = 0
	return m, m.startLoad()
}

func (m *Model) workspaceMenu(kind string) {
	if m.workspace.chooser == kind {
		m.workspace.chooser = ""
		m.workspace.menu = nil
		return
	}
	m.workspace.chooser = ""
	m.workspace.overlay = kind
	m.workspace.menuCursor = 0
	m.workspace.suggestions = nil
	var a []workspaceAction
	switch kind {
	case "Comparison":
		for i, b := range m.workspace.rows {
			a = append(a, workspaceAction{workspaceName(b, m.workspaceGroup()) + " · " + workspaceCost(b), fmt.Sprintf("row:%d", i)})
		}
		if len(a) == 0 {
			a = append(a, workspaceAction{"No matching usage", "back"})
		}
	case "Group":
		for _, g := range []string{"model", "tool", "provider", "project", "session"} {
			a = append(a, workspaceAction{workspaceGroupLabel(g), "group:" + g})
		}
	case "Sort":
		for _, s := range []Sort{SortCost, SortTotal, SortEvents, SortName} {
			a = append(a, workspaceAction{s.Label(), fmt.Sprintf("sort:%d", s)})
		}
	case "Chart":
		for _, metric := range []UsageMetric{UsageMetricCost, UsageMetricInput, UsageMetricOutput, UsageMetricCache, UsageMetricTotal} {
			a = append(a, workspaceAction{string(metric), "chart:" + string(metric)})
		}
	default:
		a = []workspaceAction{{"Compare rows", "menu:Comparison"}, {"Open selected row", "open"}, {"Full selected details", "details"}, {"Group by…", "menu:Group"}, {"Sort rows…", "menu:Sort"}, {"Chart metric…", "menu:Chart"}, {"Input details", "metric:Input"}, {"Output details", "metric:Output"}, {"Cache details", "metric:Cache"}, {"Cost details", "metric:Cost"}, {"Total details", "metric:Total"}, {"Suggestions for selection", "suggestions"}, {"Tool calls / skills / MCP / context", "activity"}, {"Filter comparison", "filter"}, {"Classic usage trend", "classic"}, {"Usage leverage chart", "leverage"}, {"Refresh data", "refresh"}, {"Back to parent", "back"}}
	}
	m.workspace.menu = a
	if kind == "Group" || kind == "Sort" || kind == "Chart" {
		m.workspace.overlay = ""
		m.workspace.chooser = kind
		for i, action := range a {
			if m.workspaceChoiceApplied(action.action) {
				m.workspace.menuCursor = i
				break
			}
		}
	}
}

func (m Model) workspaceDetails() (Model, tea.Cmd) {
	b, ok := m.workspaceSelected()
	if !ok {
		return m, nil
	}
	c := m.workspaceContext(true)
	content := UsageMetricInspector(UsageMetricCost, c) + "\n\n" + UsageMetricInspector(UsageMetricTotal, c) + "\n\n" + usageMetricValue(UsageMetricCache, b)
	if m.workspaceGroup() == "model" {
		content += "\n\nModel-attributed code changes unavailable. Recorded session snapshots have no model attribution."
	}
	m.openWorkspaceText("Selected usage", content)
	if m.workspaceGroup() == "session" {
		tool, session, project := b.Keys["tool"], b.Keys["session"], b.Keys["project"]
		m.workspace.content += "\n\nRecorded session lines · Lifetime\nLoading recorded counts…"
		m.workspaceRelayout()
		if tool == "" || session == "" {
			m.workspace.content = content + "\n\nRecorded session lines unavailable: exact harness and session are required."
			return m, nil
		}
		gen, seq := m.loadGen, m.workspace.codeSeq
		data, ctx := m.data, m.detail.next()
		return m, func() tea.Msg {
			summary, err := data.SessionCodeChanges(ctx, tool, session, project)
			return workspaceCodeMsg{gen: gen, seq: seq, summary: summary, err: err, allProjects: project == ""}
		}
	}
	return m, nil
}

func (m Model) workspaceAction(action string) (Model, tea.Cmd) {
	if strings.HasPrefix(action, "menu:") {
		m.workspaceMenu(strings.TrimPrefix(action, "menu:"))
		return m, nil
	}
	m.workspace.overlay = ""
	m.workspace.chooser = ""
	m.workspace.menu = nil
	switch {
	case strings.HasPrefix(action, "row:"):
		i, _ := strconv.Atoi(strings.TrimPrefix(action, "row:"))
		m.workspace.cursor = max(0, min(len(m.workspace.rows)-1, i))
		m.rowChosen = false
		return m.workspaceDetails()
	case strings.HasPrefix(action, "group:"):
		m.workspaceSave()
		m.workspace.group = strings.TrimPrefix(action, "group:")
		m.workspace.cursor = 0
		m.workspace.top = 0
		m.filter = ""
		return m, m.startLoad()
	case strings.HasPrefix(action, "sort:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(action, "sort:"))
		m.sort = Sort(n)
		selected := ""
		if b, ok := m.workspaceSelected(); ok {
			selected = workspaceIdentity(b, m.workspaceGroup())
		}
		m.workspace.rows = slices.Clone(m.workspace.rows)
		sortBuckets(m.workspace.rows, m.workspaceGroup(), m.sort)
		for i, b := range m.workspace.rows {
			if workspaceIdentity(b, m.workspaceGroup()) == selected {
				m.workspace.cursor = i
				break
			}
		}
		m.rowChosen = false
		return m, nil
	case strings.HasPrefix(action, "chart:"):
		m.workspace.chart = UsageMetric(strings.TrimPrefix(action, "chart:"))
		return m, nil
	case strings.HasPrefix(action, "metric:"):
		metric := UsageMetric(strings.TrimPrefix(action, "metric:"))
		m.openWorkspaceText(string(metric)+" inspector", UsageMetricInspector(metric, m.workspaceContext(false)))
		return m, nil
	case strings.HasPrefix(action, "suggestion:"):
		i, _ := strconv.Atoi(strings.TrimPrefix(action, "suggestion:"))
		return m.workspaceSuggestionInspector(i)
	}
	switch action {
	case "open":
		return m.workspaceOpen()
	case "details":
		return m.workspaceDetails()
	case "back":
		return m.workspaceBack()
	case "suggestions":
		if _, ok := m.workspaceSelected(); !ok {
			return m, nil
		}
		c := m.workspaceContext(true)
		s := BuildUsageSuggestions(c)
		if len(s) == 0 {
			m.openWorkspaceText("Suggestions", "Local suggestions · "+c.ScopeLabel+"\n\nEvidence from "+m.workspace.appliedRange+"\n\nNo supported suggestion for this selection. No AI service was called.")
			return m, nil
		}
		m.openWorkspaceText("Suggestions", "")
		m.workspace.menu = workspaceSuggestionActions(s)
		m.workspace.menuCursor = 0
		m.workspace.suggestions = s
		m.workspace.suggestionContext = c
		m.workspace.suggestionReturn = false
		return m, nil
	case "activity":
		m.workspaceSave()
		m.workspace.returnFromActivity = true
		if b, ok := m.workspaceSelected(); ok {
			m.crumbs = workspaceScope(m.workspace.appliedScope, b, m.workspaceGroup())
			m.rng = m.workspace.appliedSpan.R
			m.step = m.workspace.appliedSpan.Step
			m.syncStepKeys()
		}
		return m, m.setView(ViewActivity)
	case "up":
		m.workspaceMove(-1)
		return m, nil
	case "down":
		m.workspaceMove(1)
		return m, nil
	case "filter":
		m.filtering = true
		m.filterUI.SetValue(m.filter)
		return m, m.filterUI.Focus()
	case "classic":
		m.classicOverview = true
		m.heroPivot = false
		return m, m.startLoad()
	case "leverage":
		m.classicOverview = true
		m.heroPivot = true
		return m, m.startLoad()
	case "refresh":
		m.data.Invalidate()
		return m, m.startLoad()
	}
	return m, nil
}

func workspaceSuggestionActions(suggestions []UsageSuggestion) []workspaceAction {
	actions := make([]workspaceAction, 0, len(suggestions))
	for i, suggestion := range suggestions {
		actions = append(actions, workspaceAction{
			label:  suggestion.Title + " · " + string(suggestion.Metric),
			action: "suggestion:" + strconv.Itoa(i),
		})
	}
	return actions
}

func (m Model) workspaceSuggestionInspector(i int) (Model, tea.Cmd) {
	if i < 0 || i >= len(m.workspace.suggestions) {
		return m, nil
	}
	suggestion := m.workspace.suggestions[i]
	context := m.workspace.suggestionContext
	context.Scope = slices.Clone(suggestion.Scope)
	content := strings.Join([]string{
		suggestion.Title,
		"",
		suggestion.Evidence,
		"",
		suggestion.Action,
		"",
		suggestion.Limits,
		"",
		UsageMetricInspector(suggestion.Metric, context),
	}, "\n")
	suggestions := slices.Clone(m.workspace.suggestions)
	m.openWorkspaceText(string(suggestion.Metric)+" inspector", content)
	m.workspace.suggestions = suggestions
	m.workspace.suggestionCursor = i
	m.workspace.suggestionReturn = true
	return m, nil
}

func (m Model) workspaceKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	k := msg.String()
	if m.workspace.overlay != "" {
		if k == "ctrl+c" || k == "q" {
			return m, tea.Sequence(tea.ClearScreen, tea.Quit), true
		}
		if k == "esc" || k == "backspace" {
			n, c := m.workspaceBack()
			return n, c, true
		}
		if m.workspace.menu != nil {
			switch k {
			case "up", "k":
				m.workspace.menuCursor = max(0, m.workspace.menuCursor-1)
			case "down", "j":
				m.workspace.menuCursor = min(len(m.workspace.menu)-1, m.workspace.menuCursor+1)
			case "enter":
				n, c := m.workspaceAction(m.workspace.menu[m.workspace.menuCursor].action)
				return n, c, true
			}
		} else {
			m.workspace.viewport, _ = m.workspace.viewport.Update(msg)
		}
		return m, nil, true
	}
	if m.view != ViewOverview || m.classicOverview || m.filtering {
		return m, nil, false
	}
	if m.workspace.chooser != "" {
		switch k {
		case "left", "up", "h", "k":
			m.workspace.menuCursor = max(0, m.workspace.menuCursor-1)
			return m, nil, true
		case "right", "down", "l", "j":
			m.workspace.menuCursor = min(len(m.workspace.menu)-1, m.workspace.menuCursor+1)
			return m, nil, true
		case "enter":
			n, c := m.workspaceAction(m.workspace.menu[m.workspace.menuCursor].action)
			return n, c, true
		case "esc", "backspace":
			n, c := m.workspaceBack()
			return n, c, true
		}
	}
	switch k {
	case "down", "j":
		m.workspaceMove(1)
	case "up", "k":
		m.workspaceMove(-1)
	case "home", "g":
		m.workspaceMove(-len(m.workspace.rows))
	case "end", "G":
		m.workspaceMove(len(m.workspace.rows))
	case "pgdown":
		m.workspaceMove(5)
	case "pgup":
		m.workspaceMove(-5)
	case "enter", "right":
		n, c := m.workspaceOpen()
		return n, c, true
	case "esc", "backspace", "left":
		n, c := m.workspaceBack()
		return n, c, true
	case "o":
		m.workspaceMenu("Group")
	case "s":
		m.workspaceMenu("Sort")
	case "c":
		m.workspaceMenu("Chart")
	case "a":
		m.workspaceMenu("More")
	case "d":
		n, c := m.workspaceDetails()
		return n, c, true
	case "i":
		n, c := m.workspaceAction("metric:Cost")
		return n, c, true
	case "u":
		n, c := m.workspaceAction("suggestions")
		return n, c, true
	case "f6", "f7", "f8", "f9":
		i := map[string]int{"f6": 0, "f7": 1, "f8": 2, "f9": 3}[k]
		n, c := m.workspaceSetRange([]Range{RangeToday, Range7d, Range30d, RangeAll}[i])
		return n, c, true
	default:
		return m, nil, false
	}
	return m, nil, true
}

func (m Model) workspaceMouse(msg tea.MouseMsg) (Model, tea.Cmd, bool) {
	if m.workspace.overlay == "" && (m.view != ViewOverview || m.classicOverview) {
		return m, nil, false
	}
	if wheel, ok := msg.(tea.MouseWheelMsg); ok {
		dir := 1
		if wheel.Button == tea.MouseWheelUp {
			dir = -1
		}
		if m.workspace.overlay != "" || m.workspace.chooser != "" {
			if m.workspace.menu != nil {
				m.workspace.menuCursor = max(0, min(len(m.workspace.menu)-1, m.workspace.menuCursor+dir))
			} else {
				m.workspace.viewport, _ = m.workspace.viewport.Update(msg)
			}
		} else {
			m.workspaceMove(dir)
		}
		return m, nil, true
	}
	click, ok := msg.(tea.MouseClickMsg)
	if !ok {
		return m, nil, m.workspace.overlay != ""
	}
	if click.Button == tea.MouseRight {
		n, c := m.workspaceBack()
		return n, c, true
	}
	if click.Button != tea.MouseLeft || m.zoneMgr == nil {
		return m, nil, m.workspace.overlay != ""
	}
	hit := func(id string) bool { return m.zoneMgr.Get(id).InBounds(click) }
	if m.workspace.chooser != "" {
		if hit("workspace-choice-close") || hit("workspace-choice-dismiss") {
			n, c := m.workspaceBack()
			return n, c, true
		}
		if hit("workspace-choice-prev") {
			m.workspace.menuCursor = max(0, m.workspace.menuCursor-1)
			return m, nil, true
		}
		if hit("workspace-choice-next") {
			m.workspace.menuCursor = min(len(m.workspace.menu)-1, m.workspace.menuCursor+1)
			return m, nil, true
		}
		if hit("workspace-choice-apply") {
			n, c := m.workspaceAction(m.workspace.menu[m.workspace.menuCursor].action)
			return n, c, true
		}
		for i, a := range m.workspace.menu {
			if hit(fmt.Sprintf("workspace-choice-%d", i)) {
				n, c := m.workspaceAction(a.action)
				return n, c, true
			}
		}
	}
	if m.workspace.overlay != "" {
		if hit("workspace-close") {
			n, c := m.workspaceBack()
			return n, c, true
		}
		if hit("workspace-scroll-up") {
			m.workspace.viewport.ScrollUp(3)
		}
		if hit("workspace-scroll-down") {
			m.workspace.viewport.ScrollDown(3)
		}
		for i, a := range m.workspace.menu {
			if hit(fmt.Sprintf("workspace-action-%d", i)) {
				n, c := m.workspaceAction(a.action)
				return n, c, true
			}
		}
		return m, nil, true
	}
	for depth := 0; depth <= len(m.crumbs); depth++ {
		if hit(views.CrumbZone(depth)) {
			for i := len(m.workspace.history) - 1; i >= 0; i-- {
				if len(m.workspace.history[i].crumbs) == depth {
					m.workspace.history = slices.Clone(m.workspace.history[:i+1])
					n, c := m.workspaceBack()
					return n, c, true
				}
			}
			if depth == len(m.crumbs) {
				return m, nil, true
			}
			m.crumbs = slices.Clone(m.crumbs[:depth])
			m.workspace.group = "model"
			return m, m.startLoad(), true
		}
	}
	for _, r := range []Range{RangeToday, Range7d, Range30d, RangeAll} {
		if hit(fmt.Sprintf("workspace-range-%d", r)) {
			n, c := m.workspaceSetRange(r)
			return n, c, true
		}
	}
	for _, a := range []struct{ zone, action string }{{views.ZoneWorkspaceInput, "metric:Input"}, {views.ZoneWorkspaceOutput, "metric:Output"}, {views.ZoneWorkspaceCache, "metric:Cache"}, {views.ZoneWorkspaceCost, "metric:Cost"}, {"workspace-menu-more", "menu:More"}, {"workspace-footer-more", "menu:More"}, {"workspace-menu-group", "menu:Group"}, {"workspace-menu-sort", "menu:Sort"}, {"workspace-details", "details"}, {"workspace-footer-details", "details"}, {"workspace-back", "back"}, {"workspace-up", "up"}, {"workspace-down", "down"}, {"workspace-filter", "filter"}, {"workspace-open", "open"}, {"workspace-footer-open", "open"}, {"workspace-suggestions", "suggestions"}, {"workspace-suggestion-card", "suggestions"}} {
		if hit(a.zone) {
			n, c := m.workspaceAction(a.action)
			return n, c, true
		}
	}
	for _, metric := range []UsageMetric{UsageMetricInput, UsageMetricOutput, UsageMetricCache, UsageMetricCost, UsageMetricTotal} {
		if hit("workspace-chart-" + string(metric)) {
			m.workspace.chart = metric
			return m, nil, true
		}
	}
	for i := range m.workspace.rows {
		if hit(fmt.Sprintf("workspace-row-%d", i)) {
			if i == m.workspace.cursor && m.rowChosen {
				n, c := m.workspaceOpen()
				return n, c, true
			}
			m.workspace.cursor = i
			m.rowChosen = true
			return m, nil, true
		}
	}
	return m, nil, false
}

func (m Model) workspaceSetRange(r Range) (Model, tea.Cmd) {
	if m.rng == r && m.step == 0 {
		return m, nil
	}
	m.rng, m.step = r, 0
	m.scrubPinned = false
	m.syncStepKeys()
	m.persistUI()
	cmd := m.startLoadAfter(rangeLoadSettle)
	// A previously visited range can apply from the current cache snapshot on
	// this turn. No database I/O is allowed in this optimistic cache-only path.
	next := m
	next.err, next.detailWanted = nil, false
	next.loadOverviewSnapshot(true)
	if next.err == nil && !next.detailWanted {
		next.flight.stop()
		next.fresh, next.dataGen = FreshLive, next.loadGen
		return next, nil
	}
	return m, cmd
}

// A late code response cannot overwrite another inspector or a newer dataset.
type workspaceCodeMsg struct {
	gen, seq    uint64
	summary     store.CodeChangeSummary
	err         error
	allProjects bool
}

func (m Model) handleWorkspaceCode(msg workspaceCodeMsg) (Model, tea.Cmd) {
	if msg.gen != m.loadGen || msg.seq != m.workspace.codeSeq || m.workspace.overlay != "Selected usage" {
		return m, nil
	}
	text := "Recorded session lines unavailable: no known source snapshots."
	if msg.err != nil {
		text = "Recorded session lines unavailable: source query failed. Refresh to retry."
	} else if msg.summary.KnownChanges > 0 {
		text = fmt.Sprintf("Recorded session lines · Lifetime\n+%d added / -%d removed\n%d known snapshots; %d unknown snapshots", msg.summary.LinesAdded, msg.summary.LinesRemoved, msg.summary.KnownChanges, msg.summary.UnknownChanges)
		if msg.summary.UnknownChanges > 0 {
			text += "\nPartial recorded coverage."
		}
	} else if msg.summary.UnknownChanges > 0 {
		text = fmt.Sprintf("Recorded session lines unavailable: %d snapshots have unknown counts.", msg.summary.UnknownChanges)
	}
	text += "\nSource-recorded change counts, not final diff lines. Independent of this model and selected period."
	if msg.allProjects {
		text += "\nLifetime query covers all projects recorded for this harness/session."
	}
	m.workspace.content = strings.Replace(m.workspace.content, "Recorded session lines · Lifetime\nLoading recorded counts…", text, 1)
	m.workspaceRelayout()
	return m, nil
}
