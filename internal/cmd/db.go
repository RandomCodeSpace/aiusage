package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
	"github.com/RandomCodeSpace/aiusage/store"
)

func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Back up, verify, restore, or quarantine the usage database",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newDBBackupCmd(), newDBVerifyCmd(), newDBRestoreCmd(), newDBResetCmd())
	return cmd
}

type maintenanceCollectionState struct {
	wasRunning    bool
	nativeStopped bool
	detached      bool
}

var pauseCollectionForMaintenance = func(ctx context.Context, cfg config.Config) (maintenanceCollectionState, error) {
	running, pid := daemon.Status(cfg)
	if !running {
		return maintenanceCollectionState{}, nil
	}
	state := maintenanceCollectionState{wasRunning: true}
	if autoInstall(flags) {
		supervisor := newSupervisor()
		supervisorCtx, cancel := supervisionContext(ctx)
		defer cancel()
		if supervisor.Available(supervisorCtx) {
			stopped, err := supervisor.StopCollection(supervisorCtx)
			if err != nil {
				return state, err
			}
			if stopped {
				state.nativeStopped = true
				return state, nil
			}
			state.detached = true
		}
	}
	if err := stopDaemon(cfg, pid); err != nil {
		return state, err
	}
	return state, nil
}

var resumeCollectionAfterMaintenance = func(ctx context.Context, cfg config.Config, state maintenanceCollectionState, warn io.Writer) error {
	if !state.wasRunning {
		return nil
	}
	if state.nativeStopped {
		supervisor := newSupervisor()
		supervisorCtx, cancel := supervisionContext(ctx)
		defer cancel()
		return supervisor.StartCollection(supervisorCtx)
	}
	if state.detached {
		return spawnDaemon(cfg)
	}
	return ensureDaemon(ctx, cfg, warn)
}

func newDBRestoreCmd() *cobra.Command {
	var replace bool
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "restore <backup>",
		Short: "Restore a complete verified database snapshot",
		Long: "restore validates and, when needed, migrates a staging copy before it " +
			"stops collection. An existing live database requires --replace and receives a safety backup first.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runDBRestore(c, args[0], replace, asJSON)
		},
	}
	cmd.Flags().BoolVar(&replace, "replace", false, "replace the configured database after creating a verified safety backup")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the restore result as JSON")
	return cmd
}

type restoreCommandResult struct {
	store.RestoreResult
	CollectorWasRunning bool `json:"collector_was_running"`
	CollectorRestarted  bool `json:"collector_restarted"`
}

var applyPreparedRestore = store.ApplyRestore

func runDBRestore(c *cobra.Command, backup string, replace, asJSON bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	plan, err := store.PrepareRestore(cmdContext(c), backup, cfg.DBPath)
	if err != nil {
		return err
	}
	cleanupPlan := true
	defer func() {
		if cleanupPlan {
			plan.Cleanup()
		}
	}()
	if _, err := os.Lstat(cfg.DBPath); err == nil && !replace {
		return fmt.Errorf("restore target exists; pass --replace to replace %s", cfg.DBPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect restore target %s: %w", cfg.DBPath, err)
	}

	prior, err := pauseCollectionForMaintenance(cmdContext(c), cfg)
	if err != nil {
		return fmt.Errorf("stop collection before restore: %w", err)
	}
	release, err := daemon.AcquireCollectionLock(cfg.PIDPath, buildinfo.Identity())
	if err != nil {
		resumeErr := resumeCollectionAfterMaintenance(cmdContext(c), cfg, prior, c.ErrOrStderr())
		return errors.Join(fmt.Errorf("acquire collection lock for restore: %w", err), resumeErr)
	}
	result, applyErr := applyPreparedRestore(cmdContext(c), plan, cfg.DBPath, replace)
	release()

	commandResult := restoreCommandResult{RestoreResult: result, CollectorWasRunning: prior.wasRunning}
	if applyErr != nil {
		if result.TargetUsable {
			resumeErr := resumeCollectionAfterMaintenance(cmdContext(c), cfg, prior, c.ErrOrStderr())
			return errors.Join(applyErr, resumeErr)
		}
		cleanupPlan = false
		return fmt.Errorf("%w; collection remains stopped because the target is not verified usable; staging database retained at %s", applyErr, plan.StagePath)
	}
	resumeErr := resumeCollectionAfterMaintenance(cmdContext(c), cfg, prior, c.ErrOrStderr())
	commandResult.CollectorRestarted = prior.wasRunning && resumeErr == nil
	if asJSON {
		if err := writeIndentedJSON(c.OutOrStdout(), commandResult); err != nil {
			return err
		}
	} else if err := writeRestore(c.OutOrStdout(), commandResult); err != nil {
		return err
	}
	return resumeErr
}

func newDBResetCmd() *cobra.Command {
	var quarantine bool
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Quarantine the current database and start fresh",
		Long: "reset is the explicit last resort when no verified backup can be restored. " +
			"It requires --quarantine and preserves the database, WAL, and SHM together before creating a fresh database.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDBReset(c, quarantine, asJSON)
		},
	}
	cmd.Flags().BoolVar(&quarantine, "quarantine", false, "required: preserve the complete SQLite bundle before resetting")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the quarantine result as JSON")
	return cmd
}

