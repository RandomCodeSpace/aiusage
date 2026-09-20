package views

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	zone "github.com/lrstanley/bubblezone/v2"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

var workspaceANSI = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

func workspaceTestContext() Ctx {
	return Ctx{
		Humanize: func(n int64) string { return strconv.FormatInt(n, 10) },
		Money:    model.FormatCost,
		Comp: []CompSpec{
			{Key: "input", Color: lipgloss.Color("2")},
			{Key: "output", Color: lipgloss.Color("4")},
			{Key: "cache", Color: lipgloss.Color("1")},
		},
	}
}

func TestWorkspaceCacheUsesCombinedComponentStyle(t *testing.T) {
	c := workspaceTestContext()
	want := c.Comp[2].Color
	if got := workspaceCompStyle(c, "cache").GetForeground(); got != want {
		t.Fatalf("cache style foreground = %v, want combined cache color %v", got, want)
	}
}

func TestWorkspaceSummaryTypographyUsesMetricAndSecondaryStyles(t *testing.T) {
	c := workspaceTestContext()
	metric := workspaceMetricStyle(workspaceCompStyle(c, "input"))
	if !metric.GetBold() || metric.GetForeground() != c.Comp[0].Color {
		t.Fatalf("metric style = bold %v foreground %v", metric.GetBold(), metric.GetForeground())
	}
	if !workspaceSecondaryStyle(c).GetItalic() {
		t.Fatal("secondary summary text is not italic")
	}
}

func TestWorkspaceCacheOverflowKeepsCombinedMagnitude(t *testing.T) {
	c := workspaceTestContext()
	plain := workspaceANSI.ReplaceAllString(workspaceCompactSummary(c, store.Bucket{
		CacheRead: math.MaxInt64, CacheCreation: math.MaxInt64,
	}, "$0.00", 42), "")
	if want := "Cache 18446744.1T"; !strings.Contains(plain, want) {
		t.Fatalf("overflowing combined cache missing %q:\n%s", want, plain)
	}
}

func TestWorkspaceGeometryKeepsNarrowValuesAndDetail(t *testing.T) {
	g := WorkspaceGeometry(42, 24, 3)
	if g.Side {
		t.Fatal("42-column workspace unexpectedly allocated a side column")
	}
	if g.SummaryH != 11 || g.MachineH != 1 || g.BodyH != 12 {
		t.Fatalf("top/body allocation = summary %d, machine %d, body %d", g.SummaryH, g.MachineH, g.BodyH)
	}
	if g.TableW != 42 || g.DetailW != 42 || g.TableH <= 0 || g.DetailH <= 0 {
		t.Fatalf("narrow pane geometry = %+v", g)
	}
	if g.TableH+workspaceFrame+workspaceGap+g.DetailH+workspaceFrame != g.BodyH {
		t.Fatalf("left regions do not fill body: %+v", g)
	}
}

func TestWorkspaceGeometryAddsBoundedWideSide(t *testing.T) {
	g := WorkspaceGeometry(120, 40, 2)
	if !g.Side {
		t.Fatal("120x40 workspace did not allocate its side column")
	}
	if g.TableH != 4 { // control/header plus two rows; border is outside it
		t.Fatalf("short model table grew past its content: height %d", g.TableH)
	}
	if g.TableW+workspaceFrame+workspaceGap+g.ChartW+workspaceFrame != 120 {
		t.Fatalf("columns do not fill viewport: %+v", g)
	}
	if g.ChartH+workspaceFrame+workspaceGap+g.SuggestionH+workspaceFrame != g.BodyH {
		t.Fatalf("side regions do not fill body: %+v", g)
	}
}

func TestWorkspaceCompactGeometryPreservesOneModelRow(t *testing.T) {
	g := WorkspaceGeometry(42, 12, 8)
	if g.SummaryH != 3 || g.MachineH != 1 || g.BodyH != 8 {
		t.Fatalf("compact allocation = summary %d, machine %d, body %d", g.SummaryH, g.MachineH, g.BodyH)
	}
	if g.TableW != 42 || g.TableH != 8 || g.DetailH != 0 {
		t.Fatalf("compact table allocation = %+v", g)
	}
}

func TestWorkspaceCompactFrameShowsMetricsMachineAndTable(t *testing.T) {
	c := workspaceTestContext()
	d := WorkspaceData{
		Totals:   store.Bucket{Input: 12, Output: 3, CacheRead: 4, CostMicroUSD: 50_000, Events: 1},
		RowCount: 1,
		Machine:  "Machine CPU 12% memory 44% disk 31%",
		Table:    "Model · Sort: Cost\nModel  Cost\nmodel-a  $0.05",
	}
	out := Workspace(c, d, 42, 12)
	if got := lipgloss.Width(out); got != 42 {
		t.Fatalf("compact workspace width = %d, want 42", got)
	}
	if got := lipgloss.Height(out); got != 12 {
		t.Fatalf("compact workspace height = %d, want 12", got)
	}
	plain := workspaceANSI.ReplaceAllString(out, "")
	for _, want := range []string{"Input", "Output", "Cache", "Cost", "CPU 12%", "memory 44%", "disk 31%", "Model · Sort: Cost", "model-a"} {
		if !strings.Contains(plain, want) {
			t.Errorf("compact workspace omitted %q:\n%s", want, plain)
		}
	}
}

