package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

func workspaceSessionDetailModel(t *testing.T, src DataSource) Model {
	t.Helper()
	m := newTestModel(t, src)
	m.workspace.group, m.workspace.appliedGroup = "session", "session"
	m.workspace.rows = []store.Bucket{{Keys: map[string]string{"session": "selected-session", "tool": "codex", "project": "/selected/project"}, Events: 1, Input: 10, Total: 10}}
	m.workspace.cursor = 0
	m.workspace.appliedScope = []Crumb{{Dim: "model", Value: "selected-model"}}
	return m
}

func TestWorkspaceDetailsCodeQueryDeferredAndTruthful(t *testing.T) {
	cases := []struct {
		name    string
		summary store.CodeChangeSummary
		err     error
		want    []string
		absent  string
	}{
		{name: "known zero", summary: store.CodeChangeSummary{KnownChanges: 1}, want: []string{"+0 added / -0 removed", "1 known snapshots; 0 unknown snapshots"}, absent: "unavailable"},
		{name: "partial", summary: store.CodeChangeSummary{KnownChanges: 2, UnknownChanges: 3, LinesAdded: 19, LinesRemoved: 7}, want: []string{"+19 added / -7 removed", "2 known snapshots; 3 unknown snapshots", "Partial recorded coverage."}, absent: "unavailable"},
		{name: "unknown", summary: store.CodeChangeSummary{UnknownChanges: 3}, want: []string{"Recorded session lines unavailable: 3 snapshots have unknown counts."}, absent: "+0 added"},
		{name: "empty", want: []string{"Recorded session lines unavailable: no known source snapshots."}, absent: "+0 added"},
		{name: "failed", err: errors.New("fixture source down"), want: []string{"Recorded session lines unavailable: source query failed."}, absent: "+0 added"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var identity [3]string
			src := &codeChangesData{read: func(_ context.Context, tool, session, project string) (store.CodeChangeSummary, error) {
				identity = [3]string{tool, session, project}
				return tt.summary, tt.err
			}}
			m := workspaceSessionDetailModel(t, src)
			var cmd tea.Cmd
			if n := queriesDuring(&src.fakeData, func() { m, cmd = m.workspaceDetails(); _ = m.View() }); n != 0 || src.codeCalls.Load() != 0 {
				t.Fatal("opening or rendering details queried the source")
			}
			if cmd == nil || !strings.Contains(m.workspace.content, "Loading recorded counts") {
				t.Fatal("exact-session details did not dispatch a pending query")
			}
			msg := cmd()
			if src.codeCalls.Load() != 1 || identity != [3]string{"codex", "selected-session", "/selected/project"} {
				t.Fatalf("code calls=%d identity=%q", src.codeCalls.Load(), identity)
			}
			if n := queriesDuring(&src.fakeData, func() { m = send(m, msg) }); n != 0 || src.codeCalls.Load() != 1 {
				t.Fatal("applying recorded counts queried the source")
			}
			// Check the code section separately from token/cost reporting limits.
			start := strings.LastIndex(m.workspace.content, "Recorded session lines")
			if start < 0 {
				t.Fatal("missing recorded-count section")
			}
			text := m.workspace.content[start:]
			for _, want := range append(tt.want, "Source-recorded change counts, not final diff lines.", "Independent of this model and selected period.") {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in:\n%s", want, text)
				}
			}
			if strings.Contains(text, tt.absent) || strings.Contains(text, "Loading recorded counts") {
				t.Errorf("incorrect result text:\n%s", text)
			}
		})
	}
}

func TestWorkspaceDetailsCodeRejectsLateResponses(t *testing.T) {
	for _, transition := range []string{"dismiss", "reopen", "different inspector", "superseded generation"} {
		t.Run(transition, func(t *testing.T) {
			src := &codeChangesData{read: func(context.Context, string, string, string) (store.CodeChangeSummary, error) {
				return store.CodeChangeSummary{KnownChanges: 1, LinesAdded: 987654}, nil
			}}
			m := workspaceSessionDetailModel(t, src)
			m, cmd := m.workspaceDetails()
			if cmd == nil {
				t.Fatal("missing session command")
			}
			// The response is ready, but delivery occurs after the UI changes.
			late := cmd()
			switch transition {
			case "dismiss":
				m = send(m, keyMsg("esc"))
			case "reopen":
				m = send(m, keyMsg("esc"))
				m, _ = m.workspaceDetails()
			case "different inspector":
				m, _ = m.workspaceAction("metric:Input")
			case "superseded generation":
				_ = m.startLoad()
			}
			content, overlay := m.workspace.content, m.workspace.overlay
			m = send(m, late)
			if m.workspace.content != content || m.workspace.overlay != overlay {
				t.Fatalf("late response changed %s inspector:\n%s", transition, m.workspace.content)
			}
			if src.codeCalls.Load() != 1 {
				t.Fatal("late response caused a source query")
			}
		})
	}
}

