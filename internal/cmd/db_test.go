package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

func TestDBVerifyJSON(t *testing.T) {
	path := seedRecoveryDB(t)
	out, err := runCmd(t, "--db", path, "--config", offlineConfig(t), "db", "verify", "--json")
	if err != nil {
		t.Fatalf("db verify --json: %v\n%s", err, out)
	}
	var result store.Verification
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode verification: %v\n%s", err, out)
	}
	if result.State != store.VerificationOK || result.SchemaVersion != store.SchemaVersion || result.RowCounts["usage_events"] != 1 {
		t.Fatalf("verification = %+v", result)
	}
}

func TestDBVerifyNonOKPrintsResultAndReturnsError(t *testing.T) {
	path := seedRecoveryDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER trg_events_no_delete`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, "--db", path, "--config", offlineConfig(t), "db", "verify")
	if err == nil {
		t.Fatalf("corrupt verification exited zero:\n%s", out)
	}
	for _, want := range []string{"State:", "corrupt", "trg_events_no_delete"} {
		if !strings.Contains(out, want) {
			t.Fatalf("verification output missing %q:\n%s", want, out)
		}
	}
}

func TestDBBackupJSON(t *testing.T) {
	source := seedRecoveryDB(t)
	destination := filepath.Join(t.TempDir(), "saved.db")
	out, err := runCmd(t, "--db", source, "--config", offlineConfig(t), "db", "backup", "--out", destination, "--json")
	if err != nil {
		t.Fatalf("db backup --json: %v\n%s", err, out)
	}
	var result store.BackupResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode backup: %v\n%s", err, out)
	}
	if result.Path != destination || result.State != store.VerificationOK || result.RowCounts["usage_events"] != 1 {
		t.Fatalf("backup = %+v", result)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("published backup: %v", err)
	}
}

func TestDBBackupHumanOutputReportsProgress(t *testing.T) {
	source := seedRecoveryDB(t)
	destination := filepath.Join(t.TempDir(), "saved.db")
	out, err := runCmd(t, "--db", source, "--config", offlineConfig(t), "db", "backup", "--out", destination)
	if err != nil {
		t.Fatalf("db backup: %v\n%s", err, out)
	}
	for _, want := range []string{"Backup: copying...", "Backup: verifying...", "Backup: publishing...", "SHA-256:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("backup output missing %q:\n%s", want, out)
		}
	}
}

func TestDBBackupUsesVersionedDefaultPath(t *testing.T) {
	source := seedRecoveryDB(t)
	out, err := runCmd(t, "--db", source, "--config", offlineConfig(t), "db", "backup", "--json")
	if err != nil {
		t.Fatalf("db backup default: %v\n%s", err, out)
	}
	var result store.BackupResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	wantDirectory := filepath.Join(filepath.Dir(source), "backups")
	if filepath.Dir(result.Path) != wantDirectory || !strings.Contains(filepath.Base(result.Path), fmt.Sprintf("-v%d-", store.SchemaVersion)) {
		t.Fatalf("default backup path = %s", result.Path)
	}
}

func TestDBRestoreJSON(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	snapshotSource := filepath.Join(dir, "snapshot-source.db")
	seedRecoveryDBAt(t, snapshotSource, "from-snapshot")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), snapshotSource, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "live.db")
	seedRecoveryDBAt(t, target, "live-before")

	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup, "--replace", "--json")
	if err != nil {
		t.Fatalf("db restore --json: %v\n%s", err, out)
	}
	var result restoreCommandResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode restore: %v\n%s", err, out)
	}
	if !result.TargetUsable || result.Verification.State != store.VerificationOK || result.SafetyBackupPath == "" {
		t.Fatalf("restore = %+v", result)
	}
	assertRecoveryCLIKeys(t, target, []string{"from-snapshot"})
	assertRecoveryCLIKeys(t, result.SafetyBackupPath, []string{"live-before"})
	if stages, err := filepath.Glob(filepath.Join(dir, ".restore-stage-*.db")); err != nil || len(stages) != 0 {
		t.Fatalf("restore staging files = %v err=%v", stages, err)
	}
}

func TestDBRestorePreservesSummaryAndExportOutput(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "from-snapshot")
	cfg := offlineConfig(t)

	wantSummary, err := runCmd(t, "--db", source, "--config", cfg, "--no-daemon", "summary", "--json")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := runCmd(t, "--db", source, "--config", cfg, "--no-daemon", "export")
	if err != nil {
		t.Fatal(err)
	}
	wantCSV, err := runCmd(t, "--db", source, "--config", cfg, "--no-daemon", "export", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "live.db")
	seedRecoveryDBAt(t, target, "live-before")
	if out, err := runCmd(t, "--db", target, "--config", cfg, "db", "restore", backup, "--replace", "--json"); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}

	gotSummary, err := runCmd(t, "--db", target, "--config", cfg, "--no-daemon", "summary", "--json")
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := runCmd(t, "--db", target, "--config", cfg, "--no-daemon", "export")
	if err != nil {
		t.Fatal(err)
	}
	gotCSV, err := runCmd(t, "--db", target, "--config", cfg, "--no-daemon", "export", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}
	if gotSummary != wantSummary || gotJSON != wantJSON || gotCSV != wantCSV {
		t.Fatalf("restore changed outputs\nsummary equal=%t JSON equal=%t CSV equal=%t", gotSummary == wantSummary, gotJSON == wantJSON, gotCSV == wantCSV)
	}
}

func TestDBRestoreHumanOutputExplainsSnapshotConsequences(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "from-snapshot")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "new-live.db")
	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup)
	if err != nil {
		t.Fatalf("human restore: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Restored:", target,
		"Snapshot:", backup,
		"Safety backup:", "none",
		"Collector restarted:", "false",
		"events newer than this snapshot may be absent",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("restore output missing %q:\n%s", want, out)
		}
	}
}

func TestDBRestoreRestartsPreviouslyRunningCollector(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "from-snapshot")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "live.db")
	seedRecoveryDBAt(t, target, "live-before")

	originalPause := pauseCollectionForMaintenance
	originalResume := resumeCollectionAfterMaintenance
	t.Cleanup(func() {
		pauseCollectionForMaintenance = originalPause
		resumeCollectionAfterMaintenance = originalResume
	})
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		return maintenanceCollectionState{wasRunning: true, detached: true}, nil
	}
	resumeCalls := 0
	resumeCollectionAfterMaintenance = func(context.Context, config.Config, maintenanceCollectionState, io.Writer) error {
		resumeCalls++
		return nil
	}

	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup, "--replace", "--json")
	if err != nil {
		t.Fatalf("db restore: %v\n%s", err, out)
	}
	var result restoreCommandResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.CollectorWasRunning || !result.CollectorRestarted || resumeCalls != 1 {
		t.Fatalf("restore lifecycle = %+v, resume calls=%d", result, resumeCalls)
	}
}

func TestDBRestoreFailureResumesUsableCollector(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "from-snapshot")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "live.db")
	seedRecoveryDBAt(t, target, "live-before")
	if err := os.WriteFile(filepath.Join(dir, "backups"), []byte("block safety backup directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalPause := pauseCollectionForMaintenance
	originalResume := resumeCollectionAfterMaintenance
	t.Cleanup(func() {
		pauseCollectionForMaintenance = originalPause
		resumeCollectionAfterMaintenance = originalResume
	})
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		return maintenanceCollectionState{wasRunning: true}, nil
	}
	resumeCalls := 0
	resumeCollectionAfterMaintenance = func(context.Context, config.Config, maintenanceCollectionState, io.Writer) error {
		resumeCalls++
		return nil
	}

	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup, "--replace", "--json")
	if err == nil || !strings.Contains(err.Error(), "safety backup") {
		t.Fatalf("db restore safety failure = %v\n%s", err, out)
	}
	if resumeCalls != 1 {
		t.Fatalf("usable target resume calls = %d, want 1", resumeCalls)
	}
	assertRecoveryCLIKeys(t, target, []string{"live-before"})
}

func TestDBRestoreUnusableFailureRetainsStagingAndLeavesCollectorStopped(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "from-snapshot")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "live.db")
	seedRecoveryDBAt(t, target, "live-before")

	originalApply := applyPreparedRestore
	originalPause := pauseCollectionForMaintenance
	originalResume := resumeCollectionAfterMaintenance
	t.Cleanup(func() {
		applyPreparedRestore = originalApply
		pauseCollectionForMaintenance = originalPause
		resumeCollectionAfterMaintenance = originalResume
	})
	var stagePath string
	applyPreparedRestore = func(_ context.Context, plan *store.PreparedRestore, target string, replace bool) (store.RestoreResult, error) {
		stagePath = plan.StagePath
		return store.RestoreResult{TargetPath: target, TargetUsable: false}, errors.New("injected unusable restore")
	}
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		return maintenanceCollectionState{wasRunning: true}, nil
	}
	resumeCalls := 0
	resumeCollectionAfterMaintenance = func(context.Context, config.Config, maintenanceCollectionState, io.Writer) error {
		resumeCalls++
		return nil
	}

	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup, "--replace", "--json")
	if err == nil || !strings.Contains(err.Error(), "staging database retained") || !strings.Contains(err.Error(), stagePath) {
		t.Fatalf("unusable restore error = %v\n%s", err, out)
	}
	if resumeCalls != 0 {
		t.Fatalf("unusable restore resumed collector %d time(s)", resumeCalls)
	}
	if _, statErr := os.Stat(stagePath); statErr != nil {
		t.Fatalf("retained stage %s: %v", stagePath, statErr)
	}
	storePlan := &store.PreparedRestore{StagePath: stagePath}
	storePlan.Cleanup()
}

func TestDBRestorePauseFailureCleansStaging(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "from-snapshot")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "live.db")
	seedRecoveryDBAt(t, target, "live-before")

	originalPause := pauseCollectionForMaintenance
	t.Cleanup(func() { pauseCollectionForMaintenance = originalPause })
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		return maintenanceCollectionState{}, errors.New("injected pause failure")
	}
	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup, "--replace")
	if err == nil || !strings.Contains(err.Error(), "stop collection") {
		t.Fatalf("restore pause failure = %v\n%s", err, out)
	}
	stages, globErr := filepath.Glob(filepath.Join(dir, ".restore-stage-*.db"))
	if globErr != nil || len(stages) != 0 {
		t.Fatalf("pause failure stages = %v err=%v", stages, globErr)
	}
}

func TestDBRestoreRestartFailureReportsCompletedRestore(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "from-snapshot")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "live.db")
	seedRecoveryDBAt(t, target, "live-before")

	originalPause := pauseCollectionForMaintenance
	originalResume := resumeCollectionAfterMaintenance
	t.Cleanup(func() {
		pauseCollectionForMaintenance = originalPause
		resumeCollectionAfterMaintenance = originalResume
	})
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		return maintenanceCollectionState{wasRunning: true}, nil
	}
	resumeCollectionAfterMaintenance = func(context.Context, config.Config, maintenanceCollectionState, io.Writer) error {
		return errors.New("injected restart failure")
	}
	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup, "--replace", "--json")
	if err == nil || !strings.Contains(err.Error(), "injected restart failure") {
		t.Fatalf("restore restart failure = %v\n%s", err, out)
	}
	var result restoreCommandResult
	if decodeErr := json.Unmarshal([]byte(out), &result); decodeErr != nil {
		t.Fatalf("decode completed restore: %v\n%s", decodeErr, out)
	}
	if !result.TargetUsable || result.CollectorRestarted {
		t.Fatalf("completed restore result = %+v", result)
	}
}

func TestDBRestoreRequiresReplaceBeforeLifecyclePause(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedRecoveryDBAt(t, source, "source")
	backup := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), source, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	seedRecoveryDBAt(t, target, "target")

	originalPause := pauseCollectionForMaintenance
	t.Cleanup(func() { pauseCollectionForMaintenance = originalPause })
	paused := false
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		paused = true
		return maintenanceCollectionState{}, nil
	}
	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "restore", backup)
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("restore without --replace = %v\n%s", err, out)
	}
	if paused {
		t.Fatal("restore stopped collection before enforcing --replace")
	}
	assertRecoveryCLIKeys(t, target, []string{"target"})
}

func TestDBResetRequiresQuarantineBeforeLifecyclePause(t *testing.T) {
	originalPause := pauseCollectionForMaintenance
	t.Cleanup(func() { pauseCollectionForMaintenance = originalPause })
	paused := false
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		paused = true
		return maintenanceCollectionState{}, nil
	}
	out, err := runCmd(t, "--config", offlineConfig(t), "db", "reset")
	if err == nil || !strings.Contains(err.Error(), "--quarantine") {
		t.Fatalf("db reset without --quarantine = %v\n%s", err, out)
	}
	if paused {
		t.Fatal("db reset paused collection before enforcing --quarantine")
	}
}

func TestDBResetQuarantineJSON(t *testing.T) {
	isolateState(t)
	target := filepath.Join(t.TempDir(), "usage.db")
	seedRecoveryDBAt(t, target, "history")
	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "reset", "--quarantine", "--json")
	if err != nil {
		t.Fatalf("db reset --quarantine --json: %v\n%s", err, out)
	}
	var result resetCommandResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode reset: %v\n%s", err, out)
	}
	if !result.TargetUsable || !result.HistoricalCompletenessUnknown || result.Verification.State != store.VerificationOK {
		t.Fatalf("reset = %+v", result)
	}
	assertRecoveryCLIKeys(t, target, nil)
	assertRecoveryCLIKeys(t, filepath.Join(result.QuarantinePath, "usage.db"), []string{"history"})
}

func TestDBResetHumanOutputExplainsHistoryLimit(t *testing.T) {
	isolateState(t)
	target := filepath.Join(t.TempDir(), "usage.db")
	seedRecoveryDBAt(t, target, "history")
	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "reset", "--quarantine")
	if err != nil {
		t.Fatalf("human reset: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Fresh database:", target,
		"Quarantine:",
		"Collector restarted:", "false",
		"historical completeness is unknown",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("reset output missing %q:\n%s", want, out)
		}
	}
}

func TestDBResetRestartsPreviouslyRunningCollector(t *testing.T) {
	isolateState(t)
	target := filepath.Join(t.TempDir(), "usage.db")
	seedRecoveryDBAt(t, target, "history")
	originalPause := pauseCollectionForMaintenance
	originalResume := resumeCollectionAfterMaintenance
	t.Cleanup(func() {
		pauseCollectionForMaintenance = originalPause
		resumeCollectionAfterMaintenance = originalResume
	})
	pauseCollectionForMaintenance = func(context.Context, config.Config) (maintenanceCollectionState, error) {
		return maintenanceCollectionState{wasRunning: true, detached: true}, nil
	}
	resumeCalls := 0
	resumeCollectionAfterMaintenance = func(context.Context, config.Config, maintenanceCollectionState, io.Writer) error {
		resumeCalls++
		return nil
	}
	out, err := runCmd(t, "--db", target, "--config", offlineConfig(t), "db", "reset", "--quarantine", "--json")
	if err != nil {
		t.Fatalf("reset lifecycle: %v\n%s", err, out)
	}
	var result resetCommandResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.CollectorWasRunning || !result.CollectorRestarted || resumeCalls != 1 {
		t.Fatalf("reset lifecycle = %+v resume calls=%d", result, resumeCalls)
	}
}

func TestNextDefaultBackupPathIncludesVersionAndAvoidsCollision(t *testing.T) {
	database := filepath.Join(t.TempDir(), "usage.db")
	now := time.Date(2026, time.August, 31, 7, 30, 0, 0, time.FixedZone("offset", 3600))
	first, err := nextDefaultBackupPath(database, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := filepath.Join(filepath.Dir(database), "backups", "usage-v7-20260831T063000Z.db")
	if first != wantFirst {
		t.Fatalf("default path = %s, want %s", first, wantFirst)
	}
	if err := os.MkdirAll(filepath.Dir(first), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := nextDefaultBackupPath(database, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(first, ".db") + "-2.db"; second != want {
		t.Fatalf("collision path = %s, want %s", second, want)
	}
}

func TestOpenCollectionStoreRejectsDamagedLedgerWithoutMutation(t *testing.T) {
	path := seedRecoveryDB(t)
	backup := filepath.Join(filepath.Dir(path), "backups", "good.db")
	if _, err := store.Backup(context.Background(), path, backup); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER trg_events_no_update`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := openCollectionStoreLocked(config.Config{DBPath: path})
	if st != nil {
		st.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "trg_events_no_update") ||
		!strings.Contains(err.Error(), "newest verified backup: "+backup) ||
		!strings.Contains(err.Error(), "db restore") {
		t.Fatalf("collection open error = %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("refused collection changed damaged ledger: read=%v", readErr)
	}
}

func seedRecoveryDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	seedRecoveryDBAt(t, path, "recovery-cli")
	return path
}

func seedRecoveryDBAt(t *testing.T, path, key string) {
	t.Helper()
	ledger, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.InsertEvents(context.Background(), []model.UsageEvent{{
		Tool:        model.ToolCodex,
		Model:       "gpt-5",
		EventTime:   time.Unix(1_750_000_000, 0),
		InputTokens: 10,
		TotalTokens: 10,
		DedupKey:    key,
		Kind:        model.KindUsage,
	}}); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryCLIKeys(t *testing.T, path string, want []string) {
	t.Helper()
	reader, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.ListEvents(context.Background(), store.Filter{})
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
