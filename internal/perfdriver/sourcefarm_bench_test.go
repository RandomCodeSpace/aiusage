package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/RandomCodeSpace/aiusage/adapter/all"
	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/store"
)

// BenchmarkSourceFarmUnchanged measures one production-shaped steady-state
// collection pass. Fixture creation and the warm catch-up happen outside this
// binary; the timed operation is exactly RunOnce over all 15 adapters and all
// 2,500 sources with persisted checkpoints.
func BenchmarkSourceFarmUnchanged(b *testing.B) {
	root := os.Getenv("AIUSAGE_SOURCE_FARM")
	dbPath := os.Getenv("AIUSAGE_SOURCE_FARM_DB")
	if root == "" || dbPath == "" {
		b.Skip("AIUSAGE_SOURCE_FARM and AIUSAGE_SOURCE_FARM_DB are required")
	}

	err := withNeutralSourceFarmEnvironment(func() error {
		ledger, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer ledger.Close()
		ctx := context.Background()
		reg := all.Default()
		dc := sourceFarmConfig(root)

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			stats, err := collect.RunOnce(ctx, reg, ledger, dc)
			if err != nil {
				return err
			}
			if len(stats.Errors) > 0 {
				return &sourceFarmCycleError{errors: stats.Errors}
			}
			inserted := stats.EventsInserted + stats.ActivityInserted + stats.TurnContextsInserted
			if inserted != 0 {
				return &sourceFarmCycleError{inserted: inserted}
			}
			b.ReportMetric(float64(stats.Sources), "sources/op")
			b.ReportMetric(float64(inserted), "inserted/op")
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}

type sourceFarmCycleError struct {
	errors   []string
	inserted int
}

func (e *sourceFarmCycleError) Error() string {
	if len(e.errors) > 0 {
		return "source-farm cycle errors: " + e.errors[0]
	}
	return fmt.Sprintf("source-farm unchanged cycle inserted %d records", e.inserted)
}
