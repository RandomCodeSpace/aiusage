package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/internal/tui/views"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// byToolModel loads a By-Tool model over fakeData — whose tool rows are
// claude-code and codex, i.e. NO copilot usage in the range — with the given
// startup discovery result.
func byToolModel(t *testing.T, sources map[string]int) Model {
	t.Helper()
	m := NewModel(&fakeData{}, Options{DBPath: "/tmp/usage.db", Sources: sources})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 44})
	return step(t, loadOnce(tm.(Model)), keyMsg("2"))
}

// TestCopilotFootnoteComesFromDiscoveryNotTheRange pins issue #44: whether a
// copilot data SOURCE exists is a fact about the machine, so the footnote reads
// it from adapter discovery. Deriving it from a zero total in the shown range
// told a user with telemetry enabled and data from last week that they had no
// data source at all, while they were looking at Today. Every case below has an
// empty copilot range — only the discovery result differs.
func TestCopilotFootnoteComesFromDiscoveryNotTheRange(t *testing.T) {
	cases := []struct {
		name    string
		sources map[string]int
		want    views.CopilotSourceState
		text    string // expected footnote fragment; "" = the view must say nothing
	}{
		{"discovery found no source", map[string]int{model.ToolCopilot: 0}, views.CopilotNoSource, "configured, no data source"},
		{"source exists, range empty", map[string]int{model.ToolCopilot: 2}, views.CopilotIdle, "no usage in this range"},
		{"discovery unavailable", nil, views.CopilotUnknown, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := byToolModel(t, c.sources)
			if m.byTool.Copilot != c.want {
				t.Fatalf("copilot state = %d, want %d", m.byTool.Copilot, c.want)
			}
			frame := ansiWindow.ReplaceAllString(m.View().Content, "")
			if c.text == "" {
				if strings.Contains(frame, "copilot:") {
					t.Fatal("the view made a claim about copilot with no discovery result behind it")
				}
				return
			}
			if !strings.Contains(frame, c.text) {
				t.Fatalf("frame does not carry the footnote %q", c.text)
			}
			if c.want == views.CopilotIdle && strings.Contains(frame, "no data source") {
				t.Fatal("a discovered copilot source is still reported as absent")
			}
		})
	}
}

// TestCopilotFootnoteSilentWhenUsageIsPresent: a source that produced usage in
// the range needs no footnote — the bar itself is the statement.
func TestCopilotFootnoteSilentWhenUsageIsPresent(t *testing.T) {
	m := byToolModel(t, map[string]int{model.ToolCopilot: 1})
	rows := append(append([]store.Bucket{}, m.byTool.Rows...), store.Bucket{
		Keys:        map[string]string{"tool": model.ToolCopilot},
		OrderedKeys: []string{"tool"},
		Events:      3,
		Input:       1_000,
		Output:      500,
		Total:       1_500,
	})
	if got := m.copilotState(rows); got != views.CopilotActive {
		t.Fatalf("copilot state with usage in range = %d, want %d", got, views.CopilotActive)
	}

	d := m.byTool
	d.Rows = rows
	d.Copilot = views.CopilotActive
	frame := ansiWindow.ReplaceAllString(views.ByTool(m.vctx, d, m.bodyLayout()), "")
	if strings.Contains(frame, "copilot:") {
		t.Fatal("an active copilot source still renders a footnote")
	}
}
