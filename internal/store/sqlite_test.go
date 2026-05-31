package store

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// openTemp opens a fresh SQLite store in a temp dir.
func openTemp(t *testing.T) *SQLite {
	t.Helper()
	db := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func ev(dedup, tool string, et time.Time, total int64) model.UsageEvent {
	return model.UsageEvent{
		Tool:        tool,
		Model:       "m",
		SessionID:   "s",
		EventTime:   et,
		TotalTokens: total,
		InputTokens: total,
		DedupKey:    dedup,
		Kind:        model.KindUsage,
	}
}

// TestInsertOrIgnoreIdempotent verifies re-inserting the same dedup key is a
// no-op and never errors (the append-only INSERT OR IGNORE contract).
func TestInsertOrIgnoreIdempotent(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	e := ev("k1", model.ToolCodex, time.Now(), 100)

	n, err := st.InsertEvents(ctx, []model.UsageEvent{e})
	if err != nil || n != 1 {
		t.Fatalf("first insert n=%d err=%v want 1,nil", n, err)
	}
	n, err = st.InsertEvents(ctx, []model.UsageEvent{e})
	if err != nil || n != 0 {
		t.Fatalf("second insert n=%d err=%v want 0,nil (idempotent)", n, err)
	}
}

// TestEmptyDedupKeyRejected ensures events without a dedup key are rejected.
func TestEmptyDedupKeyRejected(t *testing.T) {
	st := openTemp(t)
	e := ev("", model.ToolCodex, time.Now(), 1)
	if _, err := st.InsertEvents(context.Background(), []model.UsageEvent{e}); err == nil {
		t.Fatalf("expected error for empty dedup key")
	}
}

// TestImmutabilityUpdateForbidden proves the no-UPDATE trigger fires on the
// historical table, the core append-only guarantee at the storage layer.
func TestImmutabilityUpdateForbidden(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("k1", model.ToolCodex, time.Now(), 100)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := st.db.ExecContext(ctx, `UPDATE usage_events SET total_tokens = 0 WHERE dedup_key='k1'`)
	if err == nil {
		t.Fatalf("UPDATE on usage_events should be forbidden")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort, got %v", err)
	}
}

// TestImmutabilityDeleteForbidden proves the no-DELETE trigger fires.
func TestImmutabilityDeleteForbidden(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("k1", model.ToolCodex, time.Now(), 100)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := st.db.ExecContext(ctx, `DELETE FROM usage_events WHERE dedup_key='k1'`)
	if err == nil {
		t.Fatalf("DELETE on usage_events should be forbidden")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only abort, got %v", err)
	}
}

// TestDurabilitySurvivesReopenAndCompaction is the storage-layer twin of the
// collector invariant: 2,000,000 tokens written, then the DB is closed and
// reopened (simulating a daemon restart) and a re-poll inserts only NEW keys.
// A window total must never erode.
func TestDurabilitySurvivesReopenAndCompaction(t *testing.T) {
	db := filepath.Join(t.TempDir(), "usage.db")
	ctx := context.Background()
	start := time.Date(2026, 5, 29, 6, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)

	st, err := Open(db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	full := make([]model.UsageEvent, 0, 20)
	for i := 0; i < 20; i++ {
		full = append(full, ev("cc|"+strconv.Itoa(i), model.ToolClaudeCode, start.Add(time.Duration(i)*30*time.Second), 100_000))
	}
	if _, err := st.InsertEvents(ctx, full); err != nil {
		t.Fatalf("insert full: %v", err)
	}
	if got := windowTot(t, st, start, end); got != 2_000_000 {
		t.Fatalf("after write total=%d want 2,000,000", got)
	}
	st.Close()

	// Reopen and re-poll a now-empty/compacted source: re-inserting the same
	// keys must be a no-op, and the historical total stays put.
	st2, err := Open(db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if n, err := st2.InsertEvents(ctx, full); err != nil || n != 0 {
		t.Fatalf("re-poll inserted n=%d err=%v want 0,nil", n, err)
	}
	if got := windowTot(t, st2, start, end); got != 2_000_000 {
		t.Fatalf("after reopen+compaction total=%d want still 2,000,000", got)
	}
}

func windowTot(t *testing.T, st *SQLite, since, until time.Time) int64 {
	t.Helper()
	sum, err := st.Summarize(context.Background(), Filter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return sum.Totals.Total
}

// TestSummarizeGroupingAndTotals verifies grouped buckets carry keys and the
// grand total reflects the whole filtered set regardless of grouping.
func TestSummarizeGroupingAndTotals(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	in := []model.UsageEvent{
		ev("a", model.ToolClaudeCode, now, 100),
		ev("b", model.ToolClaudeCode, now, 200),
		ev("c", model.ToolCodex, now, 50),
	}
	if _, err := st.InsertEvents(ctx, in); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Buckets) != 2 {
		t.Fatalf("buckets=%d want 2", len(sum.Buckets))
	}
	if sum.Totals.Total != 350 {
		t.Fatalf("grand total=%d want 350", sum.Totals.Total)
	}
	byTool := map[string]int64{}
	for _, b := range sum.Buckets {
		if len(b.OrderedKeys) != 1 || b.OrderedKeys[0] != "tool" {
			t.Fatalf("bucket OrderedKeys=%v want [tool]", b.OrderedKeys)
		}
		byTool[b.Keys["tool"]] = b.Total
	}
	if byTool[model.ToolClaudeCode] != 300 || byTool[model.ToolCodex] != 50 {
		t.Fatalf("per-tool totals = %v", byTool)
	}
}

// TestSummarizeTimeBucketLocalDay checks day grouping produces a lexically
// sortable local-date key.
func TestSummarizeTimeBucketLocalDay(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	// Use local noon so the local-date bucket is unambiguous regardless of tz.
	day := time.Date(2026, 5, 29, 12, 0, 0, 0, time.Local)
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{ev("d1", model.ToolCodex, day, 10)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"day"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Buckets) != 1 {
		t.Fatalf("buckets=%d want 1", len(sum.Buckets))
	}
	if got := sum.Buckets[0].Keys["day"]; got != "2026-05-29" {
		t.Fatalf("day bucket key=%q want 2026-05-29", got)
	}
}

// TestStateRoundTrip verifies LastState/UpsertState keep one row per cell.
func TestStateRoundTrip(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if v, err := st.LastState(ctx, model.ToolHermes, "k"); err != nil || v != nil {
		t.Fatalf("empty LastState=%v err=%v want nil,nil", v, err)
	}
	snap := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "k", SessionID: "k",
		InputTokens: 100, TotalTokens: 100, ObservedTime: time.Now(),
	}
	if err := st.UpsertState(ctx, snap); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	snap.TotalTokens = 250
	snap.InputTokens = 250
	if err := st.UpsertState(ctx, snap); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err := st.LastState(ctx, model.ToolHermes, "k")
	if err != nil || got == nil {
		t.Fatalf("LastState=%v err=%v", got, err)
	}
	if got.TotalTokens != 250 {
		t.Fatalf("state total=%d want 250 (one row per cell)", got.TotalTokens)
	}
}

// TestStatsAndSourceStats checks the diagnostic aggregates.
func TestStatsAndSourceStats(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{
		ev("a", model.ToolClaudeCode, now, 100),
		ev("b", model.ToolCodex, now.Add(time.Hour), 50),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Events != 2 || stats.DistinctTools != 2 {
		t.Fatalf("stats events=%d tools=%d want 2,2", stats.Events, stats.DistinctTools)
	}
	if stats.SchemaVersion != schemaVersion {
		t.Fatalf("schema version=%d want %d", stats.SchemaVersion, schemaVersion)
	}
	if stats.SizeBytes <= 0 {
		t.Fatalf("size bytes=%d want >0", stats.SizeBytes)
	}

	ss, err := st.SourceStats(ctx)
	if err != nil {
		t.Fatalf("source stats: %v", err)
	}
	if len(ss) != 2 {
		t.Fatalf("source stats len=%d want 2", len(ss))
	}
	// Ordered by total desc; claude-code (100) first.
	if ss[0].Tool != model.ToolClaudeCode || ss[0].Total != 100 {
		t.Fatalf("first source stat = %+v", ss[0])
	}
	if ss[0].Sessions != 1 {
		t.Fatalf("sessions=%d want 1", ss[0].Sessions)
	}
}