func TestWorkspaceDetailsRequireExactSessionAndLabelAllProjects(t *testing.T) {
	src := &codeChangesData{read: func(_ context.Context, tool, session, project string) (store.CodeChangeSummary, error) {
		if tool != "codex" || session != "selected-session" || project != "" {
			t.Fatalf("unexpected lifetime identity %q/%q/%q", tool, session, project)
		}
		return store.CodeChangeSummary{KnownChanges: 1}, nil
	}}
	m := workspaceSessionDetailModel(t, src)
	m.workspace.rows[0].Keys["project"] = ""
	m, cmd := m.workspaceDetails()
	if cmd == nil {
		t.Fatal("missing lifetime query")
	}
	m = send(m, cmd())
	if !strings.Contains(m.workspace.content, "Lifetime query covers all projects recorded for this harness/session.") {
		t.Fatal("empty-project lifetime scope was not disclosed")
	}
	for _, missing := range []string{"tool", "session"} {
		m := workspaceSessionDetailModel(t, src)
		m.workspace.rows[0].Keys[missing] = ""
		m, cmd := m.workspaceDetails()
		if cmd != nil || !strings.Contains(m.workspace.content, "exact harness and session are required") {
			t.Fatalf("missing %s should be unavailable without a query", missing)
		}
	}
	if src.codeCalls.Load() != 1 {
		t.Fatal("incomplete identity queried source")
	}
}

func TestWorkspaceDetailsResizeScrollReachesWrappedEnd(t *testing.T) {
	src := &fakeData{}
	m := newTestModelWH(t, src, 120, 40)
	m.openWorkspaceText("Long inspector", strings.Repeat("Evidence remains available after wrapping and resizing. ", 100)+"\nEND-OF-EVIDENCE")
	before := m.workspace.viewport.TotalLineCount()
	if n := queriesDuring(src, func() {
		m = send(m, tea.WindowSizeMsg{Width: 42, Height: 18})
	}); n != 0 {
		t.Fatal("resizing the inspector queried the source")
	}
	if m.workspace.viewport.TotalLineCount() <= before {
		t.Fatal("narrow resize did not rewrap the stored viewport content")
	}
	for i := 0; i < m.workspace.viewport.TotalLineCount(); i++ {
		m = send(m, keyMsg("f"))
	}
	if !strings.Contains(plainFrame(m), "END-OF-EVIDENCE") {
		t.Fatalf("page-down cannot reach the wrapped inspector tail:\n%s", plainFrame(m))
	}
	m = send(m, tea.WindowSizeMsg{Width: 90, Height: 28})
	for i := 0; i < m.workspace.viewport.TotalLineCount(); i++ {
		m = send(m, keyMsg("f"))
	}
	if !strings.Contains(plainFrame(m), "END-OF-EVIDENCE") {
		t.Fatal("tail became unreachable after widening")
	}
}

func TestWorkspaceActivityBackRestoresCapturedPlace(t *testing.T) {
	m := workspaceSessionDetailModel(t, &fakeData{})
	m.workspace.appliedScope = []Crumb{{Dim: "tool", Value: "codex"}, {Dim: "model", Value: "selected-model"}}
	m.crumbs = append([]Crumb(nil), m.workspace.appliedScope...)
	m.filter = "selected"
	m.sort = SortName
	wantScope := append([]Crumb(nil), m.crumbs...)
	m, _ = m.workspaceAction("activity")
	if m.view != ViewActivity {
		t.Fatal("activity route did not open")
	}
	m = send(m, keyMsg("esc"))
	if m.view != ViewOverview || m.workspace.returnFromActivity || !reflect.DeepEqual(m.crumbs, wantScope) || m.filter != "selected" || m.sort != SortName {
		t.Fatalf("activity Back lost workspace place: view=%v scope=%v filter=%q sort=%v", m.view, m.crumbs, m.filter, m.sort)
	}
}
