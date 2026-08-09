package views

import (
	"math"
	"testing"
)

// ladderBelow returns the 1/2/5x10^n value immediately below d, or 0 when d is
// the bottom of the ladder. Used to prove quantizeDetent is minimal, not merely
// sufficient.
func ladderBelow(d int64) int64 {
	switch {
	case d <= 1:
		return 0
	case isDecadeMultiple(d, 5):
		return d / 5 * 2
	case isDecadeMultiple(d, 2):
		return d / 2
	default: // 1x10^n
		return d / 10 * 5
	}
}

func isDecadeMultiple(d, m int64) bool {
	if d%m != 0 {
		return false
	}
	for v := d / m; v > 1; v /= 10 {
		if v%10 != 0 {
			return false
		}
	}
	return true
}

// TestQuantizeDetentLadder pins the ladder: the result is always >= v, always a
// 1/2/5x10^n value, and never one rung higher than it needs to be.
func TestQuantizeDetentLadder(t *testing.T) {
	cases := []struct {
		v    float64
		want int64
	}{
		{0, 1}, {0.4, 1}, {1, 1},
		{1.1, 2}, {2, 2}, {2.1, 5}, {5, 5}, {5.1, 10},
		{9_999, 10_000}, {10_000, 10_000},
		{2_086_000, 5_000_000},
		{300_000_000, 500_000_000},
		{1_230_000_000, 2_000_000_000},
	}
	for _, c := range cases {
		if got := quantizeDetent(c.v); got != c.want {
			t.Errorf("quantizeDetent(%g) = %d, want %d", c.v, got, c.want)
		}
	}

	// Minimality across the whole working range: the rung below never covers v.
	for v := 1.0; v < 2e9; v *= 1.37 {
		d := quantizeDetent(v)
		if float64(d) < v {
			t.Fatalf("quantizeDetent(%g) = %d does not cover v", v, d)
		}
		if b := ladderBelow(d); b > 0 && float64(b) >= v {
			t.Fatalf("quantizeDetent(%g) = %d is one rung too high (%d covers it)", v, d, b)
		}
	}
}

// TestDetentRingRoundTrip is the hard guarantee behind the whole detented axis:
// for every pane geometry and peak, the value ntcharts computes for a labeled
// row — viewMaxY*i/graphHeight — is an EXACT multiple of the declared step, and
// the printed label is that multiple. If this ever drifts, "SCALE 5M/div" is a
// lie and the rings become decoration.
func TestDetentRingRoundTrip(t *testing.T) {
	c := heroTestCtx()
	peaks := []int64{1, 999, 1_000, 47_500, 912_300, 9_450_000, 250_000_000, 1_230_000_000}
	for graphH := 4; graphH <= 20; graphH++ {
		for _, peak := range peaks {
			step := paneDetent(peak, graphH, detentYStep)
			viewMax := detentViewMax(step, graphH, detentYStep)

			if viewMax < float64(peak) {
				t.Fatalf("graphH=%d peak=%d: viewMax %g does not cover the peak", graphH, peak, viewMax)
			}
			label := detentYLabel(c, step, detentYStep, 0)
			for i := 0; i <= graphH; i += detentYStep {
				want := int64(i/detentYStep) * step
				got := viewMax * float64(i) / float64(graphH)
				if math.Abs(got-float64(want)) > math.Abs(float64(want))*1e-9 {
					t.Fatalf("graphH=%d peak=%d row=%d: ntcharts value %g, want exactly %d",
						graphH, peak, i, got, want)
				}
				if l, wl := label(i, got), detentHuman(c, want); l != wl {
					t.Fatalf("graphH=%d peak=%d row=%d: label %q, want %q", graphH, peak, i, l, wl)
				}
			}
			// Off-ring rows stay blank so no label can land off the ladder.
			if l := label(1, 0); l != "" {
				t.Fatalf("graphH=%d: off-ring row labelled %q", graphH, l)
			}
		}
	}
}

// TestDetentLockHysteresis locks the anti-flap contract: the scale steps up the
// moment the data overflows it, holds while the peak stays inside the band, and
// steps down only once the peak has dropped past the margin. Geometry changes
// reset it; a nil lock is always the unlocked value.
func TestDetentLockHysteresis(t *testing.T) {
	const graphH, key = 6, "cache"
	top := func(step int64) int64 { return int64(detentViewMax(step, graphH, detentYStep)) }

	var l detentLock
	first := l.pick(key, graphH, detentYStep, 500_000_000)
	if first != paneDetent(500_000_000, graphH, detentYStep) {
		t.Fatalf("first pick = %d, want the unlocked value", first)
	}

	// Inside the band: hold, even though a smaller step would now fit.
	held := l.pick(key, graphH, detentYStep, top(first)*6/10)
	if held != first {
		t.Fatalf("held pick = %d, want %d (inside the hysteresis band)", held, first)
	}

	// Overflow: step up immediately.
	up := l.pick(key, graphH, detentYStep, top(first)+1)
	if up <= first {
		t.Fatalf("overflow pick = %d, want a step above %d", up, first)
	}

	// Past the margin: adopt the smaller scale.
	small := int64(float64(top(up)) * detentShrinkMargin / 4)
	down := l.pick(key, graphH, detentYStep, small)
	if down >= up {
		t.Fatalf("shrink pick = %d, want a step below %d", down, up)
	}
	if want := paneDetent(small, graphH, detentYStep); down != want {
		t.Fatalf("shrink pick = %d, want the freshly computed %d", down, want)
	}

	// A different pane height is a different axis: no hold across it.
	if got, want := l.pick(key, graphH+4, detentYStep, small), paneDetent(small, graphH+4, detentYStep); got != want {
		t.Fatalf("resized pick = %d, want %d", got, want)
	}

	var nilLock *detentLock
	if got, want := nilLock.pick(key, graphH, detentYStep, 42), paneDetent(42, graphH, detentYStep); got != want {
		t.Fatalf("nil-lock pick = %d, want %d", got, want)
	}
}
