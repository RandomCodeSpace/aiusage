package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestMigrateFullChainFromV1 drives the whole ladder: a database written by the
// v1 binary must reach SchemaVersion through every intermediate step, gaining
// the v2 checkpoint table and the v3 cost columns, without disturbing the row
// it already held. The v1 database is built from a literal legacy DDL rather
// than by mutating a fresh one — a fresh database now carries columns v1 never
// had, so "drop the new table and rewind the stamp" no longer reproduces it.
func TestMigrateFullChainFromV1(t *testing.T) {
	ctx := context.Background()
	path := legacyDB(t, 1)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v1 db: %v", err)
	}
	defer st.Close()

	v, err := readSchemaVersion(ctx, st.db)
	if err != nil || v != SchemaVersion {
		t.Fatalf("post-migration version=%d err=%v want %d,nil", v, err, SchemaVersion)
	}
	ok, err := tableExists(ctx, st.db, "source_checkpoints")
	if err != nil || !ok {
		t.Fatalf("source_checkpoints exists=%v err=%v want true,nil", ok, err)
	}
	// The migrated table must be usable, not just present.
	cp := model.SourceCheckpoint{Tool: model.ToolCodex, SourcePath: "/p", Size: 1}
	if _, err := st.ApplyEvents(ctx, nil, &cp); err != nil {
		t.Fatalf("checkpoint write on migrated db: %v", err)
	}
	assertV3Columns(t, st)
}

// TestMigrateV2ToV3AddsCostColumns covers the single step most users will run:
// a v2 database (the shipped checkpoints release) gains the four v3 columns,
// its existing rows read back unpriced rather than free, and new rows can be
// stored with a cost.
func TestMigrateV2ToV3AddsCostColumns(t *testing.T) {
	ctx := context.Background()
	path := legacyDB(t, 2)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen v2 db: %v", err)
	}
	defer st.Close()

	v, err := readSchemaVersion(ctx, st.db)
	if err != nil || v != 3 {
		t.Fatalf("post-migration version=%d err=%v want 3,nil", v, err)
	}
	assertV3Columns(t, st)
}

// assertV3Columns checks the post-migration behaviour of the cost columns: the
// pre-existing legacy row is unpriced (NULL, not zero) with empty provider and
// tier, and a freshly stamped event round-trips its cost.
func assertV3Columns(t *testing.T, st *SQLite) {
	t.Helper()
	ctx := context.Background()

	evs, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("legacy rows = %d, want 1", len(evs))
	}
	if _, priced := evs[0].Cost(); priced {
		t.Errorf("legacy row is priced; migrated rows must read back unpriced")
	}
	if evs[0].Provider != "" || evs[0].ServiceTier != "" || evs[0].PriceSource != "" {
		t.Errorf("legacy row = %+v, want empty provider/tier/price_source", evs[0])
	}

	fresh := model.UsageEvent{
		Tool:      model.ToolClaudeCode,
		Model:     "claude-sonnet-4-6",
		Provider:  model.ProviderAnthropic,
		DedupKey:  "post-migration-1",
		EventTime: time.Unix(1_760_000_000, 0).UTC(),
	}
	fresh.SetCost(1234, "embedded-2026-08-09")
	if _, err := st.InsertEvents(ctx, []model.UsageEvent{fresh}); err != nil {
		t.Fatalf("insert on migrated db: %v", err)
	}
	evs, err = st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("list events after insert: %v", err)
	}
	var got *model.UsageEvent
	for i := range evs {
		if evs[i].DedupKey == "post-migration-1" {
			got = &evs[i]
		}
	}
	if got == nil {
		t.Fatalf("stamped event missing after insert")
	}
	if c, ok := got.Cost(); !ok || c != 1234 {
		t.Errorf("stamped cost = %d,%v want 1234,true", c, ok)
	}
	if got.PriceSource != "embedded-2026-08-09" || got.Provider != model.ProviderAnthropic {
		t.Errorf("stamped row = %+v, want anthropic/embedded-2026-08-09", *got)
	}
}

