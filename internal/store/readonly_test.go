package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// seedForReadOnly creates a current-version database holding two events and
// returns its path, with the read-write handle already closed.
func seedForReadOnly(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	at := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.InsertEvents(context.Background(), []model.UsageEvent{
		rollupEvent("ro-1", model.ToolCodex, "gpt-5", "/w/alpha", at, 100),
		rollupEvent("ro-2", model.ToolClaudeCode, "claude-sonnet-4-6", "/w/beta", at.Add(time.Hour), -1),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// TestOpenReadOnlyServesReads is the point of the handle: everything the
// reporting surfaces need works through it.
func TestOpenReadOnlyServesReads(t *testing.T) {
	path := seedForReadOnly(t)
	ctx := context.Background()

	st, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer st.Close()

	sum, err := st.Summarize(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Buckets) != 2 || sum.Totals.Events != 2 {
		t.Fatalf("summarize buckets=%d events=%d want 2,2", len(sum.Buckets), sum.Totals.Events)
	}
	roll, err := st.SummarizeRollup(ctx, Filter{GroupBy: []string{"tool"}})
	if err != nil {
		t.Fatalf("summarize rollup: %v", err)
	}
	if roll.Totals.Events != 2 || roll.Totals.UnpricedEvents != 1 {
		t.Fatalf("rollup events=%d unpriced=%d want 2,1", roll.Totals.Events, roll.Totals.UnpricedEvents)
	}
	evs, err := st.ListEvents(ctx, Filter{})
	if err != nil || len(evs) != 2 {
		t.Fatalf("list events n=%d err=%v want 2,nil", len(evs), err)
	}
	if _, err := st.SourceStats(ctx); err != nil {
		t.Fatalf("source stats: %v", err)
	}
	if _, err := st.Stats(ctx); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if _, err := st.Checkpoint(ctx, model.ToolCodex, "/src/a"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// TestOpenReadOnlyRefusesEveryWrite pins the guarantee a serving process is
// given: the handle cannot write the ledger, and it says so itself rather than
// surfacing a driver error from three layers down.
func TestOpenReadOnlyRefusesEveryWrite(t *testing.T) {
	path := seedForReadOnly(t)
	ctx := context.Background()

	st, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer st.Close()

	at := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	ev := rollupEvent("ro-write", model.ToolCodex, "gpt-5", "/w/alpha", at, 1)
	snap := model.AggregateSnapshot{Tool: model.ToolHermes, Key: "k", ObservedTime: at}
	cp := model.SourceCheckpoint{Tool: model.ToolCodex, SourcePath: "/src/a"}

	writes := []struct {
		name string
		err  error
	}{
		{"InsertEvents", func() error { _, e := st.InsertEvents(ctx, []model.UsageEvent{ev}); return e }()},
		{"ApplyEvents", func() error { _, e := st.ApplyEvents(ctx, []model.UsageEvent{ev}, &cp); return e }()},
		{"ApplySnapshot", func() error { _, e := st.ApplySnapshot(ctx, []model.UsageEvent{ev}, snap, nil); return e }()},
		{"UpsertState", st.UpsertState(ctx, snap)},
		{"RebuildRollup", st.RebuildRollup(ctx)},
		{"EnsureRollup", func() error { _, e := st.EnsureRollup(ctx); return e }()},
	}
	for _, w := range writes {
		if w.err == nil {
			t.Errorf("%s succeeded on a read-only handle", w.name)
			continue
		}
		if !strings.Contains(w.err.Error(), "read-only") {
			t.Errorf("%s failed with %v; the refusal must name the read-only handle", w.name, w.err)
		}
	}

	// Nothing reached the file.
	rw, err := Open(path)
	if err != nil {
		t.Fatalf("reopen read-write: %v", err)
	}
	defer rw.Close()
	evs, err := rw.ListEvents(ctx, Filter{})
	if err != nil || len(evs) != 2 {
		t.Fatalf("post-refusal events=%d err=%v want 2,nil", len(evs), err)
	}
}

// TestOpenReadOnlyRefusesOlderSchema: an older database is refused, NOT
// migrated. Migrating would be a write, and this handle exists precisely
// because a serving process must not be able to make one.
func TestOpenReadOnlyRefusesOlderSchema(t *testing.T) {
	ctx := context.Background()
	path := legacyDB(t, 3)

	st, err := OpenReadOnly(path)
	if err == nil {
		st.Close()
		t.Fatalf("expected refusal opening a v3 database read-only")
	}
	for _, want := range []string{"v3", fmt.Sprintf("v%d", SchemaVersion), path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}

	// The file must be untouched: still v3, still without the v4 table.
	raw := rawDB(t, path)
	if v, err := readSchemaVersion(ctx, raw); err != nil || v != 3 {
		t.Fatalf("post-refusal version=%d err=%v want 3,nil", v, err)
	}
	ok, err := tableExists(ctx, raw, "usage_rollup")
	if err != nil {
		t.Fatalf("check rollup table: %v", err)
	}
	if ok {
		t.Fatalf("a read-only open created the v4 table; it must never migrate")
	}
}

// TestOpenReadOnlyRefusesNewerSchema is the other direction: a database written
// by a newer binary is refused rather than served through a schema this build
// does not understand.
func TestOpenReadOnlyRefusesNewerSchema(t *testing.T) {
	path := seedForReadOnly(t)
	newer := SchemaVersion + 1
	raw := rawDB(t, path)
	if _, err := raw.Exec(`UPDATE schema_meta SET value=? WHERE key='schema_version'`,
		strconv.Itoa(newer)); err != nil {
		t.Fatalf("stamp newer: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := OpenReadOnly(path)
	if err == nil {
		st.Close()
		t.Fatalf("expected refusal opening a v%d database read-only", newer)
	}
	for _, want := range []string{fmt.Sprintf("v%d", newer), fmt.Sprintf("v%d", SchemaVersion)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
}

// TestOpenReadOnlyLeavesFileModeAlone: Open tightens the database to 0600
// because raw can hold transcript content. A read-only handle must not, since
// chmod is a change to a file a reader was given no mandate over.
func TestOpenReadOnlyLeavesFileModeAlone(t *testing.T) {
	path := seedForReadOnly(t)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	st, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer st.Close()
	// Touch the file through the handle so a lazy connection is really opened.
	if _, err := st.Stats(context.Background()); err != nil {
		t.Fatalf("stats: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("file mode = %04o, want 0640 (a read-only open must not chmod)", got)
	}
}

// TestOpenReadOnlyRejectsMissingFile: mode=ro never creates, so a wrong path is
// an error at open rather than an empty database that reports zero usage.
func TestOpenReadOnlyRejectsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	st, err := OpenReadOnly(path)
	if err == nil {
		st.Close()
		t.Fatalf("expected an error opening a nonexistent database read-only")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatalf("a read-only open created %s", path)
	}
	if _, err := OpenReadOnly(""); err == nil {
		t.Fatalf("expected an error for an empty path")
	}
}
