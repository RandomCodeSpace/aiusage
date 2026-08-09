package views

import (
	"math"
	"strconv"
	"strings"
)

// scale.go is the pure arithmetic behind the hero's detented axes (issue #8):
// the 1/2/5x10^n step ladder, the per-pane step selection, and the frame-to-
// frame lock that stops a chosen step from flapping. Nothing here touches
// ntcharts or a widget — chart construction lives behind the chartstyle.go
// seam.
//
// Why detent at all: a continuously autoscaled axis re-derives its top of range
// on every refresh, so the gridlines move under the data and real growth is
// invisible. A detented axis declares one step ("SCALE 5M/div") and holds it,
// which is what makes two refreshes — and the two panes — comparable.
//
// Ring exactness is by construction, not by rounding. ntcharts labels canvas
// row i with viewMaxY*i/graphHeight, and graphHeight is derived from the canvas
// and the axis steps only — never from the Y range — so setting
// viewMaxY = D*graphHeight/yStep puts every labeled row on an exact multiple of
// D. detentYLabel then derives the printed number from the row index (k*D as an
// int64) rather than from the float it is handed, so no rounding can leak in.

const (
	// detentYStep is the ring pitch: one labeled ring every N canvas rows.
	detentYStep = 2

	// detentShrinkMargin is the hysteresis band. The step rises as soon as the
	// data overflows the current top of range, but falls only once the peak has
	// dropped below this fraction of it. Without the band a series sitting near
	// a ladder boundary would re-quantize the whole axis every refresh.
	detentShrinkMargin = 0.5

	// maxDetentUnit caps the decade walk in quantizeDetent so the ladder search
	// cannot overflow int64 on a pathological input.
	maxDetentUnit = int64(1e17)

	// maxDecadePitch is the widest power of ten int64 can name, and therefore
	// the cap on every decade count below: a ring label can never be computed
	// past the type that holds it.
	maxDecadePitch = int64(18)
)

// quantizeDetent returns the smallest value on the 1/2/5x10^n ladder that is
// >= v. Values at or below 1 pin to 1 (one token per division is the floor).
func quantizeDetent(v float64) int64 {
	if v <= 1 {
		return 1
	}
	p := int64(1)
	for p < maxDetentUnit && float64(p)*10 <= v {
		p *= 10
	}
	for _, m := range []int64{1, 2, 5} {
		if float64(m*p) >= v {
			return m * p
		}
	}
	return 10 * p
}

// paneDetent picks the per-division step for a pane: the smallest ladder value
// whose full pane height (detentViewMax) still covers maxV.
func paneDetent(maxV int64, graphH, yStep int) int64 {
	if yStep < 1 {
		yStep = 1
	}
	if graphH < yStep {
		graphH = yStep
	}
	if maxV < 1 {
		maxV = 1
	}
	return quantizeDetent(float64(maxV) * float64(yStep) / float64(graphH))
}

// detentViewMax is the Y range top that lands every labeled row on an exact
// multiple of step.
func detentViewMax(step int64, graphH, yStep int) float64 {
	if yStep < 1 {
		yStep = 1
	}
	if graphH < yStep {
		graphH = yStep
	}
	return float64(step) * float64(graphH) / float64(yStep)
}

// detentAxis is one pane's settled Y axis: the step it labels in, and the graph
// height ntcharts will hand it (canvas height minus any axis rows).
type detentAxis struct {
	step   int64
	graphH int
}

// detentHuman renders an exact detent multiple compactly. The injected Humanize
// keeps one decimal below 100 ("2.0M"), which is noise on a number that is
// exact by construction, so the trailing ".0" is dropped. Output stays ASCII:
// ntcharts measures the Y gutter in bytes, so a multi-byte label would reserve
// the wrong number of columns.
func detentHuman(c Ctx, n int64) string {
	s := humanizeOr(c, n)
	for _, suf := range []string{"K", "M", "B", "T"} {
		s = strings.Replace(s, ".0"+suf, suf, 1)
	}
	return s
}

// detentLabelWidth is the shared Y-gutter width for a set of panes: the widest
// ring label any of them can print. It has to be settled BEFORE either widget
// is constructed — ntcharts sizes the gutter from its formatter's widest output
// and that width is what column-aligns the panes' plot areas.
func detentLabelWidth(c Ctx, axes []detentAxis) int {
	w := 0
	for _, a := range axes {
		for k := 0; k <= a.graphH/detentYStep; k++ {
			if n := len([]rune(detentHuman(c, int64(k)*a.step))); n > w {
				w = n
			}
		}
	}
	return w
}

// detentYLabel formats the ring label for canvas row i, padded to the shared
// gutter width. Off-ring rows print nothing; ring rows print k*step, derived
// from the row index so the label can never disagree with the axis arithmetic.
func detentYLabel(c Ctx, step int64, yStep, labelW int) func(int, float64) string {
	return func(i int, _ float64) string {
		if yStep < 1 || i%yStep != 0 {
			return ""
		}
		return padLeftLocal(detentHuman(c, int64(i/yStep)*step), labelW)
	}
}

// detentLock keeps each pane's chosen step across frames. The step rises the
// moment the data needs it and falls only once the peak has dropped clear of
// detentShrinkMargin, so a live dashboard does not re-scale its axes every time
// a refresh lands. Geometry is part of the state: a resize changes what a step
// means, so it re-picks from scratch.
//
// The zero value is usable. A nil *detentLock is the unlocked case (a direct,
// un-memoized render) and always returns the freshly computed step.
type detentLock struct {
	steps map[string]detentAxis
}

