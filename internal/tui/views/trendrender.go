package views

import (
	"math"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/runes"
	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// trendrender.go draws the hero's trend body: three self-scaled heat lanes,
// intensity-only, written into an ntcharts plot rectangle.
//
// The lanes are the rendering, not a mode of it. The hero's information
// architecture around them is the one that was locked - the quantized 1/2/5
// SCALE readout, the decade-ring log band, the leverage pivot, the three series
// colors, gap breaks at more than 1.5 bucket steps - and the lanes take over
// only the ink inside the plot rectangle plus the vertical layout of the pane
// they own. Below their geometry floor the two detented braille panes still
// build (chartstyle.go, buildTwoPaneFrame), which is the sub-floor fallback.
//
// Building the SAME ntcharts model the braille panes build is what keeps the
// scrub crosshair post-pass (SetColumnBackgroundStyle), the exact-height
// contract and the render memo working unchanged: the lanes are a draw call,
// not a second widget. The one cost of that is paneGeom: ntcharts keeps a
// dataset's scaled points in an unexported buffer, so a custom renderer has to
// redo the X-to-graph mapping from the same public accessors the widget derives
// it from.

// --- pane geometry ------------------------------------------------------------

// paneGeom is a BUILT pane's plot rectangle: its size in cells, the canvas
// column its content starts at, and the value ranges its axes were sized from.
type paneGeom struct {
	gw, gh                 int
	startX                 int
	minX, maxX, minY, maxY float64
}

// newPaneGeom reads a built model's geometry through its public accessors.
func newPaneGeom(m *timeserieslinechart.Model) paneGeom {
	g := paneGeom{
		gw:   m.GraphWidth(),
		gh:   m.GraphHeight(),
		minX: m.ViewMinX(),
		maxX: m.ViewMaxX(),
		minY: m.ViewMinY(),
		maxY: m.ViewMaxY(),
	}
	if m.YStep() > 0 {
		// The Y gutter and the axis column come first; content starts after them,
		// which is where DrawBrailleDataSets starts its patterns too.
		g.startX = m.Origin().X + 1
	}
	return g
}

// ok reports whether the pane has a plot rectangle worth drawing into.
func (g paneGeom) ok() bool {
	return g.gw > 0 && g.gh > 0 && g.maxX > g.minX && g.maxY > g.minY
}

// rawX is the raw X a cell column of the plot rectangle stands for.
func (g paneGeom) rawX(c int) float64 {
	if g.gw < 2 {
		return g.minX
	}
	return g.minX + float64(c)*(g.maxX-g.minX)/float64(g.gw-1)
}

// --- series extraction --------------------------------------------------------

// paneSeriesData is one series' RAW points on a pane, already split into the
// contiguous runs the gap rule produced. A lane must never draw across an
// outage, so the split happens before any of it reaches the canvas.
type paneSeriesData struct {
	style lipgloss.Style
	runs  [][]canvas.Float64Point
}

// paneSeriesFor extracts each spec's points, split by gapRuns. X is unix seconds
// the way timeserieslinechart stores a TimePoint; Y is the pane's value scale.
func paneSeriesFor(specs []CompSpec, buckets []store.Bucket, times []time.Time, dim string) []paneSeriesData {
	runs := gapRuns(times, dim)
	out := make([]paneSeriesData, 0, len(specs))
	for _, s := range specs {
		d := paneSeriesData{style: s.Style()}
		for _, run := range runs {
			if len(run) == 0 {
				continue
			}
			pts := make([]canvas.Float64Point, 0, len(run))
			for _, i := range run {
				pts = append(pts, canvas.Float64Point{
					X: float64(times[i].UnixMilli()) / 1e3,
					Y: float64(s.Pick(Split(buckets[i]))),
				})
			}
			d.runs = append(d.runs, pts)
		}
		out = append(out, d)
	}
	return out
}

// nearestValue returns the value of the point closest to tx, provided it is
// within reach. Points are in ascending X order.
func nearestValue(pts []canvas.Float64Point, tx, reach float64) (float64, bool) {
	if len(pts) == 0 {
		return 0, false
	}
	i := sort.Search(len(pts), func(k int) bool { return pts[k].X >= tx })
	best, dist := -1, math.Inf(1)
	for _, k := range [2]int{i - 1, i} {
		if k < 0 || k >= len(pts) {
			continue
		}
		if d := math.Abs(pts[k].X - tx); d < dist {
			best, dist = k, d
		}
	}
	if best < 0 || dist > reach {
		return 0, false
	}
	return pts[best].Y, true
}

// --- self-scaled heat lanes ---------------------------------------------------

// The ramp itself - heatRamp, heatTrack, heatRungFor, heatInk - lives in
// heat.go: the KPI tiles and the degraded trend strips speak the same vocabulary
// in string form, and one ladder shared beats two that drift.

// heatGeom is the vertical layout of the lanes inside a pane: how tall each
// lane's band is, where the first lane's rule sits, and - in the degraded modes
// only - the blank rows between lanes.
//
// tall is the shipping shape: the bands GROW to consume the hero body, split
// three ways, so the three lanes read as three thick horizontal bands. It says
// nothing about the encoding - a band is painted the same way at any height,
// every row of a bucket carrying the same rung - only about how much of the
// panel the lanes are allowed to take. The degraded modes are what is left when
// there are not enough rows for that: fixed thin strips, centred with margins,
// which say the same thing in less space.
type heatGeom struct {
	tall  bool
	bandH int
	top   int
	gap   int
}

// heatFlatBandH is the band height of the degraded strip.
const heatFlatBandH = 2

// heatMinTallBand is the shortest band worth growing to fill the pane with. At
// one or two rows the grown layout and the fixed strip are the same thickness,
// and the fixed one spends its leftover rows on margins between the lanes
// instead of dumping the whole remainder above the first one.
const heatMinTallBand = 3

// heatMaxGap caps the blank rows between lanes in the degraded modes. Slack past
// it goes to the margins: lanes that drift too far apart stop reading as one
// chart.
const heatMaxGap = 3

// newHeatGeom lays out n lanes in gh rows.
//
// The rows go into the BANDS first. Three equal bands plus their three rules is
// the whole budget, and any remainder (0..n-1 rows, from the integer split) sits
// above the first lane rather than being smeared between bands, so the three
// bands stay exactly equal - a lane one row taller than its neighbour would make
// two equal values render at different heights.
//
// ok=false when even one row per lane plus its rule does not fit, which leaves
// the caller on the sub-floor pane layout.
func newHeatGeom(gh, n int) (heatGeom, bool) {
	if n < 1 || gh < n*2 {
		return heatGeom{}, false
	}
	if bandH := (gh - n) / n; bandH >= heatMinTallBand {
		return heatGeom{tall: true, bandH: bandH, top: gh - n*(1+bandH)}, true
	}
	hg := heatGeom{bandH: heatFlatBandH}
	if gh < n*(1+heatFlatBandH) {
		hg.bandH = 1
	}
	content := n * (1 + hg.bandH)
	// The slack falls into n+1 slots: the top margin, the n-1 gaps, the bottom.
	if slack := gh - content; slack > 0 && n > 1 {
		if hg.gap = slack / (n + 1); hg.gap > heatMaxGap {
			hg.gap = heatMaxGap
		}
		content += hg.gap * (n - 1)
	}
	hg.top = (gh - content) / 2
	return hg, true
}

// headerRow is the canvas row carrying lane i's rule.
func (hg heatGeom) headerRow(i int) int {
	return hg.top + i*(1+hg.bandH+hg.gap)
}

// heatRow is the TOP canvas row of lane i's band.
func (hg heatGeom) heatRow(i int) int { return hg.headerRow(i) + 1 }

// baseRow is the LAST canvas row of lane i's band. A bucket paints every row
// from heatRow to baseRow inclusive, so the two together are the extent of a
// lane rather than a baseline anything is measured from.
func (hg heatGeom) baseRow(i int) int { return hg.heatRow(i) + hg.bandH - 1 }

// drawHeatLanes draws the hero trend: one horizontal lane per series, every
// bucket a full-height slab of ONE intensity read against that lane's own peak.
//
// Intensity is the only channel, which is what makes this a heat map rather
// than a bar chart wearing shade glyphs. A bucket paints every row of its band
// with the same rung, so a lane is a continuous horizontal band whose darkness
// is the value; nothing about a bucket's silhouette carries information, and the
// reader's eye compares ink rather than measuring heights against a baseline
// that does not exist. Band height therefore buys legibility, not resolution -
// a taller band is the same picture, easier to see.
//
// Self-scaling per lane is the other half. Cache runs about 155x fresh, which
// is the same data fact that forced the two-pane split; on one shared scale two
// of the three lanes would sit at the bottom rung and the picture would say
// nothing. Each lane states its own peak on its rule, so the intensity is read
// as a fraction of a number the reader can see rather than of an unstated one.
func drawHeatLanes(c Ctx, m *timeserieslinechart.Model, specs []CompSpec,
	buckets []store.Bucket, times []time.Time, dim string) bool {
	g := newPaneGeom(m)
	hg, ok := newHeatGeom(g.gh, len(specs))
	if !g.ok() || !ok {
		return false
	}
	m.Clear()
	m.DrawXYAxisAndLabel()

	series := paneSeriesFor(specs, buckets, times, dim)
	reach := bucketStep(dim).Seconds() / 2
	for i, s := range specs {
		var pts []canvas.Float64Point
		for _, run := range series[i].runs {
			pts = append(pts, run...)
		}
		var peak float64
		for _, p := range pts {
			if p.Y > peak {
				peak = p.Y
			}
		}
		writeHeatHeader(c, m, g, hg.headerRow(i), s, peak)
		base := c.compStyle(s)
		for x := 0; x < g.gw; x++ {
			v, ok := nearestValue(pts, g.rawX(x), reach)
			switch {
			case !ok:
				// A missing bucket leaves the band blank: an outage and an idle
				// day are different facts, and only one is about usage.
			case v <= 0 || peak <= 0:
				fillHeatSlab(m, g, hg, i, x, heatTrack, c.Faint)
			default:
				glyph, style := heatInk(base, v/peak)
				fillHeatSlab(m, g, hg, i, x, glyph, style)
			}
		}
	}
	return true
}

// fillHeatSlab paints one bucket's whole band: the same glyph and style on every
// row from the band top to its last row. Uniformity is the contract - a slab
// whose rows differed would put a silhouette back into a picture that encodes
// with ink alone, and the reader would start measuring it.
func fillHeatSlab(m *timeserieslinechart.Model, g paneGeom, hg heatGeom,
	lane, x int, glyph rune, style lipgloss.Style) {
	for row := hg.heatRow(lane); row <= hg.baseRow(lane); row++ {
		m.Canvas.SetCell(canvas.Point{X: g.startX + x, Y: row},
			canvas.NewCellWithStyle(glyph, style))
	}
}

// writeHeatHeader draws one lane's rule directly onto the canvas: glyph, name,
// hairline, and the lane's own peak in the SCALE vocabulary. It is written cell
// by cell because a canvas holds one style per cell and the parts are styled
// differently; lipgloss cannot hand a multi-styled string to a cell grid.
func writeHeatHeader(c Ctx, m *timeserieslinechart.Model, g paneGeom, row int, s CompSpec, peak float64) {
	if row < 0 || row >= g.gh {
		return
	}
	x := g.startX
	m.Canvas.SetStringWithStyle(canvas.Point{X: x, Y: row}, s.Glyph, c.compStyle(s))
	x += len([]rune(s.Glyph)) + 1
	m.Canvas.SetStringWithStyle(canvas.Point{X: x, Y: row}, s.Label, c.StatLabel)
	x += len([]rune(s.Label)) + 1

	tail := ""
	if h := detentHuman(c, int64(peak)); h != "" {
		tail = "max " + h
	}
	end := g.startX + g.gw
	if tail != "" {
		end -= len([]rune(tail)) + 1
	}
	if end > x {
		m.Canvas.SetStringWithStyle(canvas.Point{X: x, Y: row},
			strings.Repeat(string(runes.LineHorizontal), end-x), c.Faint)
	}
	if tail != "" && end >= x {
		m.Canvas.SetStringWithStyle(canvas.Point{X: end + 1, Y: row}, tail, c.Subtle)
	}
}
