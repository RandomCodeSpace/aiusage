// SQLite-backed implementation of the Store interface. Pure Go via
// modernc.org/sqlite (CGO_ENABLED=0). The append-only guarantee is enforced by
// schema.sql (UNIQUE(dedup_key) + no-UPDATE/no-DELETE triggers); this file only
// ever appends to usage_events (INSERT .. ON CONFLICT(dedup_key) DO NOTHING)
// and upserts mutable accumulator state into aggregate_state.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// SchemaVersion is the schema version this binary creates fresh databases at
// and can open. Bump when the schema changes, keep schema.sql describing the
// full latest schema, and add the matching step to migrations (migrate.go).
const SchemaVersion = 1

// SQLite is the concrete append-only store backed by modernc.org/sqlite.
type SQLite struct {
	db   *sql.DB
	path string
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if absent) the database at path with WAL and
// busy_timeout=5000 pragmas, then reads the recorded schema version before
// touching anything: same version opens as-is, older versions run the ordered
// migrations (migrate.go), and a newer version is refused so an older binary
// can never stamp it backwards. The handle is read/write because the collector
// appends to it; all reporting paths only issue SELECTs.
func Open(path string) (*SQLite, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}

	// modernc driver name is "sqlite". Pragmas applied via the DSN run on every
	// pooled connection; the schema is managed once by ensureSchema below.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	if err := ensureSchema(context.Background(), db); err != nil {
		db.Close()
		return nil, err
	}

	// The raw column holds transcript content, so the DB must be owner-only.
	// SQLite creates WAL/SHM sidecars with the main DB file's mode, so tightening
	// the DB here also covers sidecars created later; existing sidecars are fixed
	// directly. Best-effort: doctor surfaces perms that could not be repaired.
	restrictPerms(path)

	return &SQLite{db: db, path: path}, nil
}

// restrictPerms chmods the DB and its WAL/SHM sidecars to 0600. Missing
// sidecars and chmod failures are ignored (see Open).
func restrictPerms(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}
}

func ensureParentDir(path string) error {
	dir := dirOf(path)
	if dir == "" || dir == "." {
		return nil
	}
	// 0700: the DB inside holds transcript content (raw column).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create dir %s: %w", dir, err)
	}
	return nil
}

// dirOf returns the directory component of a path without importing path-style
// assumptions beyond the OS separator handled by the caller's filepath usage.
func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// Close releases the database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// InsertEvents appends events idempotently in a single transaction. Returns the
// count of rows actually inserted (new dedup keys). Existing dedup keys are
// ignored; rows are never updated or deleted. A row that fails its own insert
// (CHECK violation, empty dedup key) is skipped and reported in the returned
// error while the rest of the batch still commits — one poison row must not
// abort a batch that is re-read and retried every cycle.
func (s *SQLite) InsertEvents(ctx context.Context, events []model.UsageEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	inserted, skipErr, err := insertEventsTx(ctx, tx, events)
	if err != nil {
		return inserted, err
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("store: commit: %w", err)
	}
	return inserted, skipErr
}