// pick returns the step for the named pane at graphH covering maxV, applying
// the hysteresis, and records it.
func (l *detentLock) pick(key string, graphH, yStep int, maxV int64) int64 {
	step := paneDetent(maxV, graphH, yStep)
	if l == nil {
		return step
	}
	if prev, ok := l.steps[key]; ok && prev.graphH == graphH {
		top := detentViewMax(prev.step, graphH, yStep)
		if float64(maxV) <= top && float64(maxV) >= top*detentShrinkMargin {
			step = prev.step
		}
	}
	if l.steps == nil {
		l.steps = map[string]detentAxis{}
	}
	l.steps[key] = detentAxis{step: step, graphH: graphH}
	return step
}

// --- decade rings (issue #39) -------------------------------------------------
//
// The small-terminal hero cannot afford two detented panes, so it puts every
// series on ONE log axis. Its detent is not a token step but a DECADE PITCH:
// how many powers of ten one labeled ring spans. Whole decades are what make
// the rings exact — a fractional pitch would label rows with numbers that are
// not powers of ten, which is precisely the "decoration gridline" the detent
// exists to avoid.
//
// The exactness argument is the one above, unchanged: ntcharts labels canvas
// row i with viewMaxY*i/graphHeight, so detentViewMax(pitch, graphH, yStep)
// puts row k*yStep at exactly k*pitch decades, and decadeYLabel derives the
// printed power of ten from the row index rather than from the float.

// decadesFor is the number of whole decades a peak needs: the smallest n with
// 10^n >= v. A peak of one token or less needs none — the baseline of a log
// axis IS one token, since log10 has no zero.
func decadesFor(v int64) int64 {
	n := int64(0)
	for p := int64(1); n < maxDecadePitch && p < v; p *= 10 {
		n++
	}
	return n
}

// pow10 is 10^n, clamped to the widest power of ten int64 holds.
func pow10(n int64) int64 {
	if n < 1 {
		return 1
	}
	if n > maxDecadePitch {
		n = maxDecadePitch
	}
	p := int64(1)
	for i := int64(0); i < n; i++ {
		p *= 10
	}
	return p
}

// decadeRings is how many labeled rings a pane of graphH rows carries at the
// given ring pitch — the divisor the pitch search works against.
func decadeRings(graphH, yStep int) int64 {
	if yStep < 1 {
		yStep = 1
	}
	if n := int64(graphH / yStep); n > 0 {
		return n
	}
	return 1
}

// decadePitch picks the ring pitch: the fewest whole decades per ring whose
// full pane height still covers maxV. It is quantizeDetent's counterpart on the
// decade ladder.
func decadePitch(maxV int64, graphH, yStep int) int64 {
	rings := decadeRings(graphH, yStep)
	d := (decadesFor(maxV) + rings - 1) / rings
	if d < 1 {
		return 1
	}
	if d > maxDecadePitch {
		return maxDecadePitch
	}
	return d
}

// pickDecade is detentLock.pick on the decade ladder: the same hysteresis, read
// in decades rather than tokens. The pitch rises the moment the peak needs more
// decades than the axis carries, and falls only once the peak's decade count
// has dropped below detentShrinkMargin of the top ring — comparing logs, not
// magnitudes, because a linear margin on a log axis would never trip.
func (l *detentLock) pickDecade(key string, graphH, yStep int, maxV int64) int64 {
	d := decadePitch(maxV, graphH, yStep)
	if l == nil {
		return d
	}
	if prev, ok := l.steps[key]; ok && prev.graphH == graphH {
		top := prev.step * decadeRings(graphH, yStep)
		need := decadesFor(maxV)
		if need <= top && float64(need) >= float64(top)*detentShrinkMargin {
			d = prev.step
		}
	}
	if l.steps == nil {
		l.steps = map[string]detentAxis{}
	}
	l.steps[key] = detentAxis{step: d, graphH: graphH}
	return d
}

// decadeLogValue maps a token count onto the decade axis. Anything at or below
// one token pins to the baseline: log10 has no zero, so the bottom row means
// "one token or none" rather than "nothing happened".
func decadeLogValue(v int64) float64 {
	if v <= 1 {
		return 0
	}
	return math.Log10(float64(v))
}

// decadeRingLabel names ring k at the given pitch: the exact power of ten
// 10^(k*pitch), humanized the same way the linear rings are.
func decadeRingLabel(c Ctx, k, pitch int64) string {
	return detentHuman(c, pow10(k*pitch))
}

// decadeLabelWidth is the Y-gutter width for a decade axis: the widest ring
// label the pane can print. ntcharts sizes the gutter from its formatter's
// widest output, so this has to be settled before the widget is constructed.
func decadeLabelWidth(c Ctx, pitch int64, graphH, yStep int) int {
	if yStep < 1 {
		yStep = 1
	}
	w := 0
	for k := 0; k <= graphH/yStep; k++ {
		if n := len([]rune(decadeRingLabel(c, int64(k), pitch))); n > w {
			w = n
		}
	}
	return w
}

// decadeYLabel is detentYLabel for the decade axis: off-ring rows print
// nothing, ring rows print 10^(k*pitch) derived from the row index.
func decadeYLabel(c Ctx, pitch int64, yStep, labelW int) func(int, float64) string {
	return func(i int, _ float64) string {
		if yStep < 1 || i%yStep != 0 {
			return ""
		}
		return padLeftLocal(decadeRingLabel(c, int64(i/yStep), pitch), labelW)
	}
}

// decadePitchLabel renders the pitch for the SCALE readout. A decade pitch is a
// multiplication, not a token count, so it prints as a plain power of ten —
// humanizing it ("1K") would read as tokens, the same reason leverageRatioLabel
// stays an integer multiple.
func decadePitchLabel(pitch int64) string {
	return "10^" + strconv.FormatInt(pitch, 10)
}
