package views

import (
	"math"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"aiusage/internal/store"
)

// Split exposes the four raw DB columns. Reasoning must NOT fold into output
// (the schema records it as a subset of output → adding it would double-count).
func TestSplitComponents(t *testing.T) {
	b := store.Bucket{Input: 10, Output: 20, Reasoning: 5, CacheCreation: 30, CacheRead: 40, Total: 999}
	c := Split(b)
	if c.Input != 10 || c.Output != 20 || c.CacheRead != 40 || c.CacheCreation != 30 {
		t.Fatalf("Split = %+v", c)
	}
	if c.Output != 20 {
		t.Errorf("reasoning leaked into output: got %d, want 20", c.Output)
	}
	if c.Sum() != 100 {
		t.Errorf("Sum = %d, want 100 (excludes reasoning + provider total)", c.Sum())
	}
	if z := Split(store.Bucket{}); z.Sum() != 0 {
		t.Errorf("zero bucket Sum = %d, want 0", z.Sum())
	}
}

// CompSpecs must keep a fixed order and each selector must pull the right field.
func TestCompSpecsOrderAndPick(t *testing.T) {
	ac := func(s string) lipgloss.AdaptiveColor { return lipgloss.AdaptiveColor{Dark: s, Light: s} }
	specs := CompSpecs(ac("#1"), ac("#2"), ac("#3"))
	if len(specs) != 3 {
		t.Fatalf("want 3 specs, got %d", len(specs))
	}
	want := []string{"input", "output", "cache"}
	wantVal := []int64{1, 2, 7} // cache combines read(3)+creation(4)
	c := Components{Input: 1, Output: 2, CacheRead: 3, CacheCreation: 4}
	for i, s := range specs {
		if s.Key != want[i] {
			t.Errorf("spec[%d].Key = %q, want %q", i, s.Key, want[i])
		}
		if got := s.Pick(c); got != wantVal[i] {
			t.Errorf("spec[%d].Pick = %d, want %d", i, got, wantVal[i])
		}
	}
}

// The log transform used by the trend chart must round-trip: plotting log10(1+v)
// and mapping the Y label back via 10^v-1 recovers v (within float tolerance),
// so axis labels read true token counts. Zero maps to the baseline.
func TestLogRoundTrip(t *testing.T) {
	for _, v := range []int64{0, 1, 100, 1000, 1_000_000, 9_615_603_782} {
		got := math.Pow(10, logT(v)) - 1
		if v == 0 {
			if got > 0.001 {
				t.Errorf("logT(0) round-trip = %v, want ~0", got)
			}
			continue
		}
		if diff := math.Abs(got-float64(v)) / float64(v); diff > 0.001 {
			t.Errorf("round-trip v=%d got=%v rel-diff=%.4f", v, got, diff)
		}
	}
}