// insertEventsTx runs the idempotent event insert inside an existing
// transaction, so ApplySnapshot (and future collection-scoped writes such as
// cycle checkpoints) can combine it with other statements atomically.
// ON CONFLICT(dedup_key) DO NOTHING keeps the dedup ignore silent while a
// CHECK violation still errors (blanket OR IGNORE would swallow it). Per-row
// failures are skipped (a failed statement does not abort the SQLite
// transaction) and summarised in skipErr; err is reserved for failures of the
// batch itself, after which the transaction must not be committed.
func insertEventsTx(ctx context.Context, tx *sql.Tx, events []model.UsageEvent) (inserted int, skipErr, err error) {
	if len(events) == 0 {
		return 0, nil, nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_events (
			dedup_key, tool, model, session_id, project,
			event_time_unix, observed_time_unix,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			reasoning_tokens, total_tokens,
			request_id, message_id, source_path, kind, raw
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(dedup_key) DO NOTHING`)
	if err != nil {
		return 0, nil, fmt.Errorf("store: prepare insert: %w", err)
	}
	defer stmt.Close()

	var (
		skipped   int
		firstSkip error
	)
	skip := func(rowErr error) {
		skipped++
		if firstSkip == nil {
			firstSkip = rowErr
		}
	}
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return inserted, nil, err
		}
		if e.DedupKey == "" {
			skip(fmt.Errorf("store: event with empty dedup key (tool=%s)", e.Tool))
			continue
		}
		kind := e.Kind
		if kind == "" {
			kind = model.KindUsage
		}
		res, execErr := stmt.ExecContext(ctx,
			e.DedupKey, e.Tool, e.Model, e.SessionID, e.Project,
			e.EventTime.UTC().Unix(), observedUnix(e),
			e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens,
			e.ReasoningTokens, e.TotalTokens,
			e.RequestID, e.MessageID, e.SourcePath, string(kind), nullString(e.Raw),
		)
		if execErr != nil {
			skip(fmt.Errorf("store: insert event %s: %w", e.DedupKey, execErr))
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if skipped > 0 {
		return inserted, fmt.Errorf("store: skipped %d of %d event(s); first: %w", skipped, len(events), firstSkip), nil
	}
	return inserted, nil, nil
}

// applySnapshotFault, when non-nil, runs between the event insert and the
// state upsert inside ApplySnapshot's transaction. Test-only seam simulating a
// crash in the window that used to double count (events committed, state not).
var applySnapshotFault func() error

// ApplySnapshot atomically appends the delta events and records the new
// accumulator state in one transaction — see Store.ApplySnapshot for the
// contract, including the skipped state write on a fully-collided insert.
func (s *SQLite) ApplySnapshot(ctx context.Context, events []model.UsageEvent, state model.AggregateSnapshot) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	inserted, skipErr, err := insertEventsTx(ctx, tx, events)
	if err != nil {
		return 0, err
	}
	if skipErr != nil {
		// A skipped delta event must not advance the baseline: rolling back
		// keeps the delta re-derivable next cycle.
		return 0, skipErr
	}
	if applySnapshotFault != nil {
		if err := applySnapshotFault(); err != nil {
			return 0, err
		}
	}
	if len(events) == 0 || inserted > 0 {
		if err := upsertState(ctx, tx, state); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return inserted, nil
}

// observedUnix returns the observed timestamp in UTC seconds, falling back to
// the event time when ObservedTime is unset.
func observedUnix(e model.UsageEvent) int64 {
	if e.ObservedTime.IsZero() {
		return e.EventTime.UTC().Unix()
	}
	return e.ObservedTime.UTC().Unix()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LastState returns the latest observed counters for the (tool, key) accumulator
// cell, or nil if none has been recorded.
func (s *SQLite) LastState(ctx context.Context, tool, key string) (*model.AggregateSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tool, acc_key, model, session_id, project, observed_time_unix,
		       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		       reasoning_tokens, total_tokens, source_path, COALESCE(raw,'')
		FROM aggregate_state WHERE tool=? AND acc_key=?`, tool, key)

	var (
		out      model.AggregateSnapshot
		observed int64
	)
	err := row.Scan(
		&out.Tool, &out.Key, &out.Model, &out.SessionID, &out.Project, &observed,
		&out.InputTokens, &out.OutputTokens, &out.CacheCreationTokens, &out.CacheReadTokens,
		&out.ReasoningTokens, &out.TotalTokens, &out.SourcePath, &out.Raw,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: last state %s/%s: %w", tool, key, err)
	}
	out.ObservedTime = time.Unix(observed, 0).UTC()
	return &out, nil
}

// UpsertState records the latest observed counters for (tool, key), replacing
// any previous value. This is mutable accumulator state, not history.
func (s *SQLite) UpsertState(ctx context.Context, st model.AggregateSnapshot) error {
	return upsertState(ctx, s.db, st)
}

// execer is the ExecContext subset shared by *sql.DB and *sql.Tx, so the state
// upsert can run standalone (autocommit) or inside ApplySnapshot's transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func upsertState(ctx context.Context, db execer, st model.AggregateSnapshot) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO aggregate_state (
			tool, acc_key, model, session_id, project, observed_time_unix,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			reasoning_tokens, total_tokens, source_path, raw
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tool, acc_key) DO UPDATE SET
			model=excluded.model, session_id=excluded.session_id, project=excluded.project,
			observed_time_unix=excluded.observed_time_unix,
			input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			cache_creation_tokens=excluded.cache_creation_tokens,
			cache_read_tokens=excluded.cache_read_tokens,
			reasoning_tokens=excluded.reasoning_tokens, total_tokens=excluded.total_tokens,
			source_path=excluded.source_path, raw=excluded.raw`,
		st.Tool, st.Key, st.Model, st.SessionID, st.Project, st.ObservedTime.UTC().Unix(),
		st.InputTokens, st.OutputTokens, st.CacheCreationTokens, st.CacheReadTokens,
		st.ReasoningTokens, st.TotalTokens, st.SourcePath, nullString(st.Raw),
	)
	if err != nil {
		return fmt.Errorf("store: upsert state %s/%s: %w", st.Tool, st.Key, err)
	}
	return nil
}

// Summarize aggregates usage matching Filter, grouped per Filter.GroupBy. Time
// dimensions (hour/day/week/month) are bucketed in the local timezone so "today"
// matches the wall clock; categorical dimensions group by their stored value.
func (s *SQLite) Summarize(ctx context.Context, f Filter) (*Summary, error) {
	where, args := buildWhere(f)

	groupExprs := make([]string, 0, len(f.GroupBy))
	for _, dim := range f.GroupBy {
		expr, err := groupExpr(dim)
		if err != nil {
			return nil, err
		}
		groupExprs = append(groupExprs, expr)
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	for _, ge := range groupExprs {
		sb.WriteString(ge)
		sb.WriteString(", ")
	}
	sb.WriteString(`COUNT(*) AS events,
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(total_tokens),0)
		FROM usage_events`)
	sb.WriteString(where)
	if len(groupExprs) > 0 {
		sb.WriteString(" GROUP BY ")
		sb.WriteString(strings.Join(groupExprs, ", "))
		sb.WriteString(" ORDER BY ")
		sb.WriteString(strings.Join(groupExprs, ", "))
	}

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: summarize: %w", err)
	}
	defer rows.Close()

	sum := &Summary{GroupBy: append([]string{}, f.GroupBy...)}
	for rows.Next() {
		keyVals := make([]string, len(f.GroupBy))
		dest := make([]any, 0, len(f.GroupBy)+7)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var b Bucket
		dest = append(dest, &b.Events, &b.Input, &b.Output, &b.CacheCreation, &b.CacheRead, &b.Reasoning, &b.Total)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan summary row: %w", err)
		}
		if len(f.GroupBy) > 0 {
			b.Keys = make(map[string]string, len(f.GroupBy))
			b.OrderedKeys = append([]string{}, f.GroupBy...)
			for i, dim := range f.GroupBy {
				b.Keys[dim] = keyVals[i]
			}
		}
		sum.Buckets = append(sum.Buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: summary rows: %w", err)
	}

	// Grand total is a separate aggregate so it reflects the full filtered set
	// regardless of grouping.
	tot, err := s.grandTotal(ctx, where, args)
	if err != nil {
		return nil, err
	}
	sum.Totals = tot
	return sum, nil
}

// grandTotal computes the ungrouped totals over the filtered set.
func (s *SQLite) grandTotal(ctx context.Context, where string, args []any) (Bucket, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(total_tokens),0)
		FROM usage_events`+where, args...)
	var b Bucket
	if err := row.Scan(&b.Events, &b.Input, &b.Output, &b.CacheCreation, &b.CacheRead, &b.Reasoning, &b.Total); err != nil {
		return Bucket{}, fmt.Errorf("store: grand total: %w", err)
	}
	return b, nil
}

