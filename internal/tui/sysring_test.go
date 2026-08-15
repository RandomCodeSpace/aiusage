package tui

import (
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/sysmon"
)

// TestSysRingEmpty: a ring nothing was pushed into reports no samples at all.
// The strip turns that into holes, never into a window of zeros.
func TestSysRingEmpty(t *testing.T) {
	var r sysRing
	if got := r.values(); len(got) != 0 {
		t.Fatalf("empty ring returned %d samples", len(got))
	}
}

// TestSysRingPartialFill: a young ring returns exactly what it holds, oldest
// first, and stays short of its capacity.
func TestSysRingPartialFill(t *testing.T) {
	var r sysRing
	for _, v := range []float64{0.1, 0.2, 0.3} {
		r.push(v)
	}
	got := r.values()
	want := []float64{0.1, 0.2, 0.3}
	if len(got) != len(want) {
		t.Fatalf("values() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("values()[%d] = %v, want %v (order is oldest-first)", i, got[i], want[i])
		}
	}
}

// TestSysRingWraps: past capacity the oldest samples are dropped and the newest
// one stays last, which is what makes the strip roll.
func TestSysRingWraps(t *testing.T) {
	var r sysRing
	const extra = 5
	for i := 0; i < sysRingCap+extra; i++ {
		r.push(float64(i))
	}
	got := r.values()
	if len(got) != sysRingCap {
		t.Fatalf("wrapped ring holds %d samples, cap is %d", len(got), sysRingCap)
	}
	if got[0] != extra {
		t.Fatalf("oldest retained sample = %v, want %v", got[0], float64(extra))
	}
	if last := got[len(got)-1]; last != float64(sysRingCap+extra-1) {
		t.Fatalf("newest sample = %v, want %v", last, float64(sysRingCap+extra-1))
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("wrapped order broke at %d: %v then %v", i, got[i-1], got[i])
		}
	}
}

// TestSysRingValuesAreACopy: the ring is copied with the Model on every message,
// so the slice it hands the view must not alias the buffer a later push writes.
func TestSysRingValuesAreACopy(t *testing.T) {
	var r sysRing
	r.push(0.4)
	got := r.values()
	r.push(0.9)
	if got[0] != 0.4 {
		t.Fatalf("a later push rewrote a handed-out sample: %v", got[0])
	}
}

// TestSysHistorySkipsUnknown: a gauge the monitor could not read contributes no
// sample - an unreadable CPU is not an idle one - while the readable gauges of
// the same snapshot still record.
func TestSysHistorySkipsUnknown(t *testing.T) {
	var h sysHistory
	h.push(sysmon.Snapshot{
		CPU:  sysmon.Gauge{Frac: 0.5, Known: false},
		Mem:  sysmon.Gauge{Frac: 0.25, Known: true},
		Disk: sysmon.Gauge{Frac: 0.75, Known: true},
	})
	if got := h.cpu.values(); len(got) != 0 {
		t.Fatalf("unknown CPU recorded %v", got)
	}
	if got := h.mem.values(); len(got) != 1 || got[0] != 0.25 {
		t.Fatalf("mem history = %v, want [0.25]", got)
	}
	if got := h.disk.values(); len(got) != 1 || got[0] != 0.75 {
		t.Fatalf("disk history = %v, want [0.75]", got)
	}
}

// TestSysTickAccumulatesHistory: the tick handler is what grows the window, and
// the gauges it hands the view carry it.
func TestSysTickAccumulatesHistory(t *testing.T) {
	m := Model{mon: sysmon.New("")}
	for i := 0; i < 3; i++ {
		next, _ := m.handleSysTick(sysTickMsg{snap: sysmon.Snapshot{
			CPU:  sysmon.Gauge{Frac: 0.1 * float64(i+1), Known: true},
			Mem:  sysmon.Gauge{Frac: 0.5, Known: true},
			Disk: sysmon.Gauge{Frac: 0.5, Known: true},
		}})
		m = next.(Model)
	}
	g := m.sysGauges()
	if len(g) != 3 {
		t.Fatalf("sysGauges() = %d gauges, want 3", len(g))
	}
	if len(g[0].History) != 3 {
		t.Fatalf("cpu history = %v, want 3 samples", g[0].History)
	}
	if last := g[0].History[2]; last < 0.29 || last > 0.31 {
		t.Fatalf("newest cpu sample = %v, want the latest tick (0.3)", last)
	}
}
