package tui

import (
	"os"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

type productionView struct {
	name  string
	view  View
	pivot ActivityPivot
}

func productionViews() []productionView {
	views := []productionView{
		{name: "Overview", view: ViewOverview, pivot: PivotCalls},
		{name: "ByTool", view: ViewByTool, pivot: PivotCalls},
		{name: "ByModel", view: ViewByModel, pivot: PivotCalls},
		{name: "Sessions", view: ViewBrowse, pivot: PivotCalls},
		{name: "ActivityCalls", view: ViewActivity, pivot: PivotCalls},
	}
	for _, dimension := range model.TurnDimensions() {
		views = append(views, productionView{
			name:  "Activity" + camelDimension(string(dimension)),
			view:  ViewActivity,
			pivot: ActivityPivot(dimension),
		})
	}
	return views
}

func camelDimension(value string) string {
	out := make([]byte, 0, len(value))
	upper := true
	for i := range len(value) {
		if value[i] == '_' {
			upper = true
			continue
		}
		c := value[i]
		if upper && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
		upper = false
	}
	return string(out)
}

func productionModel(src DataSource, width, height int, target productionView) Model {
	m := NewModel(src, Options{DBPath: "/fixture/usage.db"})
	fixed := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	m.data.now = func() time.Time { return fixed }
	m.loadNow = fixed
	m.view = target.view
	m.pivot = target.pivot
	tm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return loadOnce(tm.(Model))
}

func benchmarkProductionRender(b *testing.B, width, height int) {
	for _, target := range productionViews() {
		b.Run(target.name, func(b *testing.B) {
			m := productionModel(&fakeData{}, width, height, target)
			b.ReportAllocs()
			for b.Loop() {
				benchFrame = m.View().Content
			}
		})
	}
}

func BenchmarkProductionRender120x40(b *testing.B) { benchmarkProductionRender(b, 120, 40) }
func BenchmarkProductionRender200x60(b *testing.B) { benchmarkProductionRender(b, 200, 60) }

// BenchmarkProductionColdLoad measures every view and Activity partition over
// long-ledger-v1. It skips outside the performance job, where the deterministic
// database path is provided explicitly.
func BenchmarkProductionColdLoad(b *testing.B) {
	path := os.Getenv("AIUSAGE_PERF_DB")
	if path == "" {
		b.Skip("AIUSAGE_PERF_DB is not set")
	}
	reader, err := store.OpenReadOnly(path)
	if err != nil {
		b.Fatalf("open long ledger: %v", err)
	}
	b.Cleanup(func() { reader.Close() })
	for _, target := range productionViews() {
		b.Run(target.name, func(b *testing.B) {
			src := &countingSource{src: reader}
			var queries, queryNanos int64
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				beforeQueries, beforeNanos := src.sample()
				_ = productionModel(src, 120, 40, target)
				afterQueries, afterNanos := src.sample()
				queries += afterQueries - beforeQueries
				queryNanos += afterNanos - beforeNanos
			}
			reportQueryCost(b, queries, queryNanos)
		})
	}
}

// BenchmarkProductionUIThread covers representative input classes up to the
// point where Bubble Tea would render. Returned background commands are not
// executed; issuing those commands must itself stay query-free and cheap.
func BenchmarkProductionUIThread(b *testing.B) {
	target := productionView{name: "Overview", view: ViewOverview, pivot: PivotCalls}
	src := &countingSource{src: &fakeData{}}
	base := productionModel(src, 120, 40, target)
	filtering := base
	tm, _ := filtering.Update(keyMsg("/"))
	filtering = tm.(Model)
	cases := []struct {
		name  string
		model Model
		msg   tea.Msg
	}{
		{name: "Key", model: base, msg: keyMsg("?")},
		{name: "Mouse", model: base, msg: tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 1}},
		{name: "Resize", model: base, msg: tea.WindowSizeMsg{Width: 121, Height: 41}},
		{name: "Filter", model: filtering, msg: keyMsg("x")},
		{name: "Scrub", model: base, msg: keyMsg("right")},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			before, _ := src.sample()
			b.ReportAllocs()
			for b.Loop() {
				_, _ = tc.model.Update(tc.msg)
			}
			after, _ := src.sample()
			b.ReportMetric(float64(after-before)/float64(b.N), "queries/op")
		})
	}
}