// ListEvents returns raw events matching Filter, ordered by event_time.
func (s *SQLite) ListEvents(ctx context.Context, f Filter) ([]model.UsageEvent, error) {
	where, args := buildWhere(f)
	q := `SELECT tool, model, session_id, project, event_time_unix, observed_time_unix,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		reasoning_tokens, total_tokens, request_id, message_id, source_path, kind,
		dedup_key, COALESCE(raw,'')
		FROM usage_events` + where + ` ORDER BY event_time_unix, id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()

	var out []model.UsageEvent
	for rows.Next() {
		var (
			e            model.UsageEvent
			eventUnix    int64
			observedUnix int64
			kind         string
		)
		if err := rows.Scan(
			&e.Tool, &e.Model, &e.SessionID, &e.Project, &eventUnix, &observedUnix,
			&e.InputTokens, &e.OutputTokens, &e.CacheCreationTokens, &e.CacheReadTokens,
			&e.ReasoningTokens, &e.TotalTokens, &e.RequestID, &e.MessageID, &e.SourcePath, &kind,
			&e.DedupKey, &e.Raw,
		); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		e.EventTime = time.Unix(eventUnix, 0).UTC()
		e.ObservedTime = time.Unix(observedUnix, 0).UTC()
		e.Kind = model.EventKind(kind)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: event rows: %w", err)
	}
	return out, nil
}

// SourceStats returns per-tool stored stats for the `sources` command.
func (s *SQLite) SourceStats(ctx context.Context) ([]SourceStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tool,
			COUNT(*) AS events,
			COUNT(DISTINCT CASE WHEN session_id <> '' THEN session_id END) AS sessions,
			COALESCE(SUM(total_tokens),0) AS total,
			MIN(event_time_unix), MAX(event_time_unix), MAX(observed_time_unix)
		FROM usage_events
		GROUP BY tool
		ORDER BY total DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: source stats: %w", err)
	}
	defer rows.Close()

	var out []SourceStat
	for rows.Next() {
		var (
			st                   SourceStat
			first, last, lastObs sql.NullInt64
		)
		if err := rows.Scan(&st.Tool, &st.Events, &st.Sessions, &st.Total, &first, &last, &lastObs); err != nil {
			return nil, fmt.Errorf("store: scan source stat: %w", err)
		}
		if first.Valid {
			st.FirstEvent = time.Unix(first.Int64, 0).UTC()
		}
		if last.Valid {
			st.LastEvent = time.Unix(last.Int64, 0).UTC()
		}
		if lastObs.Valid {
			st.LastObserved = time.Unix(lastObs.Int64, 0).UTC()
		}
		models, err := s.distinctModels(ctx, st.Tool)
		if err != nil {
			return nil, err
		}
		st.Models = models
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: source stat rows: %w", err)
	}
	return out, nil
}

// distinctModels returns the sorted distinct non-empty model ids for a tool.
func (s *SQLite) distinctModels(ctx context.Context, tool string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT model FROM usage_events
		WHERE tool=? AND model <> '' ORDER BY model`, tool)
	if err != nil {
		return nil, fmt.Errorf("store: distinct models %s: %w", tool, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("store: scan model: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Stats returns whole-database diagnostics for the `doctor` command.
func (s *SQLite) Stats(ctx context.Context) (DBStats, error) {
	st := DBStats{Path: s.path, SchemaVersion: SchemaVersion}

	var (
		earliest, latest sql.NullInt64
	)
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COUNT(DISTINCT tool),
			COUNT(DISTINCT CASE WHEN model <> '' THEN model END),
			MIN(event_time_unix), MAX(event_time_unix)
		FROM usage_events`)
	if err := row.Scan(&st.Events, &st.DistinctTools, &st.DistinctModel, &earliest, &latest); err != nil {
		return DBStats{}, fmt.Errorf("store: stats: %w", err)
	}
	if earliest.Valid {
		st.EarliestEvent = time.Unix(earliest.Int64, 0).UTC()
	}
	if latest.Valid {
		st.LatestEvent = time.Unix(latest.Int64, 0).UTC()
	}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM aggregate_state`).Scan(&st.Snapshots); err != nil {
		return DBStats{}, fmt.Errorf("store: snapshot count: %w", err)
	}

	if v, err := readSchemaVersion(ctx, s.db); err == nil && v > 0 {
		st.SchemaVersion = v
	}

	if fi, err := os.Stat(s.path); err == nil {
		st.SizeBytes = fi.Size()
	}
	return st, nil
}
