// SQLite-backed implementation of the Store interface. Pure Go via
// modernc.org/sqlite (CGO_ENABLED=0). The append-only guarantee is enforced by
// schema.sql (UNIQUE(dedup_key) + no-UPDATE/no-DELETE triggers); this file only
// ever INSERTs OR IGNOREs into usage_events and upserts mutable accumulator
// state into aggregate_state.
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

// schemaVersion is the logical version recorded in schema_meta and reported by
// doctor. Bump when the schema changes in a way that needs a migration.
const schemaVersion = 1

// SQLite is the concrete append-only store backed by modernc.org/sqlite.
type SQLite struct {
	db   *sql.DB
	path string
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if absent) the database at path, applies the schema and
// pragmas (WAL, busy_timeout=5000), and records the schema version. The handle
// is read/write because the collector appends to it; all reporting paths only
// issue SELECTs.
func Open(path string) (*SQLite, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}

	// modernc driver name is "sqlite". Pragmas applied via the DSN run on every
	// pooled connection; the schema is applied once below.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if _, err := db.Exec(
		`INSERT INTO schema_meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("%d", schemaVersion),
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: record schema version: %w", err)
	}

	return &SQLite{db: db, path: path}, nil
}

func ensureParentDir(path string) error {
	dir := dirOf(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
// ignored; rows are never updated or deleted.
func (s *SQLite) InsertEvents(ctx context.Context, events []model.UsageEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO usage_events (
			dedup_key, tool, model, session_id, project,
			event_time_unix, observed_time_unix,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			reasoning_tokens, total_tokens,
			request_id, message_id, source_path, kind, raw
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare insert: %w", err)
	}
	defer stmt.Close()

	inserted := 0
	for _, e := range events {
		if e.DedupKey == "" {
			return inserted, fmt.Errorf("store: event with empty dedup key (tool=%s)", e.Tool)
		}
		kind := e.Kind
		if kind == "" {
			kind = model.KindUsage
		}
		res, err := stmt.ExecContext(ctx,
			e.DedupKey, e.Tool, e.Model, e.SessionID, e.Project,
			e.EventTime.UTC().Unix(), observedUnix(e),
			e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens,
			e.ReasoningTokens, e.TotalTokens,
			e.RequestID, e.MessageID, e.SourcePath, string(kind), nullString(e.Raw),
		)
		if err != nil {
			return inserted, fmt.Errorf("store: insert event %s: %w", e.DedupKey, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("store: commit: %w", err)
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
	_, err := s.db.ExecContext(ctx, `
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
	st := DBStats{Path: s.path, SchemaVersion: schemaVersion}

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

	if v, err := s.readSchemaVersion(ctx); err == nil && v > 0 {
		st.SchemaVersion = v
	}

	if fi, err := os.Stat(s.path); err == nil {
		st.SizeBytes = fi.Size()
	}
	return st, nil
}

// readSchemaVersion reads the recorded schema version, or 0 if absent.
func (s *SQLite) readSchemaVersion(ctx context.Context) (int, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int
	_, _ = fmt.Sscanf(v, "%d", &n)
	return n, nil
}
