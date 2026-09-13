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
	history := []float64{0.12, 0.28, 0.19, 0.44, 0.38}
	return []SysGauge{
		{Label: "cpu", Frac: 0.38, Text: "0.8/2 cpu", Known: true, History: history},
		{Label: "mem", Frac: 0.72, Text: "2.9G/4.0G", Known: true, History: history},
		{Label: "disk", Frac: 0.93, Text: "28G/30G", Known: true, History: history},
	}
}

// TestSysStripNeverOverflows is the load-bearing invariant: the strip must fit
// within the width it is given at every size, or it would push the Overview
// layout off-screen.
func TestSysStripNeverOverflows(t *testing.T) {
	c := sysTestCtx()
	for w := 8; w <= 240; w++ {
		out := SysStrip(c, sysTestGauges(), w)
		if got := lipgloss.Width(ansiSys.ReplaceAllString(out, "")); got > w {
			t.Fatalf("w=%d: strip width %d exceeds budget", w, got)
		}
	}
}

func TestSysStripResponsiveComposition(t *testing.T) {
	c := sysTestCtx()
	narrow := ansiSys.ReplaceAllString(SysStrip(c, sysTestGauges(), 42), "")
	if got := lipgloss.Height(narrow); got != 3 {
		t.Fatalf("narrow strip height = %d, want one row per gauge", got)
	}
	if got := strings.Count(narrow, "▕"); got != 3 {
		t.Fatalf("narrow strip rendered %d utilization bars, want one per gauge: %q", got, narrow)
	}
	if got := lipgloss.Height(SysStrip(c, sysTestGauges(), 120)); got != 2 {
		t.Fatalf("wide strip height = %d, want reading plus utilization bar", got)
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
	for _, want := range []string{"38%", "72%", "93%", "cpu", "mem", "disk", "0.8/2 cpu", "2.9G/4.0G", "28G/30G"} {
		if !strings.Contains(out, want) {
			t.Errorf("strip missing %q in %q", want, out)
		}
	}
}

func TestMachineGaugeBarShowsLiveFillAndBoundary(t *testing.T) {
	rendered := machineGaugeBar(sysTestCtx(), sysTestGauges()[0], 20)
	if strings.Contains(rendered, "\x1b[48") {
		t.Fatalf("utilization bar painted a background: %q", rendered)
	}
	out := ansiSys.ReplaceAllString(rendered, "")
	if !strings.HasPrefix(out, "▕") || !strings.HasSuffix(out, "▏") {
		t.Fatalf("utilization bar boundaries missing: %q", out)
	}
	if !strings.Contains(out, "█") || !strings.Contains(out, "░") {
		t.Fatalf("utilization bar does not distinguish fill from remainder: %q", out)
	}
	if got := strings.Count(out, "█"); got != 7 {
		t.Fatalf("38%% utilization filled %d of 18 cells, want 7: %q", got, out)
	}
}

func TestGaugeReadingUsesBoldSemanticStyle(t *testing.T) {
	c := sysTestCtx()
	style := gaugeStyle(c, 0.38)
	if !style.GetBold() {
		t.Fatal("known gauge label and value are not bold")
	}
	if got := style.GetForeground(); got != c.GoodColor {
		t.Fatalf("healthy gauge foreground = %v, want %v", got, c.GoodColor)
	}
	if !gaugeNoteStyle(c).GetItalic() {
		t.Fatal("gauge readout is not italic")
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
