package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// TestOpenFreshCreatesAtLatestVersion verifies a fresh database is stamped
// directly at SchemaVersion without running any migration step.
func TestOpenFreshCreatesAtLatestVersion(t *testing.T) {
	st := openTemp(t)
	v, err := readSchemaVersion(context.Background(), st.db)
	if err != nil || v != SchemaVersion {
		t.Fatalf("fresh version=%d err=%v want %d,nil", v, err, SchemaVersion)
	}
}

// TestOpenSameVersionReopen verifies reopening a current-version database
// succeeds and leaves the stamp untouched.
func TestOpenSameVersionReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	v, err := readSchemaVersion(context.Background(), st2.db)
	if err != nil || v != SchemaVersion {
		t.Fatalf("reopen version=%d err=%v want %d,nil", v, err, SchemaVersion)
	}
}

// TestOpenNewerVersionRefused verifies a database stamped by a newer binary
// refuses to open with an error naming both versions, and that the failed open
// does not rewind the stamp (the original downgrade-stamping bug).
func TestOpenNewerVersionRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	newer := SchemaVersion + 1
	if _, err := st.db.Exec(
		`UPDATE schema_meta SET value=? WHERE key='schema_version'`, strconv.Itoa(newer),
	); err != nil {
		t.Fatalf("stamp newer: %v", err)
	}
	st.Close()

	if _, err := Open(path); err == nil {
		t.Fatalf("expected refusal opening v%d database", newer)
	} else {
		for _, want := range []string{fmt.Sprintf("v%d", newer), fmt.Sprintf("v%d", SchemaVersion)} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not name %s", err, want)
			}
		}
	}

	raw := rawDB(t, path)
	v, err := readSchemaVersion(context.Background(), raw)
	if err != nil || v != newer {
		t.Fatalf("post-refusal version=%d err=%v want %d,nil (stamp must not rewind)", v, err, newer)
	}
}

// TestMigrationsRunOrderedAndStamped drives the runner over a v0 database with
// two synthetic steps, proving they run in ascending order and the final stamp
// is the target version.
func TestMigrationsRunOrderedAndStamped(t *testing.T) {
	ctx := context.Background()
	db := bareMetaDB(t)

	steps := []migration{
		{version: 1, statements: []string{
			`CREATE TABLE mig_log (step INTEGER NOT NULL)`,
			`INSERT INTO mig_log(step) VALUES (1)`,
		}},
		{version: 2, statements: []string{
			`INSERT INTO mig_log(step) VALUES (2)`,
		}},
	}
	if err := applyMigrations(ctx, db, 0, 2, steps); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rows, err := db.Query(`SELECT step FROM mig_log ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	defer rows.Close()
	var order []int
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		order = append(order, s)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("step order=%v want [1 2]", order)
	}

	v, err := readSchemaVersion(ctx, db)
	if err != nil || v != 2 {
		t.Fatalf("version=%d err=%v want 2,nil", v, err)
	}
}

// TestMigrationFailureKeepsPriorStamp proves the stamp is transactional with
// its step: a failing v2 rolls back and leaves the database at v1.
func TestMigrationFailureKeepsPriorStamp(t *testing.T) {
	ctx := context.Background()
	db := bareMetaDB(t)

	steps := []migration{
		{version: 1, statements: []string{`CREATE TABLE mig_log (step INTEGER NOT NULL)`}},
		{version: 2, statements: []string{`INSERT INTO no_such_table VALUES (1)`}},
	}
	if err := applyMigrations(ctx, db, 0, 2, steps); err == nil {
		t.Fatalf("expected v2 step failure")
	}

	v, err := readSchemaVersion(ctx, db)
	if err != nil || v != 1 {
		t.Fatalf("version=%d err=%v want 1,nil (v1 committed, v2 rolled back)", v, err)
	}
}

// TestMigrationGapDetected verifies the runner refuses a step list that skips
// a version.
func TestMigrationGapDetected(t *testing.T) {
	db := bareMetaDB(t)
	steps := []migration{{version: 2}}
	if err := applyMigrations(context.Background(), db, 0, 2, steps); err == nil {
		t.Fatalf("expected gap error for missing v1 step")
	}
}

// TestMigrateV1ToV2CreatesCheckpoints simulates opening a v1 database with
// this (v2) binary: the migration must create source_checkpoints and stamp v2.
// The v1 state is produced by removing the v2 table from a fresh database and
// rewinding the stamp — bytewise equivalent to what a v1 binary left behind.
func TestMigrateV1ToV2CreatesCheckpoints(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.db.Exec(`DROP TABLE source_checkpoints`); err != nil {
		t.Fatalf("drop v2 table: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE schema_meta SET value='1' WHERE key='schema_version'`); err != nil {
		t.Fatalf("rewind stamp: %v", err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v1 db: %v", err)
	}
	defer st2.Close()

	v, err := readSchemaVersion(ctx, st2.db)
	if err != nil || v != 2 {
		t.Fatalf("post-migration version=%d err=%v want 2,nil", v, err)
	}
	ok, err := tableExists(ctx, st2.db, "source_checkpoints")
	if err != nil || !ok {
		t.Fatalf("source_checkpoints exists=%v err=%v want true,nil", ok, err)
	}
	// The migrated table must be usable, not just present.
	cp := model.SourceCheckpoint{Tool: model.ToolCodex, SourcePath: "/p", Size: 1}
	if _, err := st2.ApplyEvents(ctx, nil, &cp); err != nil {
		t.Fatalf("checkpoint write on migrated db: %v", err)
	}
}

// TestMigrationSkipsWhenAlreadyApplied covers the concurrent-upgrader race:
// the version is re-checked inside the migration transaction, so a process
// that finds the step already stamped (another process won the write lock and
// committed first) skips it instead of re-running the DDL.
func TestMigrationSkipsWhenAlreadyApplied(t *testing.T) {
	ctx := context.Background()
	db := bareMetaDB(t)
	if _, err := db.Exec(`INSERT INTO schema_meta(key,value) VALUES('schema_version','2')`); err != nil {
		t.Fatalf("stamp v2: %v", err)
	}

	// A non-idempotent statement: re-running it would fail loudly.
	step := migration{version: 2, statements: []string{`CREATE TABLE mig_once (x INTEGER)`}}
	if _, err := db.Exec(`CREATE TABLE mig_once (x INTEGER)`); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if err := applyMigration(ctx, db, step); err != nil {
		t.Fatalf("already-applied step must be skipped, got: %v", err)
	}

	v, err := readSchemaVersion(ctx, db)
	if err != nil || v != 2 {
		t.Fatalf("version=%d err=%v want 2,nil (untouched)", v, err)
	}
}

// bareMetaDB opens a database containing only an unstamped schema_meta table,
// i.e. a synthetic v0 database for driving the runner directly.
func bareMetaDB(t *testing.T) *sql.DB {
	t.Helper()
	db := rawDB(t, filepath.Join(t.TempDir(), "bare.db"))
	if _, err := db.Exec(`CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create schema_meta: %v", err)
	}
	return db
}

// rawDB opens a plain database handle without going through Open.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