// legacyDBSchema is the usage_events/aggregate_state DDL exactly as the v1
// binary wrote it: no source_checkpoints (v2) and none of the cost/provider
// columns (v3). Migration tests build from this rather than from schema.sql,
// which always describes the LATEST schema.
const legacyDBSchema = `
CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE usage_events (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  dedup_key             TEXT    NOT NULL UNIQUE,
  tool                  TEXT    NOT NULL,
  model                 TEXT    NOT NULL DEFAULT '',
  session_id            TEXT    NOT NULL DEFAULT '',
  project               TEXT    NOT NULL DEFAULT '',
  event_time_unix       INTEGER NOT NULL,
  observed_time_unix    INTEGER NOT NULL,
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens          INTEGER NOT NULL DEFAULT 0,
  request_id            TEXT    NOT NULL DEFAULT '',
  message_id            TEXT    NOT NULL DEFAULT '',
  source_path           TEXT    NOT NULL DEFAULT '',
  kind                  TEXT    NOT NULL DEFAULT 'usage',
  raw                   TEXT
);
CREATE TRIGGER trg_events_no_update
BEFORE UPDATE ON usage_events
BEGIN SELECT RAISE(ABORT, 'usage_events is append-only: UPDATE forbidden'); END;
CREATE TRIGGER trg_events_no_delete
BEFORE DELETE ON usage_events
BEGIN SELECT RAISE(ABORT, 'usage_events is append-only: DELETE forbidden'); END;
CREATE TABLE aggregate_state (
  tool                  TEXT    NOT NULL,
  acc_key               TEXT    NOT NULL,
  model                 TEXT    NOT NULL DEFAULT '',
  session_id            TEXT    NOT NULL DEFAULT '',
  project               TEXT    NOT NULL DEFAULT '',
  observed_time_unix    INTEGER NOT NULL,
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens          INTEGER NOT NULL DEFAULT 0,
  source_path           TEXT    NOT NULL DEFAULT '',
  raw                   TEXT,
  PRIMARY KEY (tool, acc_key)
);`

// legacyCheckpointsDDL is the source_checkpoints table as the v2 binary created
// it, added on top of legacyDBSchema to build a v2 database.
const legacyCheckpointsDDL = `
CREATE TABLE source_checkpoints (
  tool        TEXT    NOT NULL,
  source_path TEXT    NOT NULL,
  size_bytes  INTEGER NOT NULL DEFAULT 0,
  mtime_ns    INTEGER NOT NULL DEFAULT 0,
  read_offset INTEGER NOT NULL DEFAULT 0,
  watermark   INTEGER NOT NULL DEFAULT 0,
  state       TEXT,
  PRIMARY KEY (tool, source_path)
);`

// legacyDB writes a database at the given historical schema version, holding
// one usage row, and returns its path. It never goes through Open, so the file
// is exactly what the older binary would have left behind.
func legacyDB(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	db := rawDB(t, path)

	ddl := legacyDBSchema
	if version >= 2 {
		ddl += legacyCheckpointsDDL
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create v%d schema: %v", version, err)
	}
	if _, err := db.Exec(`
		INSERT INTO usage_events (dedup_key, tool, model, event_time_unix, observed_time_unix,
			input_tokens, output_tokens, total_tokens)
		VALUES ('legacy-1', ?, 'claude-sonnet-4-6', 1750000000, 1750000000, 10, 20, 30)`,
		model.ToolClaudeCode); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_meta(key,value) VALUES('schema_version',?)`,
		strconv.Itoa(version)); err != nil {
		t.Fatalf("stamp v%d: %v", version, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
	return path
}

// TestOpenUnstampedLegacyTableRefused covers the half-created database: a
// usage_events written by an older binary that never reached its version stamp.
// schema.sql cannot add the v3 columns to a table that already exists, so
// stamping SchemaVersion over it would advertise a layout the file does not
// have and every insert would fail on the placeholder count. Open must refuse,
// name the missing columns, and leave the database unstamped.
func TestOpenUnstampedLegacyTableRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	db := rawDB(t, path)
	if _, err := db.Exec(legacyDBSchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatalf("expected refusal opening an unstamped pre-v3 database")
	}
	for _, want := range []string{"provider", "service_tier", "cost_micro_usd", "price_source", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}

	raw := rawDB(t, path)
	v, err := readSchemaVersion(ctx, raw)
	if err != nil || v != 0 {
		t.Fatalf("post-refusal version=%d err=%v want 0,nil (a short table must not be stamped)", v, err)
	}
}

// TestOpenFreshStampsCompleteColumnSet is the other half of the guard: a
// genuinely fresh database passes the column check and is stamped, so the
// verification cannot degrade into "never create anything".
func TestOpenFreshStampsCompleteColumnSet(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	have, err := columnSet(ctx, st.db, "usage_events")
	if err != nil {
		t.Fatalf("column set: %v", err)
	}
	for _, c := range eventColumns {
		if !have[c] {
			t.Errorf("fresh usage_events is missing %s", c)
		}
	}
	if len(have) != len(eventColumns) {
		t.Errorf("usage_events has %d columns, eventColumns lists %d — keep them in step", len(have), len(eventColumns))
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
