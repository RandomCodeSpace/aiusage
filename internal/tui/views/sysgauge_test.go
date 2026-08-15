package views

import (
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
)

var ansiSys = regexp.MustCompile("\x1b\\[[0-9;]*m")

func sysTestCtx() Ctx {
	return Ctx{
		Faint:     lipgloss.NewStyle(),
		StatLabel: lipgloss.NewStyle(),
		Subtle:    lipgloss.NewStyle(),
		GoodColor: compat.AdaptiveColor{Light: lipgloss.Color("#1A7F37"), Dark: lipgloss.Color("#56D364")},
		NowColor:  compat.AdaptiveColor{Light: lipgloss.Color("#B5780A"), Dark: lipgloss.Color("#F2B441")},
		WarnColor: compat.AdaptiveColor{Light: lipgloss.Color("#C0362C"), Dark: lipgloss.Color("#E5534B")},
	}
}

func sysTestGauges() []SysGauge {
	hist := func(v float64) []float64 {
		out := make([]float64, 24)
		for i := range out {
			out[i] = v
		}
		return out
	}
	return []SysGauge{
		{Label: "cpu", Frac: 0.38, Text: "0.8/2 cpu", Known: true, History: hist(0.38)},
		{Label: "mem", Frac: 0.72, Text: "2.9G/4.0G", Known: true, History: hist(0.72)},
		{Label: "disk", Frac: 0.93, Text: "28G/30G", Known: true, History: hist(0.93)},
	}
}

// TestSysStripNeverOverflows is the load-bearing invariant: the strip must fit
// within the width it is given at every size, or it would push the Overview
// layout off-screen.
func TestSysStripNeverOverflows(t *testing.T) {
	c := sysTestCtx()
	for w := 12; w <= 240; w++ {
		out := SysStrip(c, sysTestGauges(), w)
		if got := lipgloss.Width(ansiSys.ReplaceAllString(out, "")); got > w {
			t.Fatalf("w=%d: strip width %d exceeds budget", w, got)
		}
	}
}

// TestSysStripUnknownPlaceholder: an unknown gauge (CPU before its 2nd sample)
// renders a muted "…" rather than a misleading 0%, and paints no heat at all -
// an unread resource is a hole, not an idle one.
func TestSysStripUnknownPlaceholder(t *testing.T) {
	c := sysTestCtx()
	g := []SysGauge{{Label: "cpu", Known: false}}
	out := ansiSys.ReplaceAllString(SysStrip(c, g, 40), "")
	if !strings.Contains(out, "…") {
		t.Errorf("unknown gauge should show a … placeholder, got %q", out)
	}
	if strings.Contains(out, "0%") {
		t.Errorf("unknown gauge must not show a misleading 0%%, got %q", out)
	}
	if strings.ContainsAny(out, "░▒▓█·") {
		t.Errorf("unknown gauge painted heat cells, got %q", out)
	}
}

// TestSysStripHeatIsAbsolute: the strips are read against the resource's own
// ceiling, never self-scaled. A busy gauge is hot and a quiet one is cool at the
// same instant, which is exactly what per-strip scaling would destroy.
func TestSysStripHeatIsAbsolute(t *testing.T) {
	c := sysTestCtx()
	g := []SysGauge{
		{Label: "cpu", Frac: 0.05, Known: true, History: []float64{0.05, 0.05}},
		{Label: "mem", Frac: 0.93, Known: true, History: []float64{0.93, 0.93}},
	}
	out := ansiSys.ReplaceAllString(SysStrip(c, g, 80), "")
	cpu, mem, ok := strings.Cut(out, "mem")
	if !ok {
		t.Fatalf("strip did not render both gauges: %q", out)
	}
	if strings.ContainsAny(cpu, "▓█") {
		t.Errorf("a 5%% cpu painted a hot rung: %q", cpu)
	}
	if !strings.Contains(mem, "█") {
		t.Errorf("a 93%% mem did not reach the top rung: %q", mem)
	}
}

// TestSysStripHoleVsZero: a strip narrower than its history is full, a young one
// leaves the OLD side blank, and a genuinely zero sample paints the track mark.
// Three different facts, three different marks.
func TestSysStripHoleVsZero(t *testing.T) {
	c := sysTestCtx()
	g := []SysGauge{{Label: "cpu", Frac: 0, Known: true, History: []float64{0, 0}}}
	out := ansiSys.ReplaceAllString(SysStrip(c, g, 40), "")
	if !strings.Contains(out, "··") {
		t.Errorf("zero samples should paint the track mark, got %q", out)
	}
	if strings.ContainsAny(out, "░▒▓█") {
		t.Errorf("zero samples painted a rung, got %q", out)
	}
	if !strings.Contains(out, "▕ ") {
		t.Errorf("a two-sample history should leave holes on the old side, got %q", out)
	}
}

// TestSysStripRollsWithHistory: the newest sample sits at the RIGHT edge of the
// strip, beside the live percentage - btop's arrangement, and the only one where
// the number and the cell next to it are the same reading.
func TestSysStripRollsWithHistory(t *testing.T) {
	c := sysTestCtx()
	hist := make([]float64, 60)
	for i := range hist {
		hist[i] = float64(i) / float64(len(hist)-1) // a 0 -> 1 ramp
	}
	g := []SysGauge{{Label: "cpu", Frac: 1, Known: true, History: hist}}
	out := ansiSys.ReplaceAllString(SysStrip(c, g, 40), "")
	strip := out[strings.Index(out, "▕")+len("▕") : strings.Index(out, "▏")]
	runes := []rune(strip)
	if len(runes) < 4 {
		t.Fatalf("strip too short to read: %q", strip)
	}
	if got := runes[len(runes)-1]; got != '█' {
		t.Errorf("newest (100%%) cell = %q, want █ at the right edge", got)
	}
	// A ramp must be monotone in ink: no rung may be denser than a later one,
	// and the oldest cell the window still shows must be cooler than the newest.
	rank := map[rune]int{'·': 0, '░': 1, '▒': 2, '▓': 3, '█': 4}
	if rank[runes[0]] >= rank[runes[len(runes)-1]] {
		t.Errorf("oldest cell %q is not cooler than the newest %q", runes[0], runes[len(runes)-1])
	}
	for i := 1; i < len(runes); i++ {
		if rank[runes[i]] < rank[runes[i-1]] {
			t.Fatalf("ramp went backwards at cell %d: %q", i, strip)
		}
	}
}

// TestSysStripShowsPercent: a known gauge shows its rounded percentage + label.
func TestSysStripShowsPercent(t *testing.T) {
	c := sysTestCtx()
	out := ansiSys.ReplaceAllString(SysStrip(c, sysTestGauges(), 200), "")
	for _, want := range []string{"38%", "72%", "93%", "cpu", "mem", "disk"} {
		if !strings.Contains(out, want) {
			t.Errorf("strip missing %q in %q", want, out)
		}
	}
}

// TestSysStripEmpty returns "" for no gauges or a too-narrow row.
func TestSysStripEmpty(t *testing.T) {
	c := sysTestCtx()
	if SysStrip(c, nil, 100) != "" {
		t.Error("no gauges should yield empty strip")
	}
	if SysStrip(c, sysTestGauges(), 5) != "" {
		t.Error("too-narrow row should yield empty strip")
	}
}
