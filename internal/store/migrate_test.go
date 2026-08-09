package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
