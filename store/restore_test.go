package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
)

func TestPrepareRestoreMigratesStageWithoutChangingInput(t *testing.T) {
	input := legacyDB(t, 3)
	before := snapshotFile(t, input)
	target := filepath.Join(t.TempDir(), "live.db")

	plan, err := PrepareRestore(context.Background(), input, target)
	if err != nil {
		t.Fatalf("PrepareRestore: %v", err)
	}
	defer plan.Cleanup()
	if plan.Verification.State != VerificationOK || plan.Verification.SchemaVersion != SchemaVersion {
		t.Fatalf("prepared restore = %+v", plan.Verification)
	}
	if after := snapshotFile(t, input); after != before {
		t.Fatalf("preparing an older backup changed the input: before=%+v after=%+v", before, after)
	}
	inputVerification, err := Verify(context.Background(), input)
	if err != nil || inputVerification.SchemaVersion != 3 || inputVerification.State != VerificationIncompatible {
		t.Fatalf("input after staged migration = %+v err=%v", inputVerification, err)
	}
}

func TestPrepareRestoreMigrationFailureLeavesInputAndTargetUntouched(t *testing.T) {
	input := legacyDB(t, 3)
	inputBefore := snapshotFile(t, input)
	target := filepath.Join(t.TempDir(), "live.db")
	seedSimpleRestoreStore(t, target, "live-before")
	targetBefore := snapshotFile(t, target)

	originalMigrations := migrations
	t.Cleanup(func() { migrations = originalMigrations })
	brokenMigrations := append([]migration(nil), originalMigrations...)
	for i := range brokenMigrations {
		if brokenMigrations[i].version == 4 {
			brokenMigrations[i] = migration{version: 4, statements: []string{
				`CREATE TABLE stage_change_must_roll_back (value INTEGER)`,
				`INSERT INTO missing_stage_table VALUES (1)`,
			}}
		}
	}
	migrations = brokenMigrations

	plan, err := PrepareRestore(context.Background(), input, target)
	if plan != nil {
		plan.Cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "migration v4") {
		t.Fatalf("PrepareRestore migration failure = %v", err)
	}
	if after := snapshotFile(t, input); after != inputBefore {
		t.Fatalf("failed staged migration changed input: before=%+v after=%+v", inputBefore, after)
	}
	if after := snapshotFile(t, target); after != targetBefore {
		t.Fatalf("failed staged migration changed target: before=%+v after=%+v", targetBefore, after)
	}
	stages, globErr := filepath.Glob(filepath.Join(filepath.Dir(target), ".restore-stage-*.db"))
	if globErr != nil || len(stages) != 0 {
		t.Fatalf("failed staged migration left stages=%v err=%v", stages, globErr)
	}
}

func TestApplyRestorePreservesCompleteSnapshotAndSafetyBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	snapshotDB := filepath.Join(dir, "snapshot-source.db")
	seedCompleteRestoreStore(t, snapshotDB, "restored")
	input := filepath.Join(dir, "input-backup.db")
	if _, err := Backup(ctx, snapshotDB, input); err != nil {
		t.Fatal(err)
	}
	inputBefore := snapshotFile(t, input)

	target := filepath.Join(dir, "live.db")
	seedSimpleRestoreStore(t, target, "live-before")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := ApplyRestore(ctx, plan, target, true)
	if err != nil {
		t.Fatalf("ApplyRestore: %v", err)
	}
	if !result.TargetUsable || result.RolledBack || result.SafetyBackupPath == "" || !result.EventsMayBeAbsent {
		t.Fatalf("restore result = %+v", result)
	}
	if after := snapshotFile(t, input); after != inputBefore {
		t.Fatalf("restore changed input backup: before=%+v after=%+v", inputBefore, after)
	}
	verification, err := Verify(ctx, target)
	if err != nil || verification.State != VerificationOK {
		t.Fatalf("restored target = %+v err=%v", verification, err)
	}
	wantCounts := map[string]int64{
		"usage_events":       2,
		"activity_events":    1,
		"usage_turn_context": 1,
		"aggregate_state":    1,
		"source_checkpoints": 1,
	}
	for table, want := range wantCounts {
		if got := verification.RowCounts[table]; got != want {
			t.Errorf("%s rows = %d, want %d", table, got, want)
		}
	}
	assertEventKeys(t, target, []string{"restored-aggregate", "restored-observation"})
	assertEventKeys(t, result.SafetyBackupPath, []string{"live-before"})
}

func TestApplyRestoreRequiresReplaceBeforeChangingTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "target")
	before := snapshotFile(t, target)
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	if _, err := ApplyRestore(ctx, plan, target, false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("ApplyRestore without --replace = %v", err)
	}
	if after := snapshotFile(t, target); after != before {
		t.Fatalf("replace refusal changed target: before=%+v after=%+v", before, after)
	}
}

func TestApplyRestoreTransferFailureLeavesLiveTargetUsable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "source")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "original")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := applyRestore(ctx, plan, target, true, restoreHooks{
		transfer: func(context.Context, string, string) error { return errors.New("injected disk full") },
	})
	if err == nil || !strings.Contains(err.Error(), "injected disk full") {
		t.Fatalf("transfer failure = %v", err)
	}
	if !result.TargetUsable || result.SafetyBackupPath == "" {
		t.Fatalf("failed transfer result = %+v", result)
	}
	assertEventKeys(t, target, []string{"original"})
}

