package views

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

var ansiSys = regexp.MustCompile("\x1b\\[[0-9;]*m")

func sysTestCtx() Ctx {
	return Ctx{
		Faint:     lipgloss.NewStyle(),
		StatLabel: lipgloss.NewStyle(),
		Subtle:    lipgloss.NewStyle(),
		GoodColor: lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#56D364"},
		NowColor:  lipgloss.AdaptiveColor{Light: "#B5780A", Dark: "#F2B441"},
		WarnColor: lipgloss.AdaptiveColor{Light: "#C0362C", Dark: "#E5534B"},
	}
}

func sysTestGauges() []SysGauge {
	return []SysGauge{
		{Label: "cpu", Frac: 0.38, Text: "0.8/2 cpu", Known: true},
		{Label: "mem", Frac: 0.72, Text: "2.9G/4.0G", Known: true},
		{Label: "disk", Frac: 0.93, Text: "28G/30G", Known: true},
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
// renders a muted "…" rather than a misleading 0%.
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
