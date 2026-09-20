package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RandomCodeSpace/aiusage/store"
)

func TestWorkspaceCacheColumnCompact(t *testing.T) {
	_, m := newWorkspaceModel(t)
	for _, tc := range []struct {
		read, write int64
		want        string
	}{
		{900, 20, "920"}, {1100, 100, "1.2K"}, {2_000_000, 500_000, "2.5M"}, {3_000_000_000, 100_000_000, "3.1B"},
	} {
		m.workspace.rows = []store.Bucket{{Keys: map[string]string{"model": "model"}, CacheRead: tc.read, CacheCreation: tc.write}}
		if got := m.workspaceTable(100, 5); !strings.Contains(got, tc.want) {
			t.Fatalf("cache %d+%d missing %s in %q", tc.read, tc.write, tc.want, got)
		}
	}
}

func TestWorkspaceValueSelectorsStayInline(t *testing.T) {
	for _, size := range [][2]int{{42, 12}, {42, 24}, {55, 52}, {120, 40}, {160, 50}} {
		for _, key := range []string{"o", "s", "c"} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], key), func(t *testing.T) {
				f, m := newWorkspaceModel(t)
				m = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				m, _ = workspaceUI(t, f, m, keyMsg(key))
				if m.workspace.overlay != "" {
					t.Fatalf("value chooser replaced data with %q screen", m.workspace.overlay)
				}
				frame := m.View().Content
				if lipgloss.Height(frame) > size[1] || lipgloss.Width(frame) > size[0] {
					t.Fatal("selector overflows viewport")
				}
				if !strings.Contains(frame, "Input") || !strings.Contains(frame, "Range") {
					t.Fatal("selector hides data or range")
				}
				for i := range m.workspace.menu {
					z := resolveZone(m, fmt.Sprintf("workspace-choice-%d", i))
					if z == nil || z.IsZero() {
						t.Fatalf("option %d has no mouse target", i)
					}
				}
				m, _ = workspaceUI(t, f, m, keyMsg("esc"))
				if m.workspace.menu != nil {
					t.Fatal("Esc did not close chooser")
				}
			})
		}
	}
}

func TestWorkspaceChoiceMouseAndKeyboardParity(t *testing.T) {
	f, m := newWorkspaceModel(t)
	m = send(m, tea.WindowSizeMsg{Width: 42, Height: 24})
	identity := selectedWorkspaceIdentity(t, m)
	m = mustPress(t, m, "workspace-menu-sort", tea.MouseLeft)
	m = mustPress(t, m, "workspace-choice-3", tea.MouseLeft)
	if m.sort != SortName || selectedWorkspaceIdentity(t, m) != identity || m.workspace.chooser != "" {
		t.Fatal("direct sort button changed selection or did not apply")
	}
	m, _ = workspaceUI(t, f, m, keyMsg("c"))
	m = mustPress(t, m, "workspace-choice-next", tea.MouseLeft)
	m = mustPress(t, m, "workspace-choice-apply", tea.MouseLeft)
	if m.workspace.chart != UsageMetricInput || m.sort != SortName {
		t.Fatal("chart focus/apply changed sort or chose the wrong metric")
	}
	m, _ = workspaceUI(t, f, m, keyMsg("o"))
	m = mustPress(t, m, "workspace-choice-2", tea.MouseLeft)
	if m.workspaceGroup() != "provider" || m.workspace.chart != UsageMetricInput || m.workspace.overlay != "" {
		t.Fatal("group button did not apply inline or changed the chart")
	}
	m, _ = workspaceUI(t, f, m, keyMsg("s"))
	if m.workspace.menuCursor != 3 {
		t.Fatal("chooser did not focus the applied value")
	}
	m = mustPress(t, m, "workspace-choice-close", tea.MouseLeft)
	if m.workspace.chooser != "" {
		t.Fatal("close button left chooser open")
	}
	m, _ = workspaceUI(t, f, m, keyMsg("s"))
	m, _ = workspaceUI(t, f, m, keyMsg("esc"))
	if m.workspace.chooser != "" {
		t.Fatal("Esc left control row focused")
	}
}