type resetCommandResult struct {
	store.QuarantineResult
	CollectorWasRunning bool `json:"collector_was_running"`
	CollectorRestarted  bool `json:"collector_restarted"`
}

func runDBReset(c *cobra.Command, quarantine, asJSON bool) error {
	if !quarantine {
		return fmt.Errorf("db reset requires --quarantine; the current database will be preserved, never silently deleted")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return fmt.Errorf("inspect database before quarantine: %w", err)
	}
	prior, err := pauseCollectionForMaintenance(cmdContext(c), cfg)
	if err != nil {
		return fmt.Errorf("stop collection before quarantine: %w", err)
	}
	release, err := daemon.AcquireCollectionLock(cfg.PIDPath, buildinfo.Identity())
	if err != nil {
		resumeErr := resumeCollectionAfterMaintenance(cmdContext(c), cfg, prior, c.ErrOrStderr())
		return errors.Join(fmt.Errorf("acquire collection lock for quarantine: %w", err), resumeErr)
	}
	result, quarantineErr := store.Quarantine(cmdContext(c), cfg.DBPath)
	release()
	if quarantineErr != nil {
		if result.TargetUsable {
			resumeErr := resumeCollectionAfterMaintenance(cmdContext(c), cfg, prior, c.ErrOrStderr())
			return errors.Join(quarantineErr, resumeErr)
		}
		return fmt.Errorf("%w; collection remains stopped because no fresh verified target exists", quarantineErr)
	}
	resumeErr := resumeCollectionAfterMaintenance(cmdContext(c), cfg, prior, c.ErrOrStderr())
	commandResult := resetCommandResult{
		QuarantineResult:    result,
		CollectorWasRunning: prior.wasRunning,
		CollectorRestarted:  prior.wasRunning && resumeErr == nil,
	}
	if asJSON {
		if err := writeIndentedJSON(c.OutOrStdout(), commandResult); err != nil {
			return err
		}
	} else if err := writeReset(c.OutOrStdout(), commandResult); err != nil {
		return err
	}
	return resumeErr
}

func newDBVerifyCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "verify [path]",
		Short: "Check a database without changing it",
		Long: "verify runs SQLite integrity, foreign-key, aiusage schema, append-only, " +
			"and rollup checks. It never creates, migrates, repairs, or changes the database.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runDBVerify(c, args, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the verification result as JSON")
	return cmd
}

func runDBVerify(c *cobra.Command, args []string, asJSON bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	path := cfg.DBPath
	if len(args) == 1 {
		path = args[0]
	}
	result, err := store.Verify(cmdContext(c), path)
	if err != nil {
		return err
	}
	if asJSON {
		if err := writeIndentedJSON(c.OutOrStdout(), result); err != nil {
			return err
		}
	} else if err := writeVerification(c.OutOrStdout(), result); err != nil {
		return err
	}
	return result.StateError()
}