func TestWorkspaceRendersExactViewportAtFortyTwoColumns(t *testing.T) {
	c := workspaceTestContext()
	d := WorkspaceData{
		Totals: store.Bucket{
			Events:             6,
			Input:              1200,
			Output:             340,
			CacheRead:          90,
			CacheCreation:      10,
			CostMicroUSD:       2_000_000,
			ComputedCostEvents: 2,
			UnpricedEvents:     1,
		},
		RowCount: 2,
		Table:    "Model · 1 of 2 · Sort: Cost\nheader\nmodel-a\nmodel-b",
		Detail:   "Selected model-a\nUsage details",
		Machine:  "Machine CPU 12% memory 44% disk 31%",
	}

	out := Workspace(c, d, 42, 24)
	if got := lipgloss.Width(out); got != 42 {
		t.Fatalf("workspace width = %d, want 42", got)
	}
	if got := lipgloss.Height(out); got != 24 {
		t.Fatalf("workspace height = %d, want 24", got)
	}
	plain := workspaceANSI.ReplaceAllString(out, "")
	for _, want := range []string{"Input", "Output", "Cache", "Cost", "3 reported events", "2 computed events", "1 unpriced event", "Machine", "model-a", "Selected model-a"} {
		if !strings.Contains(plain, want) {
			t.Errorf("42-column workspace omitted %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "request") {
		t.Fatalf("cost coverage used request accounting:\n%s", plain)
	}
}

func TestWorkspaceCostDistinguishesUnknownFromMeasuredZero(t *testing.T) {
	c := workspaceTestContext()
	unknown := strings.Join(workspaceCostLines(c, store.Bucket{Events: 4, UnpricedEvents: 4}), "\n")
	if strings.Contains(unknown, "$0.00") || !strings.Contains(unknown, "4 unpriced events") {
		t.Fatalf("all-unpriced cost = %q", unknown)
	}

	zero := strings.Join(workspaceCostLines(c, store.Bucket{Events: 4}), "\n")
	if !strings.Contains(zero, "$0.00") || !strings.Contains(zero, "4 reported events") {
		t.Fatalf("known zero cost = %q", zero)
	}
}

func TestWorkspaceKeepsNoDataCostCompleteAtFortyTwoColumns(t *testing.T) {
	c := workspaceTestContext()
	for _, tc := range []struct {
		name   string
		totals store.Bucket
		want   []string
	}{
		{name: "empty range", want: []string{"Cost", "No usage", "0 events"}},
		{name: "all unpriced", totals: store.Bucket{Events: 4, UnpricedEvents: 4}, want: []string{"Cost", "-", "4 unpriced events"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := workspaceANSI.ReplaceAllString(Workspace(c, WorkspaceData{Totals: tc.totals}, 42, 24), "")
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing complete cost text %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestWorkspaceExpandedSummaryUsesBoundedBlockTrends(t *testing.T) {
	c := workspaceTestContext()
	timeline := []store.Bucket{
		{Input: 10, Output: 4, CacheRead: 2, CacheCreation: 1, CostMicroUSD: 10_000, Events: 1},
		{Input: 30, Output: 8, CacheRead: 12, CacheCreation: 3, CostMicroUSD: 40_000, Events: 1},
		{Input: 18, Output: 6, CacheRead: 7, CacheCreation: 2, CostMicroUSD: 25_000, Events: 1},
	}
	d := WorkspaceData{
		Totals:   store.Bucket{Input: 58, Output: 18, CacheRead: 21, CacheCreation: 6, CostMicroUSD: 75_000, Events: 3},
		Timeline: timeline,
		Machine:  "CPU 12%\nmemory 44%\ndisk 31%",
	}

	for _, width := range []int{20, 42, 83, 84, 120} {
		out := Workspace(c, d, width, 40)
		if got := lipgloss.Width(out); got != width {
			t.Errorf("width %d: rendered width = %d", width, got)
		}
		if got := lipgloss.Height(out); got != 40 {
			t.Errorf("width %d: rendered height = %d", width, got)
		}
	}

	plain := workspaceANSI.ReplaceAllString(Workspace(c, d, 120, 40), "")
	if got := strings.Count(plain, "Trend · range"); got != 4 {
		t.Fatalf("expanded summary labeled %d trends, want 4:\n%s", got, plain)
	}
	if workspaceBlockCount(plain) == 0 {
		t.Fatalf("expanded summary did not render ntcharts block sparklines:\n%s", plain)
	}
}

func TestWorkspaceCompactSummaryOmitsTrends(t *testing.T) {
	c := workspaceTestContext()
	d := WorkspaceData{
		Totals:   store.Bucket{Input: 12, Output: 3, CacheRead: 4, CostMicroUSD: 50_000, Events: 1},
		Timeline: []store.Bucket{{Input: 5, Output: 1, CacheRead: 2, CostMicroUSD: 20_000, Events: 1}, {Input: 7, Output: 2, CacheRead: 2, CostMicroUSD: 30_000, Events: 1}},
		RowCount: 1,
		Machine:  "CPU 12%\nmemory 44%\ndisk 31%",
		Table:    "Model · Sort: Cost\nModel  Cost\nmodel-a  $0.05",
	}
	plain := workspaceANSI.ReplaceAllString(Workspace(c, d, 42, 12), "")
	if strings.Contains(plain, "Trend ·") || workspaceBlockCount(plain) != 0 {
		t.Fatalf("compact summary spent rows on trends:\n%s", plain)
	}
}

func TestWorkspaceAllUnpricedCostHasNoFakeZeroTrend(t *testing.T) {
	c := workspaceTestContext()
	timeline := []store.Bucket{
		{Input: 10, Events: 1, UnpricedEvents: 1},
		{Input: 20, Events: 1, UnpricedEvents: 1},
	}
	trend := workspaceKnownCostTrend(timeline)
	if trend.Available {
		t.Fatal("all-unpriced cost history reported an available numeric trend")
	}
	d := WorkspaceData{
		Totals:   store.Bucket{Input: 30, Events: 2, UnpricedEvents: 2},
		Timeline: timeline,
		Machine:  "CPU 12%\nmemory 44%\ndisk 31%",
	}
	plain := workspaceANSI.ReplaceAllString(Workspace(c, d, 120, 40), "")
	if strings.Contains(plain, "$0.00") {
		t.Fatalf("all-unpriced cost rendered a fake zero:\n%s", plain)
	}
	if !strings.Contains(plain, "Trend · unavailable") {
		t.Fatalf("all-unpriced cost omitted unavailable trend state:\n%s", plain)
	}
}

func TestWorkspaceTrendValuesFollowMetricAccounting(t *testing.T) {
	timeline := []store.Bucket{
		{Input: 11, Output: 7, CacheRead: 13, CacheCreation: 5, CostMicroUSD: 90_000, Events: 2, UnpricedEvents: 1},
		{Input: 19, Output: 3, CacheRead: 17, CacheCreation: 2, Events: 1, UnpricedEvents: 1},
		{Input: 23, Output: 9, CacheRead: 4, CacheCreation: 6, Events: 1},
	}
	input := workspaceTokenTrend(timeline, func(b store.Bucket) int64 { return b.Input })
	output := workspaceTokenTrend(timeline, func(b store.Bucket) int64 { return b.Output })
	cache := workspaceCacheTrend(timeline)
	cost := workspaceKnownCostTrend(timeline)

	if got := input.Values; len(got) != 3 || got[0] != 11 || got[1] != 19 || got[2] != 23 {
		t.Fatalf("input trend = %v", got)
	}
	if got := output.Values; len(got) != 3 || got[0] != 7 || got[1] != 3 || got[2] != 9 {
		t.Fatalf("output trend = %v", got)
	}
	if got := cache.Values; len(got) != 3 || got[0] != 18 || got[1] != 19 || got[2] != 10 {
		t.Fatalf("cache trend = %v, want read + creation", got)
	}
	if got := cost.Values; !cost.Available || cost.Label != "Trend · priced" || len(got) != 2 || got[0] != 90_000 || got[1] != 0 {
		t.Fatalf("known cost trend = label %q values %v available %v", cost.Label, got, cost.Available)
	}
}

func TestWorkspaceCacheTrendDoesNotSaturateAtInt64(t *testing.T) {
	trend := workspaceCacheTrend([]store.Bucket{{CacheRead: math.MaxInt64, CacheCreation: math.MaxInt64}})
	if len(trend.Values) != 1 || trend.Values[0] <= float64(math.MaxInt64) {
		t.Fatalf("overflowing cache trend = %v, want combined floating-point magnitude", trend.Values)
	}
}

func workspaceBlockCount(s string) int {
	count := 0
	for _, r := range s {
		if r >= '\u2581' && r <= '\u2588' {
			count++
		}
	}
	return count
}

func TestWorkspaceFitClipsPadsAndPreservesANSI(t *testing.T) {
	styled := lipgloss.NewStyle().Bold(true).Render("界abc")
	out := workspaceFit(styled+"\nsecond\nignored", 4, 2)
	if got := lipgloss.Width(out); got != 4 {
		t.Fatalf("fitted width = %d, want 4", got)
	}
	if got := lipgloss.Height(out); got != 2 {
		t.Fatalf("fitted height = %d, want 2", got)
	}
	plain := workspaceANSI.ReplaceAllString(out, "")
	if !strings.HasPrefix(plain, "界ab\nseco") {
		t.Fatalf("ANSI-aware clamp = %q", plain)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("ANSI styling was discarded: %q", out)
	}
}

func TestWorkspaceFitPreservesZoneMarkersWhenClipping(t *testing.T) {
	zm := zone.New()
	t.Cleanup(zm.Close)
	marked := zm.Mark("summary", "abcdef")
	marker := marked[:strings.Index(marked, "a")]
	out := workspaceFit(marked, 4, 1)
	if got := strings.Count(out, marker); got != 2 {
		t.Fatalf("clipped zone has %d boundary markers, want 2: %q", got, out)
	}
}
