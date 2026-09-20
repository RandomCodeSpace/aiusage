package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestWorkspaceControlsVisibleBeforeInteraction(t *testing.T) {
	for _, size := range [][2]int{{42, 12}, {42, 16}, {42, 24}, {80, 24}, {120, 40}, {200, 60}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			_, m := newWorkspaceModel(t)
			m = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			frame := plainFrame(m)
			for _, label := range []string{"Range", "Group", "Sort", "Chart", "Filter rows", "[Models]", "[Cost]", "Tokens", "Events", "Name"} {
				if !strings.Contains(frame, label) {
					t.Fatalf("initial frame omitted %q:\n%s", label, frame)
				}
			}
			for _, kind := range []string{"Group", "Sort", "Chart"} {
				for _, action := range workspaceChoiceActions(kind) {
					if z := resolveZone(m, workspaceChoiceZone(action.action)); z == nil || z.IsZero() {
						t.Fatalf("initial %s option %s has no mouse target", kind, action.label)
					}
				}
			}
			if strings.Contains(frame, "▾") {
				t.Fatal("workspace still presents a dropdown")
			}
			if size[0] >= 80 && !strings.Contains(frame, "gpt-5") {
				t.Fatalf("normal terminal lost the comparison table:\n%s", frame)
			}
			if lipgloss.Height(frame) > size[1] || lipgloss.Width(frame) > size[0] {
				t.Fatal("persistent controls overflowed the terminal")
			}
		})
	}
}

func TestWorkspaceDirectControlsPreserveIndependentState(t *testing.T) {
	f, m := newWorkspaceModel(t)
	identity := selectedWorkspaceIdentity(t, m)
	m = mustPress(t, m, workspaceChoiceZone(fmt.Sprintf("sort:%d", SortName)), tea.MouseLeft)
	if m.sort != SortName || selectedWorkspaceIdentity(t, m) != identity || m.workspace.chooser != "" {
		t.Fatal("one-click sort changed selection or opened a chooser")
	}
	m = mustPress(t, m, workspaceChoiceZone("chart:Input"), tea.MouseLeft)
	if m.workspace.chart != UsageMetricInput || m.sort != SortName || selectedWorkspaceIdentity(t, m) != identity {
		t.Fatal("one-click chart changed another comparison control")
	}
	before, gen := f.queries(), m.loadGen
	m = mustPress(t, m, workspaceChoiceZone("group:model"), tea.MouseLeft)
	if m.loadGen != gen || f.queries() != before || len(m.workspace.history) != 0 {
		t.Fatal("applied group click dispatched another load or history entry")
	}
	m = mustPress(t, m, workspaceChoiceZone("group:tool"), tea.MouseLeft)
	if m.workspaceGroup() != "tool" || m.sort != SortName || m.workspace.chart != UsageMetricInput {
		t.Fatal("one-click group failed or changed sort/chart")
	}
}

func TestWorkspaceControlFocusKeepsComparisonGeometry(t *testing.T) {
	for _, size := range [][2]int{{42, 12}, {51, 24}, {52, 24}, {53, 24}, {80, 24}, {120, 40}} {
		f, m := newWorkspaceModel(t)
		m = send(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		height := lipgloss.Height(m.workspaceControls(size[0] - 2))
		for _, key := range []string{"o", "s", "c"} {
			m, _ = workspaceUI(t, f, m, keyMsg(key))
			if got := lipgloss.Height(m.workspaceControls(size[0] - 2)); got != height {
				t.Fatalf("%dx%d %s focus moved content: %d control rows, want %d", size[0], size[1], key, got, height)
			}
			m, _ = workspaceUI(t, f, m, keyMsg("esc"))
		}
	}
}

func TestWorkspacePersistentFilterCanApplyCancelAndClear(t *testing.T) {
	f, m := newWorkspaceModel(t)
	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	totals := m.overview.Totals
	m = mustPress(t, m, "workspace-filter", tea.MouseLeft)
	for _, r := range "no-such-model" {
		m, _ = workspaceUI(t, f, m, keyMsg(string(r)))
	}
	if !strings.Contains(plainFrame(m), "Filter rows / no-such-model") {
		t.Fatal("typing did not stay in the visible filter row")
	}
	m, cmd := workspaceUI(t, f, m, keyMsg("enter"))
	m = workspaceApply(t, f, m, cmd)
	if len(m.workspace.rows) != 0 || !strings.Contains(plainFrame(m), "No matching usage") {
		t.Fatal("filter did not show its no-match state")
	}
	if m.overview.Totals.Input != totals.Input || m.overview.Totals.CostMicroUSD != totals.CostMicroUSD {
		t.Fatal("comparison filter changed full-scope totals")
	}
	m = mustPress(t, m, "workspace-filter-clear", tea.MouseLeft)
	if m.filter != "" || len(m.workspace.rows) != 2 || !strings.Contains(plainFrame(m), "all rows") {
		t.Fatal("one-click clear did not restore the comparison")
	}
	m = mustPress(t, m, "workspace-filter", tea.MouseLeft)
	m, _ = workspaceUI(t, f, m, keyMsg("x"))
	m = mustPress(t, m, workspaceChoiceZone("chart:Output"), tea.MouseLeft)
	if m.filtering || m.filter != "" || m.workspace.chart != UsageMetricOutput {
		t.Fatal("another visible control did not release filter focus")
	}
}

func TestWorkspaceMinimumTerminalComparisonIsReachable(t *testing.T) {
	_, m := newWorkspaceModel(t)
	m = send(m, tea.WindowSizeMsg{Width: 42, Height: 12})
	m = mustPress(t, m, "workspace-compare", tea.MouseLeft)
	if m.workspace.overlay != "Comparison" || len(m.workspace.menu) != 2 {
		t.Fatal("compact comparison affordance did not expose the rows")
	}
	if !strings.Contains(plainFrame(m), "gpt-5") {
		t.Fatal("compact comparison omitted usage rows")
	}
}
