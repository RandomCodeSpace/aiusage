package views

import (
	"testing"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// memoTestCtx builds the minimal chart-capable Ctx (non-zero adaptive colors —
// a zero compat.AdaptiveColor panics inside lipgloss v2 Render).
func memoTestCtx() Ctx {
	ac := func(s string) compat.AdaptiveColor {
		return compat.AdaptiveColor{Light: lipgloss.Color(s), Dark: lipgloss.Color(s)}
	}
	return Ctx{
		Comp:     CompSpecs(lipgloss.Color("2"), lipgloss.Color("4"), lipgloss.Color("5")),
		Humanize: func(n int64) string { return "9.4M" },
		Truncate: func(s string, w int) string { return s },
		Faint:    lipgloss.NewStyle(), Subtle: lipgloss.NewStyle(),
		NowColor:    ac("3"),
		AccentColor: ac("6"),
		FaintColor:  ac("8"),
	}
}

func memoTestBuckets(n int) []store.Bucket {
	out := make([]store.Bucket, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.Bucket{
			Keys:  map[string]string{"day": "2026-05-2" + string(rune('0'+i))},
			Input: int64(100000 * (i + 1)), Output: 50000, CacheRead: 9000000, Total: 9450000,
		})
	}
	return out
}

// TestHeroMemoChartBuiltOnce locks the 1f keying contract for the detented
// hero: the braille panes are built once per (generation, mode, w, h) and NOT
// rebuilt for repeated renders or scrub-index changes; geometry, mode and
// generation changes re-key.
func TestHeroMemoChartBuiltOnce(t *testing.T) {
	c := memoTestCtx()
	tl := memoTestBuckets(7)
	m := NewHeroMemo()

	s0, ok := m.frame(c, 1, tl, "day", heroFrameTwoPane, 60, 16, -1)
	if !ok || s0 == "" {
		t.Fatal("first hero frame render failed")
	}
	if got := m.Builds(); got != 1 {
		t.Fatalf("builds after first render = %d, want 1", got)
	}

	// Repeat render: cached string, no rebuild.
	if s, _ := m.frame(c, 1, tl, "day", heroFrameTwoPane, 60, 16, -1); s != s0 {
		t.Fatal("repeat render returned a different frame")
	}
	if got := m.Builds(); got != 1 {
		t.Fatalf("repeat render rebuilt the panes: builds = %d", got)
	}

	// Scrub: the key excludes the scrub index — highlight is a post-pass.
	s3, _ := m.frame(c, 1, tl, "day", heroFrameTwoPane, 60, 16, 3)
	if got := m.Builds(); got != 1 {
		t.Fatalf("scrub change rebuilt the braille panes: builds = %d", got)
	}
	if s3 == s0 {
		t.Fatal("scrub overlay did not change the rendered body")
	}

	// Geometry re-keys.
	if _, ok := m.frame(c, 1, tl, "day", heroFrameTwoPane, 70, 16, -1); !ok {
		t.Fatal("resized render failed")
	}
	if got := m.Builds(); got != 2 {
		t.Fatalf("resize builds = %d, want 2", got)
	}

	// The pivot is a different body at the same geometry: it must re-key.
	if _, ok := m.frame(c, 1, tl, "day", heroFrameLeverage, 70, 16, -1); !ok {
		t.Fatal("pivot render failed")
	}
	if got := m.Builds(); got != 3 {
		t.Fatalf("mode change builds = %d, want 3", got)
	}

	// A new generation re-keys even over the same slice.
	if _, ok := m.frame(c, 2, tl, "day", heroFrameLeverage, 70, 16, -1); !ok {
		t.Fatal("new-generation render failed")
	}
	if got := m.Builds(); got != 4 {
		t.Fatalf("new generation builds = %d, want 4", got)
	}
}

// TestHeroMemoMatchesDirectRender proves the paint→render→clear post-pass is
// indistinguishable from a body built with the highlight baked in, including
// after highlight cycles on the retained canvases (the clear must restore the
// columns exactly). The comparison build shares the memo's detent lock, so both
// sides settle on the same scales.
func TestHeroMemoMatchesDirectRender(t *testing.T) {
	c := memoTestCtx()
	tl := memoTestBuckets(7)
	m := NewHeroMemo()

	scrubs := []int{-1, 0, 3, 6, 99, -1, 3} // repeats + out-of-range → -1
	for _, idx := range scrubs {
		direct, ok := buildHeroFrame(c, tl, "day", heroFrameTwoPane, 60, 16, &m.lock)
		if !ok {
			t.Fatalf("direct build(scrub=%d) failed", idx)
		}
		want := direct.render(c, idx)
		got, ok := m.frame(c, 1, tl, "day", heroFrameTwoPane, 60, 16, idx)
		if !ok {
			t.Fatalf("frame(scrub=%d) failed", idx)
		}
		if got != want {
			t.Fatalf("memoized frame diverges from direct render at scrub=%d", idx)
		}
	}
	if got := m.Builds(); got != 1 {
		t.Fatalf("highlight cycling rebuilt the panes: builds = %d, want 1", got)
	}
}

// TestHeroMemoSparkKeying: sparklines are built once per (generation, series,
// width) and cleared with the generation.
func TestHeroMemoSparkKeying(t *testing.T) {
	m := NewHeroMemo()
	tl := memoTestBuckets(3)
	calls := 0
	build := func() string { calls++; return "SPARK" }

	if got := m.spark(1, tl, "input", 20, build); got != "SPARK" {
		t.Fatalf("spark = %q", got)
	}
	m.spark(1, tl, "input", 20, build)
	if calls != 1 {
		t.Fatalf("warm spark rebuilt: %d calls, want 1", calls)
	}
	m.spark(1, tl, "input", 24, build) // width is part of the key
	if calls != 2 {
		t.Fatalf("width change did not rebuild: %d calls, want 2", calls)
	}
	m.spark(2, tl, "input", 24, build) // generation change clears everything
	if calls != 3 {
		t.Fatalf("generation change did not rebuild: %d calls, want 3", calls)
	}
}
