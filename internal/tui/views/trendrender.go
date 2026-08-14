package views

import (
	"math"
	"math/bits"
	"sort"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/NimbleMarkets/ntcharts/v2/canvas"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/graph"
	"github.com/NimbleMarkets/ntcharts/v2/canvas/runes"
	"github.com/NimbleMarkets/ntcharts/v2/linechart"
	"github.com/NimbleMarkets/ntcharts/v2/linechart/timeserieslinechart"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// trendrender.go carries the candidate trend LINE TREATMENTS for the wayfinder
// prototype (ticket #65, research resolution on #64). The hero's information
// architecture is locked - two detented panes, the quantized 1/2/5 SCALE readout
// on each pane rule, the decade-ring log band and the leverage pivot untouched,
// the three series colors as shipped, gap breaks at more than 1.5 bucket steps -
// so only the ink inside a pane's plot rectangle changes.
//
// Every treatment therefore builds the SAME ntcharts model the shipped renderer
// builds (axes, ring labels, shared gutter, time labels) and then draws its own
// content into the graph rectangle. That is what keeps the scrub crosshair
// post-pass (SetColumnBackgroundStyle), the exact-height contract and the render
// memo working unchanged: a treatment is a draw call, not a second widget.
//
// The one integration cost the research called out applies here: ntcharts keeps
// a dataset's scaled points in an unexported buffer, so a custom renderer has to
// redo the X/Y-to-graph mapping. paneGeom is that duplicate, derived from the
// same public accessors timeserieslinechart derives it from.

// TrendRender names one candidate line treatment. The zero value is the shipped
// renderer, so any context that never sets it renders exactly as before.
type TrendRender int

const (
	// TrendCurrent is the shipped renderer: ntcharts DrawBrailleDataSets over
	// one dataset per contiguous run per series, last writer owning the cell.
	TrendCurrent TrendRender = iota
	// TrendSmooth is candidate A: Fritsch-Carlson monotone cubic densification
	// as a data pre-pass, majority-ownership cell compositing (the #67 fix), and
	// a marker at every real bucket coordinate.
	TrendSmooth
	// TrendColumns is candidate B: per-column eighth-block gradient columns, the
	// zero-font-risk control that reads at the 28-column floor.
	TrendColumns
	// TrendRidge is candidate C: the fresh pane as a split ridge - input up,
	// output down from a shared centerline - which makes occlusion structurally
	// impossible. The cache pane keeps candidate A's rendering.
	TrendRidge
	// trendRenderCount bounds the cycle.
	trendRenderCount
)

// Next cycles current -> A -> B -> C -> current. The shipped renderer stays
// reachable as the baseline, which is the whole point of a side-by-side.
func (t TrendRender) Next() TrendRender {
	if t < TrendCurrent || t+1 >= trendRenderCount {
		return TrendCurrent
	}
	return t + 1
}

// Chip names the active treatment for the hero title strip. It is plain text, so
// the reader can name what they are looking at in a monochrome terminal and in a
// screenshot alike.
func (t TrendRender) Chip() string {
	switch t {
	case TrendSmooth:
		return "render: A smooth braille"
	case TrendColumns:
		return "render: B block columns"
	case TrendRidge:
		return "render: C split ridge"
	}
	return "render: current"
}

// --- pane geometry ------------------------------------------------------------

// paneGeom is a BUILT pane's plot rectangle: its size in cells, the canvas
// column its content starts at, and the value ranges its axes were sized from.
// The mappings below reproduce, exactly, what timeserieslinechart does in
// newDataSet (scale by graph width/height) and DrawBrailleDataSets (a braille
// grid of gw*2 by gh*4 dots over that same rectangle).
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

// dotX maps a raw X (unix seconds) onto a braille dot column.
func (g paneGeom) dotX(x float64) int {
	return int(math.Round((x - g.minX) / (g.maxX - g.minX) * float64(g.gw*2-1)))
}

// dotY maps a raw Y (the pane's own value scale) onto a braille dot row,
// counted from the TOP of the plot rectangle the way the canvas counts rows.
func (g paneGeom) dotY(v float64) int {
	h := float64(g.gh*4 - 1)
	return int(math.Round(h - (v-g.minY)/(g.maxY-g.minY)*h))
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
// contiguous runs the gap rule produced. Every treatment consumes this: a
// treatment must never draw across an outage, and a monotone spline must never
// smooth across one either.
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

// --- per-dot ownership compositing (issue #67) --------------------------------

// trendLayer is one series' ink on a pane: the polylines to rasterize, the
// marker dots that name real buckets, and the style that colors whatever cells
// the layer wins.
type trendLayer struct {
	style   lipgloss.Style
	runs    [][]canvas.Point
	markers []canvas.Point
}

// drawOwnedBraille clears the pane, redraws its axes, and composites the layers.
func drawOwnedBraille(m *timeserieslinechart.Model, layers []trendLayer) {
	m.Clear()
	m.DrawXYAxisAndLabel()
	compositeLayers(m, layers)
}

// compositeLayers rasterizes every layer into its OWN braille dot grid and then
// writes one cell per position: the UNION of every layer's dots, styled by the
// layer that owns the MOST dots there.
//
// ntcharts ORs the dot patterns into the cell but writes the incoming style
// unconditionally, so a later series takes the cell color outright and an
// earlier coincident series renders zero visible cells (issue #67, measured: two
// identical series render the first at zero visible cells, flipping exactly when
// draw order flips). Majority ownership keeps both visible, which whole-cell
// glyphs cannot do at all. VisiData resolves sub-pixel color collisions the same
// way - store every attribute that hit the sub-pixel, take the most common.
//
// Ties go to the EARLIEST layer: deterministic, and the exact opposite of the
// last-writer rule it replaces.
func compositeLayers(m *timeserieslinechart.Model, layers []trendLayer) {
	g := newPaneGeom(m)
	if !g.ok() || len(layers) == 0 {
		return
	}
	pats := make([][][]rune, len(layers))
	for i, l := range layers {
		bg := graph.NewBrailleGrid(g.gw, g.gh, 0, float64(g.gw), 0, float64(g.gh))
		for _, run := range l.runs {
			if len(run) == 1 {
				bg.Set(run[0])
				continue
			}
			for j := 0; j+1 < len(run); j++ {
				for _, p := range graph.GetLinePoints(run[j], run[j+1]) {
					bg.Set(p)
				}
			}
		}
		for _, p := range l.markers {
			setMarkerDots(bg, g, p)
		}
		pats[i] = bg.BraillePatterns()
	}
	for y := range pats[0] {
		for x := range pats[0][y] {
			var mask rune
			owner, best := -1, 0
			for i := range pats {
				d := pats[i][y][x] - runes.BrailleBlockOffset
				if d == 0 {
					continue
				}
				mask |= d
				if n := bits.OnesCount32(uint32(d)); n > best {
					owner, best = i, n
				}
			}
			if owner < 0 {
				continue
			}
			m.Canvas.SetCell(canvas.Point{X: g.startX + x, Y: y},
				canvas.NewCellWithStyle(runes.BrailleBlockOffset|mask, layers[owner].style))
		}
	}
}

// setMarkerDots draws the 2x2 dot cluster that names a REAL bucket coordinate.
// A single dot on a one-dot-thick line is invisible, so the marker is thicker
// than the line it sits on. It is placed at the bucket's own coordinate and
// never at a curve sample - the trap Observable Plot documents for built-in line
// markers, which land on path vertices rather than data positions. The anchor
// dot is always the bucket's exact coordinate; the three companions fold inwards
// at the edges of the grid.
func setMarkerDots(bg *graph.BrailleGrid, g paneGeom, p canvas.Point) {
	xs := [2]int{p.X, p.X + 1}
	if xs[1] >= g.gw*2 {
		xs[1] = p.X - 1
	}
	ys := [2]int{p.Y, p.Y + 1}
	if ys[1] >= g.gh*4 {
		ys[1] = p.Y - 1
	}
	for _, x := range xs {
		for _, y := range ys {
			bg.Set(canvas.Point{X: x, Y: y})
		}
	}
}

// --- candidate A: monotone-smoothed braille -----------------------------------

// monotoneTangents returns Fritsch-Carlson tangents for a non-decreasing x
// sequence. The limiter - zero the tangent at a sign change or a zero secant,
// then clamp alpha squared plus beta squared to at most 9 - makes every segment
// monotone, so the curve can neither invent a peak above the adjacent buckets
// nor dip below two non-negative ones. Measured on an isolated spike, uniform
// Catmull-Rom undershoots by 2/27 of the spike height and centripetal
// Catmull-Rom by about a tenth; a negative token count is not an artifact, it is
// a lie about the data, which is why neither is used here.
func monotoneTangents(xs, ys []float64) []float64 {
	n := len(xs)
	if n < 2 {
		return make([]float64, n)
	}
	d := make([]float64, n-1)
	for i := 0; i < n-1; i++ {
		if dx := xs[i+1] - xs[i]; dx > 0 {
			d[i] = (ys[i+1] - ys[i]) / dx
		}
	}
	m := make([]float64, n)
	m[0], m[n-1] = d[0], d[n-2]
	for i := 1; i < n-1; i++ {
		m[i] = (d[i-1] + d[i]) / 2
	}
	for i := 0; i < n-1; i++ {
		if d[i] == 0 {
			// A flat secant pins both ends of the segment: a run of equal buckets
			// stays exactly on its value, and a run of zeros stays exactly on zero.
			m[i], m[i+1] = 0, 0
			continue
		}
		a, b := m[i]/d[i], m[i+1]/d[i]
		if a < 0 {
			m[i], a = 0, 0
		}
		if b < 0 {
			m[i+1], b = 0, 0
		}
		if s := a*a + b*b; s > 9 {
			t := 3 / math.Sqrt(s)
			m[i], m[i+1] = t*a*d[i], t*b*d[i]
		}
	}
	return m
}

// evalMonotone evaluates the cubic Hermite spline with the given tangents at x.
// Outside the sample range it clamps to the end values rather than extrapolate.
func evalMonotone(xs, ys, tan []float64, at float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	if at <= xs[0] {
		return ys[0]
	}
	if at >= xs[n-1] {
		return ys[n-1]
	}
	i := sort.SearchFloat64s(xs, at) - 1
	if i < 0 {
		i = 0
	}
	if i > n-2 {
		i = n - 2
	}
	h := xs[i+1] - xs[i]
	if h <= 0 {
		return ys[i]
	}
	t := (at - xs[i]) / h
	t2 := t * t
	t3 := t2 * t
	return (2*t3-3*t2+1)*ys[i] +
		(t3-2*t2+t)*h*tan[i] +
		(-2*t3+3*t2)*ys[i+1] +
		(t3-t2)*h*tan[i+1]
}

// densifyRun resamples one contiguous run onto its monotone cubic at roughly two
// samples per braille dot column. This is gnuplot's architecture: smoothing is a
// DATA FILTER that densifies the polyline before the renderer, and the drawing
// layer never changes. Measured, the extra points cost nothing - per-cell
// lipgloss dominates the build by an order of magnitude.
func densifyRun(g paneGeom, pts []canvas.Float64Point) []canvas.Float64Point {
	if len(pts) < 3 {
		return pts
	}
	xs := make([]float64, len(pts))
	ys := make([]float64, len(pts))
	for i, p := range pts {
		xs[i], ys[i] = p.X, p.Y
	}
	span := xs[len(xs)-1] - xs[0]
	if span <= 0 {
		return pts
	}
	// Two samples per dot column, apportioned by the run's share of the pane.
	n := int(math.Round(span / (g.maxX - g.minX) * float64(g.gw*4)))
	if n < len(pts)-1 {
		n = len(pts) - 1
	}
	if lim := g.gw * 4; n > lim {
		n = lim
	}
	tan := monotoneTangents(xs, ys)
	out := make([]canvas.Float64Point, 0, n+1)
	for i := 0; i <= n; i++ {
		x := xs[0] + span*float64(i)/float64(n)
		out = append(out, canvas.Float64Point{X: x, Y: evalMonotone(xs, ys, tan, x)})
	}
	return out
}

// smoothLayers turns raw series into candidate A's layers: the densified
// polyline plus a marker at every real bucket coordinate.
func smoothLayers(g paneGeom, series []paneSeriesData) []trendLayer {
	layers := make([]trendLayer, 0, len(series))
	for _, s := range series {
		l := trendLayer{style: s.style}
		for _, run := range s.runs {
			dense := densifyRun(g, run)
			pts := make([]canvas.Point, 0, len(dense))
			for _, p := range dense {
				pts = append(pts, canvas.Point{X: g.dotX(p.X), Y: g.dotY(p.Y)})
			}
			l.runs = append(l.runs, pts)
			for _, p := range run {
				l.markers = append(l.markers, canvas.Point{X: g.dotX(p.X), Y: g.dotY(p.Y)})
			}
		}
		layers = append(layers, l)
	}
	return layers
}

// drawSmoothBraille is candidate A: monotone densification as a pre-pass, the
// existing braille rasterizer, and majority-ownership compositing on top.
func drawSmoothBraille(m *timeserieslinechart.Model, series []paneSeriesData) {
	drawOwnedBraille(m, smoothLayers(newPaneGeom(m), series))
}

// --- candidate B: eighth-block gradient columns -------------------------------

// drawBlockColumns is candidate B: one eighth-block column per plot column, with
// a vertical brightness ramp from a dim base to a bright tip.
//
// The lower ramp (U+2581..U+2588) is complete in the BMP and universally
// covered, which is the point: it carries no font risk where braille carries a
// lot, and each column is independently quantized, so there is no line to
// fragment at the 28-column floor. Series are LAYERED shortest-last: a column
// runs from the baseline up, so drawing the taller series first and the shorter
// one over it leaves every series visible over its own extent. No series can
// erase another.
func drawBlockColumns(m *timeserieslinechart.Model, series []paneSeriesData, step time.Duration) {
	m.Clear()
	m.DrawXYAxisAndLabel()
	g := newPaneGeom(m)
	if !g.ok() || len(series) == 0 {
		return
	}
	flat := make([][]canvas.Float64Point, len(series))
	for i, s := range series {
		for _, run := range s.runs {
			flat[i] = append(flat[i], run...)
		}
	}
	// Half a bucket step is the widest a column may reach for its data: beyond
	// it the nearest bucket is the wrong bucket, and a missing one must leave the
	// column empty rather than borrow its neighbour.
	reach := step.Seconds() / 2
	type col struct {
		v     float64
		style lipgloss.Style
	}
	cols := make([]col, 0, len(series))
	for x := 0; x < g.gw; x++ {
		tx := g.rawX(x)
		cols = cols[:0]
		for i, pts := range flat {
			if v, ok := nearestValue(pts, tx, reach); ok && v > g.minY {
				cols = append(cols, col{v: v, style: series[i].style})
			}
		}
		sort.SliceStable(cols, func(a, b int) bool { return cols[a].v > cols[b].v })
		// under is the series still showing ABOVE this one, so a partial tip
		// drawn inside its column keeps that column's ink as the cell background
		// instead of punching a hole in it. A cell carries two colors; this is
		// the second one.
		var under lipgloss.Style
		for i, c := range cols {
			drawOneColumn(m, g, x, c.v, c.style, i > 0, under)
			under = c.style
		}
	}
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

// drawOneColumn paints one column from the baseline up to v, whole cells plus an
// eighth-block cap for the remainder. A non-zero value that would round to
// nothing still gets the smallest honest mark the ramp has - one eighth - so an
// active bucket is never drawn as an idle one. layered marks a column drawn over
// a taller one, whose style is under: the cap cell then keeps that column's ink
// as its background rather than clearing the part of the cell it does not fill.
func drawOneColumn(m *timeserieslinechart.Model, g paneGeom, x int, v float64,
	base lipgloss.Style, layered bool, under lipgloss.Style) {
	frac := (v - g.minY) / (g.maxY - g.minY)
	if frac <= 0 {
		return
	}
	if frac > 1 {
		frac = 1
	}
	total := frac * float64(g.gh)
	full := int(total)
	if full > g.gh {
		full = g.gh
	}
	tipRune := runes.Null
	if full < g.gh {
		tipRune = runes.LowerBlockElementFromFloat64(total - float64(full))
		if tipRune == runes.Null && full == 0 {
			// The smallest mark the ramp has, rather than nothing at all.
			tipRune = runes.LowerBlockOne
		}
	}
	tip := full - 1
	if tipRune != runes.Null {
		tip = full
	}
	for k := 0; k < full; k++ {
		setColumnCell(m, g, x, k, runes.FullBlock, columnRamp(base, k, tip))
	}
	if tipRune != runes.Null {
		s := columnRamp(base, full, tip)
		if layered {
			if bg := under.GetForeground(); bg != nil {
				s = s.Background(bg)
			}
		}
		setColumnCell(m, g, x, full, tipRune, s)
	}
}

// setColumnCell writes one cell of a column: level 0 is the baseline row.
func setColumnCell(m *timeserieslinechart.Model, g paneGeom, x, level int, r rune, s lipgloss.Style) {
	if level < 0 || level >= g.gh {
		return
	}
	m.Canvas.SetCell(canvas.Point{X: g.startX + x, Y: g.gh - 1 - level},
		canvas.NewCellWithStyle(r, s))
}

// columnRamp is the vertical brightness ramp of a gradient column: a faint base,
// a plain body, a bold tip. It rides on SGR ATTRIBUTES rather than on color
// arithmetic, because the series colors are ANSI palette indices - they have no
// numeric brightness to interpolate, and interpolating a truecolor value would
// look wrong the moment the user's terminal theme moved. Attributes survive a
// 16-color terminal, and a terminal that renders neither still has the glyph.
func columnRamp(base lipgloss.Style, level, tip int) lipgloss.Style {
	if tip > 0 && level >= tip {
		return base.Bold(true)
	}
	if tip >= 3 && level*3 < tip {
		return base.Faint(true)
	}
	return base
}

// --- candidate C: split ridge -------------------------------------------------

// ridgeGeom is the split ridge's row arithmetic: the graph row the centerline
// sits on (counted from the baseline, the way ntcharts indexes ring labels) and
// how many rows each half spans.
type ridgeGeom struct {
	center int
	half   int
}

// newRidgeGeom centres the ridge on the middle row of the pane and gives both
// halves the same reach - an asymmetric ridge would map one value to two
// distances and quietly lie about which series is larger.
//
// The centre row is deliberately NOT constrained to ntcharts' ring pitch: the
// ridge pane asks for a Y step of 1 so the label formatter is consulted on every
// row and can place the rings itself, measured out from the centre. Pinning the
// centre to an even row instead cost up to two rows of reach, which on a
// two-ring half is a whole quantization step of the SCALE ladder.
//
// ok=false when the pane cannot afford one ring pitch on each side.
func newRidgeGeom(graphH int) (ridgeGeom, bool) {
	center := graphH / 2
	half := center
	if above := graphH - 1 - center; above < half {
		half = above
	}
	if half < detentYStep {
		return ridgeGeom{}, false
	}
	return ridgeGeom{center: center, half: half}, true
}

// ridgeMax is the value at the top of the upper half - the same exact-multiple
// derivation the flat panes use, applied to half a pane.
func (r ridgeGeom) ridgeMax(step int64) float64 {
	return detentViewMax(step, r.half, detentYStep)
}

// dotY maps a value onto a braille dot row, dir -1 growing up from the
// centerline and dir +1 growing down.
func (r ridgeGeom) dotY(g paneGeom, step int64, v float64, dir int) int {
	center := (g.gh*4 - 1) - r.center*4
	span := r.half*4 - 1
	if span < 1 {
		return center
	}
	f := v / r.ridgeMax(step)
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return center + dir*int(math.Round(f*float64(span)))
}

// ridgeYLabel labels the ridge axis by DISTANCE from the centerline: the centre
// ring reads zero and the rings above and below it read the same exact multiples
// of the step, which is what a mirrored pane actually means. The printed number
// is derived from the row index, never from the float ntcharts hands over, so it
// cannot disagree with the axis arithmetic.
func ridgeYLabel(c Ctx, step int64, r ridgeGeom, labelW int) linechart.LabelFormatter {
	return func(i int, _ float64) string {
		d := i - r.center
		if d < 0 {
			d = -d
		}
		if d%detentYStep != 0 || d > r.half {
			return ""
		}
		return padLeftLocal(detentHuman(c, int64(d/detentYStep)*step), labelW)
	}
}

// ridgeLabelWidth is the Y-gutter width a ridge pane needs. ntcharts sizes the
// gutter from its formatter's widest output, so this has to be settled before
// the widget exists - exactly as detentLabelWidth is.
func ridgeLabelWidth(c Ctx, step int64, r ridgeGeom) int {
	w := 0
	for k := 0; k <= r.half/detentYStep; k++ {
		if n := len([]rune(detentHuman(c, int64(k)*step))); n > w {
			w = n
		}
	}
	return w
}

// drawSplitRidge is candidate C: the first series grows UP and the second grows
// DOWN from a shared centerline. Mirroring one non-negative series would spend
// twice the rows on the same information and falsely imply zero-crossing
// semantics, but two genuinely different series sharing a scale and an axis is
// the one case where it earns its ink - cava's split_stereo, netwatch's
// up-and-down model. Occlusion becomes structurally impossible rather than
// resolved by a tie-break.
//
// It is drawn in braille, not the eighth-block ramp: the UPPER ramp is not in
// the BMP (U+1FB82..86, Symbols for Legacy Computing) and its coverage is poor,
// which is the reason top-anchored designs are rare.
func drawSplitRidge(m *timeserieslinechart.Model, series []paneSeriesData, r ridgeGeom, step int64) {
	m.Clear()
	m.DrawXYAxisAndLabel()
	g := newPaneGeom(m)
	if !g.ok() {
		return
	}
	drawRidgeCenterline(m, g, r)
	layers := make([]trendLayer, 0, len(series))
	for i, s := range series {
		dir := -1 // the first series grows up
		if i > 0 {
			dir = 1
		}
		l := trendLayer{style: s.style}
		for _, run := range s.runs {
			dense := densifyRun(g, run)
			pts := make([]canvas.Point, 0, len(dense))
			for _, p := range dense {
				pts = append(pts, canvas.Point{X: g.dotX(p.X), Y: r.dotY(g, step, p.Y, dir)})
			}
			l.runs = append(l.runs, pts)
			for _, p := range run {
				l.markers = append(l.markers, canvas.Point{X: g.dotX(p.X), Y: r.dotY(g, step, p.Y, dir)})
			}
		}
		layers = append(layers, l)
	}
	compositeLayers(m, layers)
}

// drawRidgeCenterline draws the axis both halves are measured from. It is laid
// down BEFORE the braille so the series own every cell they reach; the reader
// sees the centerline only where nothing was plotted, which is where it is
// needed.
func drawRidgeCenterline(m *timeserieslinechart.Model, g paneGeom, r ridgeGeom) {
	y := g.gh - 1 - r.center
	if y < 0 || y >= g.gh {
		return
	}
	for x := 0; x < g.gw; x++ {
		m.Canvas.SetCell(canvas.Point{X: g.startX + x, Y: y},
			canvas.NewCellWithStyle(runes.LineHorizontal, m.AxisStyle))
	}
}
