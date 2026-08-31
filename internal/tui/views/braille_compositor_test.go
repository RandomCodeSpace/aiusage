package views

import (
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"
)

func newCompositorModel() *timeserieslinechart.Model {
	m := timeserieslinechart.New(24, 8,
		timeserieslinechart.WithTimeRange(time.Unix(0, 0), time.Unix(23, 0)),
		timeserieslinechart.WithYRange(0, 10),
		timeserieslinechart.WithXYSteps(0, 0),
	)
	return &m
}

func compositorSeries() []paneSeriesData {
	return []paneSeriesData{
		{key: "input", rank: 0, style: lipgloss.NewStyle().Foreground(lipgloss.Color("2")), runs: [][]canvas.Float64Point{{
			{X: 0, Y: 2}, {X: 23, Y: 7},
		}}},
		{key: "output", rank: 1, style: lipgloss.NewStyle().Foreground(lipgloss.Color("4")), runs: [][]canvas.Float64Point{{
			{X: 0, Y: 2.35}, {X: 23, Y: 7.35},
		}}},
		{key: "cache", rank: 2, style: lipgloss.NewStyle().Foreground(lipgloss.Color("1")), runs: [][]canvas.Float64Point{{
			{X: 0, Y: 9}, {X: 23, Y: 4},
		}}},
	}
}

func canvasIdentity(m *timeserieslinechart.Model) []struct {
	rune rune
	fg   rgba
} {
	g := newPaneGeom(m)
	out := make([]struct {
		rune rune
		fg   rgba
	}, 0, g.gw*g.gh)
	for y := 0; y < g.gh; y++ {
		for x := 0; x < g.gw; x++ {
			cell := m.Canvas.Cell(canvas.Point{X: g.startX + x, Y: y})
			out = append(out, struct {
				rune rune
				fg   rgba
			}{cell.Rune, colorName(cell.Style.GetForeground())})
		}
	}
	return out
}

type rgba struct{ r, g, b, a uint32 }

func colorName(c interface {
	RGBA() (uint32, uint32, uint32, uint32)
}) rgba {
	r, g, b, a := c.RGBA()
	return rgba{r, g, b, a}
}

// TestBrailleCompositorKeepsNearCoincidentComponents proves the defect this
// compositor owns: close lines share cells, but no later series may erase the
// earlier series' visual identity from the whole chart.
func TestBrailleCompositorKeepsNearCoincidentComponents(t *testing.T) {
	m := newCompositorModel()
	series := compositorSeries()
	if !drawBrailleComponents(m, series) {
		t.Fatal("multi-component compositor declined a valid pane")
	}
	want := map[rgba]bool{}
	for _, s := range series {
		want[colorName(s.style.GetForeground())] = false
	}
	for _, cell := range canvasIdentity(m) {
		if cell.rune != 0 {
			if _, ok := want[cell.fg]; ok {
				want[cell.fg] = true
			}
		}
	}
	for color, seen := range want {
		if !seen {
			t.Errorf("component style %#v owns no visible braille cells", color)
		}
	}
}

// TestBrailleCompositorPreservesNativeUnionGeometry compares the replacement
// with ntcharts' old multi-dataset draw. Native ownership is wrong, but its OR
// of the dots is right; the compositor must change only who styles each cell.
func TestBrailleCompositorPreservesNativeUnionGeometry(t *testing.T) {
	series := compositorSeries()
	composed := newCompositorModel()
	drawBrailleComponents(composed, series)

	native := newCompositorModel()
	order := make([]string, 0, len(series))
	for _, s := range series {
		native.SetDataSetStyle(s.key, s.style)
		for _, run := range s.runs {
			for _, p := range run {
				native.PushDataSet(s.key, timeserieslinechart.TimePoint{
					Time: time.UnixMilli(int64(p.X * 1e3)), Value: p.Y,
				})
			}
		}
		order = append(order, s.key)
	}
	native.DrawBrailleDataSets(order)

	got, want := canvasIdentity(composed), canvasIdentity(native)
	for i := range got {
		if got[i].rune != want[i].rune {
			t.Fatalf("cell %d union geometry = %q, native = %q", i, got[i].rune, want[i].rune)
		}
	}
}

// TestBrailleCompositorIsDrawOrderIndependent reverses the logical layers but
// preserves their canonical ranks. Both dot geometry and style ownership must
// remain byte-for-byte identical.
func TestBrailleCompositorIsDrawOrderIndependent(t *testing.T) {
	series := compositorSeries()
	a := newCompositorModel()
	drawBrailleComponents(a, series)
	for i, j := 0, len(series)-1; i < j; i, j = i+1, j-1 {
		series[i], series[j] = series[j], series[i]
	}
	b := newCompositorModel()
	drawBrailleComponents(b, series)

	gotA, gotB := canvasIdentity(a), canvasIdentity(b)
	if len(gotA) != len(gotB) {
		t.Fatalf("canvas sizes differ: %d != %d", len(gotA), len(gotB))
	}
	for i := range gotA {
		if gotA[i] != gotB[i] {
			t.Fatalf("cell %d changed with draw order: %#v != %#v", i, gotA[i], gotB[i])
		}
	}
}

// TestBrailleCompositorExactCoincidenceDoesNotInventOffsets: exact overlap is
// one geometric line. Its cells use the canonical first component's style,
// rather than shifting either series to manufacture separation.
func TestBrailleCompositorExactCoincidenceDoesNotInventOffsets(t *testing.T) {
	line := []canvas.Float64Point{{X: 0, Y: 2}, {X: 23, Y: 8}}
	input := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	output := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	m := newCompositorModel()
	drawBrailleComponents(m, []paneSeriesData{
		{key: "input", rank: 0, style: input, runs: [][]canvas.Float64Point{line}},
		{key: "output", rank: 1, style: output, runs: [][]canvas.Float64Point{line}},
	})

	reference := newCompositorModel()
	reference.Clear()
	reference.DrawXYAxisAndLabel()
	reference.DrawBrailleLineWithStyle(line[0], line[1], input)
	got, want := canvasIdentity(m), canvasIdentity(reference)
	for i := range got {
		if got[i].rune != want[i].rune {
			t.Fatalf("cell %d geometry changed on coincidence: %q != %q", i, got[i].rune, want[i].rune)
		}
		if got[i].rune != 0 && got[i].fg != colorName(input.GetForeground()) {
			t.Fatalf("cell %d tie owner = %#v, want canonical input", i, got[i].fg)
		}
	}
}

// TestBrailleCompositorPreservesGaps ensures separate runs remain separate in
// the union; a missing time span is never bridged by interpolation.
func TestBrailleCompositorPreservesGaps(t *testing.T) {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	m := newCompositorModel()
	drawBrailleComponents(m, []paneSeriesData{
		{key: "input", rank: 0, style: style, runs: [][]canvas.Float64Point{
			{{X: 0, Y: 3}, {X: 5, Y: 5}},
			{{X: 18, Y: 5}, {X: 23, Y: 3}},
		}},
		{key: "output", rank: 1, style: lipgloss.NewStyle().Foreground(lipgloss.Color("4"))},
	})
	g := newPaneGeom(m)
	for x := 7; x <= 16; x++ {
		for y := 0; y < g.gh; y++ {
			if cell := m.Canvas.Cell(canvas.Point{X: g.startX + x, Y: y}); cell.Rune != 0 {
				t.Fatalf("gap column %d row %d contains %q", x, y, cell.Rune)
			}
		}
	}
}
