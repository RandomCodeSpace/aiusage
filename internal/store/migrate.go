// Schema versioning and the migration runner. Open never writes before
// reading the recorded version: fresh databases are created directly at
// SchemaVersion, older ones run the ordered additive migrations, and a newer
// database refuses to open so an older binary can never stamp a version
// backwards.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// migration is one ordered schema step bringing a database from version-1 to
// version. Statements must be additive (ALTER TABLE ... ADD COLUMN, CREATE ...
// IF NOT EXISTS): trg_events_no_update blocks UPDATE backfill on usage_events.
type migration struct {
	version    int
	statements []string
}

// migrations lists every step needed to bring an older database up to
// SchemaVersion, in ascending version order with no gaps. Fresh databases
// never run these; they are created directly at SchemaVersion from schema.sql,
// which must always describe the latest schema in full.
//
// Version ledger:
//
//	v2 — source_checkpoints table (incremental collection, issue #5)
//	v3 — reserved: cost/provider columns on usage_events (issue #9)
var migrations = []migration{
	{version: 2, statements: []string{
		`CREATE TABLE IF NOT EXISTS source_checkpoints (
		  tool        TEXT    NOT NULL,
		  source_path TEXT    NOT NULL,
		  size_bytes  INTEGER NOT NULL DEFAULT 0,
		  mtime_ns    INTEGER NOT NULL DEFAULT 0,
		  read_offset INTEGER NOT NULL DEFAULT 0,
		  watermark   INTEGER NOT NULL DEFAULT 0,
		  state       TEXT,
		  PRIMARY KEY (tool, source_path)
		)`,
	}},
}

// ensureSchema reads the recorded schema version before touching anything and
// brings the database to SchemaVersion.
func ensureSchema(ctx context.Context, db *sql.DB) error {
	hasMeta, err := tableExists(ctx, db, "schema_meta")
	if err != nil {
		return err
	}
	current := 0
	if hasMeta {
		current, err = readSchemaVersion(ctx, db)
		if err != nil {
			return fmt.Errorf("store: read schema version: %w", err)
		}
	}
	switch {
	case current == 0:
		// Fresh database, or a create interrupted before the version stamp:
		// schema.sql is IF NOT EXISTS throughout, so reapplying is safe.
		if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("store: apply schema: %w", err)
		}
		// Upsert is safe here: this branch only runs when no version is
		// recorded, so it can never stamp an existing version backwards.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_meta(key, value) VALUES('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			strconv.Itoa(SchemaVersion),
		); err != nil {
			return fmt.Errorf("store: record schema version: %w", err)
		}
		return nil
	case current == SchemaVersion:
		return nil
	case current < SchemaVersion:
		return applyMigrations(ctx, db, current, SchemaVersion, migrations)
	default:
		return fmt.Errorf("store: database schema is v%d, newer than this binary's v%d; upgrade aiusage to open it", current, SchemaVersion)
	}
}

// applyMigrations runs every step in (from, to] in ascending order, refusing
// gaps so a database can never skip a version.
func applyMigrations(ctx context.Context, db *sql.DB, from, to int, steps []migration) error {
	next := from + 1
	for _, m := range steps {
		if m.version <= from {
			continue
		}
		if m.version > to {
			break
		}
		if m.version != next {
			return fmt.Errorf("store: migration to v%d missing (next step is v%d)", next, m.version)
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
		next++
	}
	if next != to+1 {
		return fmt.Errorf("store: no migration path from v%d to v%d", from, to)
	}
	return nil
}

// applyMigration runs one step in its own transaction with the version stamp
// as the last statement, so an interrupted migration never advances the
// recorded version past what actually committed.
//
// The version is re-checked INSIDE the transaction, after a write statement
// has escalated it to a write lock: two processes upgrading concurrently
// would otherwise both read the old version outside any lock and both run the
// same step (harmless for CREATE IF NOT EXISTS, corrupting for a future
// ALTER ... ADD COLUMN). The loser of the lock race re-reads the advanced
// version and skips.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration v%d: %w", m.version, err)
	}
	defer tx.Rollback()

	// No-op UPDATE: its only purpose is to take the write lock BEFORE the
	// version re-read, so the read cannot see a stale pre-upgrade snapshot.
	if _, err := tx.ExecContext(ctx,
		`UPDATE schema_meta SET value=value WHERE key='schema_version'`); err != nil {
		return fmt.Errorf("store: lock for migration v%d: %w", m.version, err)
	}
	cur, err := readSchemaVersionQ(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: re-read schema version for v%d: %w", m.version, err)
	}
	if cur >= m.version {
		return nil // another process already ran this step; deferred Rollback releases the lock
	}
	if cur != m.version-1 {
		return fmt.Errorf("store: migration v%d expected database at v%d, found v%d", m.version, m.version-1, cur)
	}

	for _, stmt := range m.statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: migration v%d: %w", m.version, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.Itoa(m.version),
	); err != nil {
		return fmt.Errorf("store: stamp schema version v%d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration v%d: %w", m.version, err)
	}
	return nil
}

// tableExists reports whether a table is present, so Open can read the version
// without creating anything first.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: check table %s: %w", name, err)
	}
	return n > 0, nil
}

// rowQuerier is the QueryRowContext subset shared by *sql.DB and *sql.Tx, so
// the version read can also run inside a migration's transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readSchemaVersion returns the recorded schema version, or 0 when the row is
// absent. The schema_meta table must already exist.
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	return readSchemaVersionQ(ctx, db)
}

func readSchemaVersionQ(ctx context.Context, q rowQuerier) (int, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("unrecognized schema_version %q", v)
	}
	return n, nil
}
