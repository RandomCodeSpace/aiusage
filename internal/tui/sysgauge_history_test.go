package tui

import (
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/sysmon"
)

func TestAppendSysHistoryBoundsAndCopies(t *testing.T) {
	var history sysHistory
	for i := 0; i < sysHistoryLimit+5; i++ {
		history = appendSysHistory(history, knownSnapshot(float64(i)/100))
	}

	for i, series := range history {
		if len(series) != sysHistoryLimit {
			t.Fatalf("series %d length = %d, want %d", i, len(series), sysHistoryLimit)
		}
	}
	if got := history[0][0]; got != 0.05 {
		t.Fatalf("oldest retained CPU sample = %v, want 0.05", got)
	}

	previous := history
	next := appendSysHistory(history, knownSnapshot(0.91))
	next[0][0] = 1
	if previous[0][0] != 0.05 {
		t.Fatalf("append mutated previous model history: got %v", previous[0][0])
	}
}

func TestAppendSysHistoryResetsOnlyMissingSeries(t *testing.T) {
	history := appendSysHistory(sysHistory{}, knownSnapshot(0.25))
	snap := knownSnapshot(0.5)
	snap.CPU = sysmon.Gauge{}
	history = appendSysHistory(history, snap)

	if history[0] != nil {
		t.Fatalf("missing CPU should reset its line, got %v", history[0])
	}
	if len(history[1]) != 2 || len(history[2]) != 2 {
		t.Fatalf("known series should continue: mem=%v disk=%v", history[1], history[2])
	}
}

func TestSysGaugesCarriesRecentHistory(t *testing.T) {
	m := Model{
		sys:        knownSnapshot(0.42),
		sysHistory: sysHistory{{0.2, 0.42}, {0.3, 0.42}, {0.4, 0.42}},
	}
	gauges := m.sysGauges()
	if len(gauges) != 3 {
		t.Fatalf("gauge count = %d, want 3", len(gauges))
	}
	for i, gauge := range gauges {
		if len(gauge.History) != 2 {
			t.Fatalf("gauge %d history length = %d, want 2", i, len(gauge.History))
		}
	}
}

func knownSnapshot(frac float64) sysmon.Snapshot {
	gauge := sysmon.Gauge{Frac: frac, Text: "reading", Known: true}
	return sysmon.Snapshot{CPU: gauge, Mem: gauge, Disk: gauge}
}
