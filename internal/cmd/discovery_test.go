package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/config"
)

// A discovery sweep that never gets to run must report every tool as unknown -
// absent from the map - rather than as zero sources. The TUI turns a zero into
// the sentence "configured, no data source", which would be a lie about the
// machine whenever the sweep was merely cut short (issue #44).
func TestDiscoveredSourcesReportsUnknownWhenCutShort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := config.Default()
	got := discoveredSources(ctx, cfg)

	for tool, n := range got {
		t.Errorf("cancelled discovery reported %s = %d sources; want the tool absent (unknown)", tool, n)
	}
}

// The sweep runs before the first frame, so it must be bounded: a hung or
// unreachable source root must not keep the dashboard from opening.
func TestDiscoveredSourcesIsBounded(t *testing.T) {
	if discoveryBudget <= 0 {
		t.Fatalf("discoveryBudget = %v; startup discovery must be bounded", discoveryBudget)
	}

	done := make(chan map[string]int, 1)
	go func() { done <- discoveredSources(context.Background(), config.Default()) }()

	select {
	case <-done:
	case <-time.After(discoveryBudget + 5*time.Second):
		t.Fatal("discoveredSources did not return within its budget; startup would block")
	}
}
