package views

import (
	"testing"
)

// TestComputeLayoutTooSmall: below the absolute floor the only thing rendered is
// the resize card; everything else is suppressed.
func TestComputeLayoutTooSmall(t *testing.T) {
	for _, c := range []struct{ w, h int }{
		{0, 0}, {39, 30}, {120, 9}, {39, 9}, {1, 1}, {40, 9}, {39, 10},
	} {
		got := ComputeLayout(c.w, c.h)
		if !got.TooSmall {
			t.Fatalf("ComputeLayout(%d,%d).TooSmall = false, want true", c.w, c.h)
		}
	}
	// At exactly the floor it is usable.
	if ComputeLayout(40, 10).TooSmall {
		t.Fatal("ComputeLayout(40,10).TooSmall = true, want usable")
	}
}

// TestComputeLayoutNavMode: nav degrades rail → tabs → mini as width shrinks.
func TestComputeLayoutNavMode(t *testing.T) {
	cases := []struct {
		w    int
		want NavMode
	}{
		{200, NavTabs}, // top tab strip at every usable width
		{120, NavTabs},
		{80, NavTabs},
		{64, NavTabs},
		{44, NavMini}, // only the icon row fits
	}
	for _, c := range cases {
		got := ComputeLayout(c.w, 40).Nav
		if got != c.want {
			t.Fatalf("ComputeLayout(%d,40).Nav = %v, want %v", c.w, got, c.want)
		}
	}
}

// TestComputeLayoutBodyFitsWidth: the body and its columns never exceed the
// terminal width — the core anti-overflow invariant, checked across a sweep.
func TestComputeLayoutBodyFitsWidth(t *testing.T) {
	for w := 40; w <= 260; w += 3 {
		for h := 10; h <= 60; h += 7 {
			l := ComputeLayout(w, h)
			if l.TooSmall {
				continue
			}
			if l.BodyW > w {
				t.Fatalf("(%d,%d): BodyW %d > W %d", w, h, l.BodyW, w)
			}
			if l.BodyW < 1 || l.BodyH < 1 {
				t.Fatalf("(%d,%d): non-positive body %dx%d", w, h, l.BodyW, l.BodyH)
			}
			if l.Nav == NavRail && l.RailW+1+l.BodyW > w {
				t.Fatalf("(%d,%d): rail %d + gutter + body %d > W %d", w, h, l.RailW, l.BodyW, w)
			}
			if l.SidePanel {
				if l.MainW+1+l.SideW > l.BodyW {
					t.Fatalf("(%d,%d): main %d + gutter + side %d > body %d", w, h, l.MainW, l.SideW, l.BodyW)
				}
				if l.SideW < 1 || l.MainW < 1 {
					t.Fatalf("(%d,%d): non-positive split main=%d side=%d", w, h, l.MainW, l.SideW)
				}
			} else if l.MainW > l.BodyW {
				t.Fatalf("(%d,%d): MainW %d > BodyW %d (no side)", w, h, l.MainW, l.BodyW)
			}
		}
	}
}

// TestComputeLayoutBodyFitsHeight: chrome rows + body height never exceed H.
func TestComputeLayoutBodyFitsHeight(t *testing.T) {
	for w := 40; w <= 260; w += 5 {
		for h := 10; h <= 60; h += 3 {
			l := ComputeLayout(w, h)
			if l.TooSmall {
				continue
			}
			chrome := 0
			for _, b := range []bool{l.ShowHeader, l.ShowBreadcrumb, l.ShowFooter, l.ShowTabStrip} {
				if b {
					chrome++
				}
			}
			if chrome+l.BodyH > h {
				t.Fatalf("(%d,%d): chrome %d + body %d > H %d", w, h, chrome, l.BodyH, h)
			}
		}
	}
}

// TestComputeLayoutSidePanelOnlyWhenWide: a side panel never appears unless both
// columns clear their minimums; it must appear on a roomy body.
func TestComputeLayoutSidePanelOnlyWhenWide(t *testing.T) {
	if ComputeLayout(50, 40).SidePanel {
		t.Fatal("side panel should not appear at body derived from 50 cols")
	}
	if !ComputeLayout(160, 40).SidePanel {
		t.Fatal("side panel should appear at 160 cols")
	}
}

// TestComputeLayoutChartModeDegrades: the chart mode steps down as space shrinks
// and never reports Full when there isn't room.
func TestComputeLayoutChartModeDegrades(t *testing.T) {
	full := ComputeLayout(160, 40)
	if full.ChartMode != ChartFull {
		t.Fatalf("160x40 ChartMode = %v, want Full", full.ChartMode)
	}
	short := ComputeLayout(160, 11) // very short height
	if short.ChartMode == ChartFull {
		t.Fatalf("160x11 ChartMode = Full, want a degraded mode")
	}
	narrow := ComputeLayout(44, 40)
	if narrow.ChartMode == ChartFull {
		t.Fatalf("44x40 ChartMode = Full, want a degraded mode")
	}
}

// TestComputeLayoutUltrawideCap: the primary column is capped so charts/tables
// don't stretch uselessly on very wide terminals.
func TestComputeLayoutUltrawideCap(t *testing.T) {
	l := ComputeLayout(400, 50)
	if l.MainW > maxMainW {
		t.Fatalf("MainW %d exceeds cap %d", l.MainW, maxMainW)
	}
}