func TestApplyRestorePostVerifyFailureRollsBackSafetyBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedSimpleRestoreStore(t, source, "replacement")
	input := filepath.Join(dir, "input.db")
	if _, err := Backup(ctx, source, input); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedSimpleRestoreStore(t, target, "original")
	plan, err := PrepareRestore(ctx, input, target)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Cleanup()

	result, err := applyRestore(ctx, plan, target, true, restoreHooks{
		transfer: restoreOnline,
		postVerify: func(context.Context, string) (Verification, error) {
			return Verification{State: VerificationCorrupt, Reason: "injected post-copy failure"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected post-copy failure") {
		t.Fatalf("post-copy failure = %v", err)
	}
	if !result.TargetUsable || !result.RolledBack || result.SafetyBackupPath == "" {
		t.Fatalf("rollback result = %+v", result)
	}
	assertEventKeys(t, target, []string{"original"})
}

func TestQuarantinePreservesBundleAndCreatesFreshDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.db")
	seedSimpleRestoreStore(t, target, "history")
	if err := os.WriteFile(target+"-wal", []byte("wal evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+"-shm", []byte("shm evidence"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Quarantine(ctx, target)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if !result.TargetUsable || !result.HistoricalCompletenessUnknown || result.Verification.State != VerificationOK {
		t.Fatalf("quarantine result = %+v", result)
	}
	if result.Verification.RowCounts["usage_events"] != 0 {
		t.Fatalf("fresh database has %d usage rows", result.Verification.RowCounts["usage_events"])
	}
	for path, want := range map[string]string{
		filepath.Join(result.QuarantinePath, "usage.db-wal"): "wal evidence",
		filepath.Join(result.QuarantinePath, "usage.db-shm"): "shm evidence",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("quarantined %s = %q err=%v, want %q", path, got, err, want)
		}
	}
	assertEventKeys(t, filepath.Join(result.QuarantinePath, "usage.db"), []string{"history"})
}

func TestQuarantineMoveFailureRollsBackWholeBundle(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "usage.db")
	seedSimpleRestoreStore(t, target, "history")
	if err := os.WriteFile(target+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainBefore := snapshotFile(t, target)

	renameCalls := 0
	_, err := quarantine(context.Background(), target, quarantineHooks{
		rename: func(old, new string) error {
			renameCalls++
			if renameCalls == 2 {
				return errors.New("injected move failure")
			}
			return os.Rename(old, new)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected move failure") {
		t.Fatalf("quarantine failure = %v", err)
	}
	if after := snapshotFile(t, target); after != mainBefore {
		t.Fatalf("failed quarantine changed main database: before=%+v after=%+v", mainBefore, after)
	}
	if got, err := os.ReadFile(target + "-wal"); err != nil || string(got) != "wal" {
		t.Fatalf("failed quarantine lost WAL: %q err=%v", got, err)
	}
}

func seedSimpleRestoreStore(t *testing.T, path, key string) {
	t.Helper()
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.InsertEvents(context.Background(), []model.UsageEvent{
		ev(key, model.ToolCodex, time.Unix(1_750_000_000, 0), 10),
	}); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedCompleteRestoreStore(t *testing.T, path, prefix string) {
	t.Helper()
	ctx := context.Background()
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_750_000_000, 0)
	aggregateEvent := ev(prefix+"-aggregate", model.ToolHermes, at, 20)
	snapshot := model.AggregateSnapshot{
		Tool: model.ToolHermes, Key: "session", Model: "m", SessionID: "session",
		ObservedTime: at, InputTokens: 20, TotalTokens: 20, SourcePath: "/source/session",
	}
	checkpoint := &model.SourceCheckpoint{
		Tool: model.ToolHermes, SourcePath: "/source/session", Size: 100, Offset: 100, State: `{"baseline":20}`,
	}
	if _, err := ledger.ApplySnapshot(ctx, []model.UsageEvent{aggregateEvent}, snapshot, checkpoint); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	observation := ev(prefix+"-observation", model.ToolClaudeCode, at.Add(time.Second), 30)
	batch := ObservationBatch{
		Events: []model.UsageEvent{observation},
		Activity: []model.ActivityEvent{{
			Tool: model.ToolClaudeCode, Kind: model.ActivityTool, Name: "Read", EventTime: at.Add(time.Second),
			ObservedTime: at.Add(time.Second), UsageDedupKey: observation.DedupKey, CallsInTurn: 1, DedupKey: prefix + "-activity",
		}},
		TurnContexts: []model.TurnContext{{
			UsageDedupKey: observation.DedupKey, Tool: model.ToolClaudeCode, Dimension: model.DimensionSkill,
			Value: "restore-test", EventTime: at.Add(time.Second), ObservedTime: at.Add(time.Second),
		}},
	}
	if _, err := ledger.ApplyBatch(ctx, batch); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertEventKeys(t *testing.T, path string, want []string) {
	t.Helper()
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.ListEvents(context.Background(), Filter{})
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(events))
	for i := range events {
		got[i] = events[i].DedupKey
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event keys = %v, want %v", got, want)
	}
}