func TestWorkspaceRepeatedActionsHaveDistinctMouseTargets(t *testing.T) {
	for _, size := range [][2]int{{55, 52}, {120, 40}, {200, 60}} {
		for _, action := range []struct {
			zones   []string
			overlay string
		}{
			{[]string{"workspace-menu-more", "workspace-footer-more"}, "More"},
			{[]string{"workspace-details", "workspace-footer-details"}, "Selected usage"},
			{[]string{"workspace-open", "workspace-footer-open"}, ""},
			{[]string{"workspace-suggestions", "workspace-suggestion-card"}, "Suggestions"},
		} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], action.zones[0]), func(t *testing.T) {
				_, m := newWorkspaceModel(t)
				m = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				var locations [][2]int
				for _, id := range action.zones {
					if size[0] < 100 && id == "workspace-suggestion-card" {
						continue // The narrow layout has no side card.
					}
					x, y, ok := zoneCenter(m, id)
					if !ok {
						t.Fatalf("visible action %s has no mouse target", id)
					}
					if slices.Contains(locations, [2]int{x, y}) {
						t.Fatalf("%s shares another action's location", id)
					}
					locations = append(locations, [2]int{x, y})
					next := mustPress(t, m, id, tea.MouseLeft)
					if action.overlay == "" {
						if next.workspaceGroup() != "session" || len(next.crumbs) != 1 {
							t.Fatalf("%s did not drill into model sessions", id)
						}
					} else if next.workspace.overlay != action.overlay {
						t.Fatalf("%s opened %q, want %q", id, next.workspace.overlay, action.overlay)
					}
				}
			})
		}
	}
}

func TestCompactCacheSumDoesNotOverflow(t *testing.T) {
	if got := humanizeCache(store.Bucket{CacheRead: math.MaxInt64, CacheCreation: math.MaxInt64}); got != "18446744.1T" {
		t.Fatalf("large cache sum: %s", got)
	}
}

func TestWorkspaceTrendRetainedAcrossLiveSamplesAndSelection(t *testing.T) {
	_, m := newWorkspaceModel(t)
	m.workspace.chart = UsageMetricInput
	before := m.renderWorkspaceTrend(60, 20)
	builds := m.workspace.trendMemo.builds
	m.workspaceMove(1)
	m.sys = knownSnapshot(0.73)
	if got := m.renderWorkspaceTrend(60, 20); got != before || m.workspace.trendMemo.builds != builds {
		t.Fatal("selection or machine sample rebuilt the unchanged plot")
	}
	if frame := monoFrame(m); !strings.Contains(frame, "73%") {
		t.Fatal("retained plot hid the new machine reading")
	}
	for _, change := range []struct {
		name  string
		apply func(*Model)
		w, h  int
	}{
		{"resize", func(*Model) {}, 50, 15},
		{"metric", func(m *Model) { m.workspace.chart = UsageMetricOutput }, 50, 15},
		{"same-generation reapply", func(m *Model) {
			m.tlData.Buckets = slices.Clone(m.tlData.Buckets)
			m.tlData.Buckets[0].Output += 1_000_000
		}, 50, 15},
		{"generation", func(m *Model) { m.dataGen++ }, 50, 15},
		{"palette", func(m *Model) { m.th.Subtle = m.th.Subtle.Foreground(lipgloss.Color("5")) }, 50, 15},
		{"empty", func(m *Model) { m.tlData.Buckets = nil }, 50, 15},
	} {
		t.Run(change.name, func(t *testing.T) {
			builds := m.workspace.trendMemo.builds
			change.apply(&m)
			got := m.renderWorkspaceTrend(change.w, change.h)
			uncached := m
			uncached.workspace.trendMemo = nil
			if got != uncached.renderWorkspaceTrend(change.w, change.h) {
				t.Fatal("retained chart differs from current data/style/geometry")
			}
			if m.workspace.trendMemo.builds != builds+1 {
				t.Fatal("plot was not rebuilt for changed input")
			}
		})
	}
}

// A priced, populated workspace exercises the actual line chart and row movement.
func BenchmarkWorkspaceRender(b *testing.B) {
	for _, size := range [][2]int{{120, 40}, {200, 60}} {
		b.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(b *testing.B) {
			m := productionModel(&fakeData{}, size[0], size[1], productionView{view: ViewOverview})
			b.Cleanup(m.zoneMgr.Close)
			m.tlData.Dim = "hour"
			m.tlData.Buckets = nil
			for i := range 24 {
				m.tlData.Buckets = append(m.tlData.Buckets, store.Bucket{Keys: map[string]string{"hour": fmt.Sprintf("2026-08-30T%02d", i)}, Input: int64(12000 + i*1300), Output: int64(3000 + i*400), CacheRead: 90000, CostMicroUSD: int64(100000 + i*25000), Events: 10})
			}
			m.workspace.rows = nil
			for i := range 120 {
				m.workspace.rows = append(m.workspace.rows, store.Bucket{Keys: map[string]string{"model": fmt.Sprintf("model-%03d", i)}, Input: 1_100_000, Output: 240_000, CacheRead: 2_400_000, CacheCreation: 100_000, CostMicroUSD: 4000000, Events: 40})
			}
			if points, _, _, peak := workspaceTrendPoints(m.tlData, UsageMetricCost); len(points) != 24 || peak == 0 {
				b.Fatal("benchmark must draw a priced timeline")
			}
			b.ReportAllocs()
			for b.Loop() {
				m.workspace.cursor = (m.workspace.cursor + 1) % len(m.workspace.rows)
				benchFrame = m.View().Content
			}
		})
	}
}