func newDBBackupCmd() *cobra.Command {
	var out string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Create a verified backup while collection keeps running",
		Long: "backup uses SQLite's online backup mechanism to capture the complete " +
			"usage database. It never overwrites an existing file.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDBBackup(c, out, asJSON)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "backup path (default: a unique file under the database's backups directory)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the backup result as JSON")
	return cmd
}

func runDBBackup(c *cobra.Command, out string, asJSON bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	destination := strings.TrimSpace(out)
	if destination == "" {
		version, err := store.RecordedSchemaVersion(cmdContext(c), cfg.DBPath)
		if err != nil {
			return err
		}
		destination, err = nextDefaultBackupPath(cfg.DBPath, version, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	var report func(store.BackupProgress)
	if !asJSON {
		lastPhase := ""
		report = func(progress store.BackupProgress) {
			if progress.Phase == lastPhase {
				return
			}
			lastPhase = progress.Phase
			fmt.Fprintf(c.ErrOrStderr(), "Backup: %s...\n", progress.Phase)
		}
	}
	result, err := store.BackupWithProgress(cmdContext(c), cfg.DBPath, destination, report)
	if err != nil {
		return err
	}
	if asJSON {
		return writeIndentedJSON(c.OutOrStdout(), result)
	}
	return writeBackup(c.OutOrStdout(), result)
}

func nextDefaultBackupPath(database string, version int, now time.Time) (string, error) {
	absolute, err := filepath.Abs(database)
	if err != nil {
		return "", fmt.Errorf("resolve database path %s: %w", database, err)
	}
	base := strings.TrimSuffix(filepath.Base(absolute), filepath.Ext(absolute))
	versionLabel := "unknown"
	if version > 0 {
		versionLabel = fmt.Sprintf("%d", version)
	}
	stem := fmt.Sprintf("%s-v%s-%s", base, versionLabel, now.UTC().Format("20060102T150405Z"))
	directory := filepath.Join(filepath.Dir(absolute), "backups")
	for sequence := 1; ; sequence++ {
		name := stem + ".db"
		if sequence > 1 {
			name = fmt.Sprintf("%s-%d.db", stem, sequence)
		}
		candidate := filepath.Join(directory, name)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect backup path %s: %w", candidate, err)
		}
	}
}

func writeVerification(out io.Writer, result store.Verification) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Database:\t%s\n", result.Path)
	fmt.Fprintf(w, "State:\t%s\n", result.State)
	fmt.Fprintf(w, "Reason:\t%s\n", result.Reason)
	fmt.Fprintf(w, "Schema:\tv%d (binary v%d, compatible: %t)\n", result.SchemaVersion, store.SchemaVersion, result.Compatible)
	fmt.Fprintf(w, "Integrity:\t%s\n", checkLabel(result.IntegrityOK))
	fmt.Fprintf(w, "Foreign keys:\t%s\n", checkLabel(result.ForeignKeysOK))
	fmt.Fprintf(w, "Application schema:\t%s\n", checkLabel(result.ApplicationSchemaOK))
	rollup := "not checked"
	if result.RollupChecked {
		rollup = checkLabel(!result.RollupStale)
	}
	fmt.Fprintf(w, "Rollup:\t%s\n", rollup)
	writeRowCounts(w, result.RowCounts)
	return w.Flush()
}

func writeBackup(out io.Writer, result store.BackupResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Backup:\t%s\n", result.Path)
	fmt.Fprintf(w, "State:\t%s\n", result.State)
	fmt.Fprintf(w, "Size:\t%d bytes\n", result.SizeBytes)
	fmt.Fprintf(w, "SHA-256:\t%s\n", result.SHA256)
	fmt.Fprintf(w, "Schema:\tv%d (compatible: %t)\n", result.SchemaVersion, result.Compatible)
	writeRowCounts(w, result.RowCounts)
	return w.Flush()
}

func writeRestore(out io.Writer, result restoreCommandResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Restored:\t%s\n", result.TargetPath)
	fmt.Fprintf(w, "Snapshot:\t%s\n", result.SourcePath)
	fmt.Fprintf(w, "Snapshot time:\t%s\n", result.SnapshotTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "Safety backup:\t%s\n", valueOrNone(result.SafetyBackupPath))
	fmt.Fprintf(w, "State:\t%s\n", result.Verification.State)
	fmt.Fprintf(w, "Collector restarted:\t%t\n", result.CollectorRestarted)
	writeRowCounts(w, result.Verification.RowCounts)
	fmt.Fprintln(w, "Warning:\tevents newer than this snapshot may be absent; only source records that still exist can be collected again")
	return w.Flush()
}

func writeReset(out io.Writer, result resetCommandResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Fresh database:\t%s\n", result.TargetPath)
	fmt.Fprintf(w, "Quarantine:\t%s\n", result.QuarantinePath)
	fmt.Fprintf(w, "State:\t%s\n", result.Verification.State)
	fmt.Fprintf(w, "Collector restarted:\t%t\n", result.CollectorRestarted)
	fmt.Fprintln(w, "Warning:\thistorical completeness is unknown; only source records that still exist may return through normal collection")
	return w.Flush()
}

func valueOrNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func writeRowCounts(w io.Writer, counts map[string]int64) {
	if len(counts) == 0 {
		return
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		label := ""
		if i == 0 {
			label = "Rows:"
		}
		fmt.Fprintf(w, "%s\t%s: %d\n", label, name, counts[name])
	}
}

func checkLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func writeIndentedJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
