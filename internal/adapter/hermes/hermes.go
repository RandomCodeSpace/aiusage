// Package hermes implements an AGGREGATE adapter for the Hermes CLI.
//
// Hermes records per-session token counters in a SQLite database at
// <home>/state.db. A single session's columns GROW as the session runs across
// many polls, so this adapter is aggregate: it emits one AggregateSnapshot per
// session row (the current cumulative totals) keyed by the session id. The
// collector compares each snapshot against the last stored state and appends a
// positive delta as an immutable event, so totals never undercount and survive
// a later deletion of the source row.
//
// CRITICAL: strictly read-only. The SQLite database is opened with mode=ro so a
// poll can never create, lock for writing, or modify the agent's state.
package hermes

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the pure-Go "sqlite" driver

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

const (
	// homeEnv may hold a comma-separated list of Hermes home directories.
	homeEnv = "HERMES_HOME"
	// dbName is the SQLite state database within a Hermes home.
	dbName = "state.db"
	// driverName is the modernc.org/sqlite database/sql driver name.
	driverName = "sqlite"
	// metaProject labels every Hermes session (no cwd is recorded by Hermes).
	metaProject = "hermes"
)

// sessionsQuery selects every session that has a model attributed. The token
// columns are cumulative running totals that grow as the session continues.
const sessionsQuery = `SELECT id, model, billing_provider, started_at, ended_at,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens
	FROM sessions
	WHERE model IS NOT NULL AND TRIM(model) != ''`

// Adapter reads the Hermes state database. Read-only.
type Adapter struct{}

// New returns a Hermes adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolHermes }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "Hermes" }

// homes returns the configured Hermes home directories. HERMES_HOME may be a
// comma-separated list; otherwise the discovery root (override or ~/.hermes).
func (a Adapter) homes(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv(homeEnv)); env != "" {
		var out []string
		for _, p := range strings.Split(env, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	def := ""
	if cfg.Home != "" {
		def = filepath.Join(cfg.Home, ".hermes")
	}
	return []string{cfg.Root(model.ToolHermes, def)}
}

// Discover locates each <home>/state.db that exists as a regular file.
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source
	for _, home := range a.homes(cfg) {
		if home == "" {
			continue
		}
		db := filepath.Join(home, dbName)
		if !isFile(db) {
			continue
		}
		if _, dup := seen[db]; dup {
			continue
		}
		seen[db] = struct{}{}
		srcs = append(srcs, adapter.Source{
			Tool:  model.ToolHermes,
			Class: model.Aggregate,
			Path:  db,
			Label: "Hermes sessions: " + db,
			Meta:  map[string]string{"home": home},
		})
	}
	return srcs, nil
}

// Collect opens the state database read-only and emits one AggregateSnapshot
// per session row. A malformed/unreadable row is skipped rather than failing
// the whole cycle; a non-fatal error is returned describing skipped rows.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	db, err := openReadOnly(src.Path)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("hermes: open %s: %w", src.Path, err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, sessionsQuery)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("hermes: query %s: %w", src.Path, err)
	}
	defer rows.Close()

	now := time.Now().UTC()
	var snaps []model.AggregateSnapshot
	var skipped int
	for rows.Next() {
		var (
			id, mdl                       string
			provider, startedAt, endedAt  sql.NullString
			input, output                 sql.NullInt64
			cacheRead, cacheWrite, reason sql.NullInt64
		)
		if err := rows.Scan(&id, &mdl, &provider, &startedAt, &endedAt,
			&input, &output, &cacheRead, &cacheWrite, &reason); err != nil {
			skipped++
			continue
		}
		id = strings.TrimSpace(id)
		mdl = strings.TrimSpace(mdl)
		if id == "" || mdl == "" {
			skipped++
			continue
		}

		in := nonNeg(input.Int64)
		out := nonNeg(output.Int64)
		cCreate := nonNeg(cacheWrite.Int64) // cache_write_tokens -> CacheCreation
		cRead := nonNeg(cacheRead.Int64)
		reasoning := nonNeg(reason.Int64)
		// Anthropic-style additive accounting; reasoning is informational and
		// (per the spec) not added into the authoritative total.
		total := in + out + cCreate + cRead

		snaps = append(snaps, model.AggregateSnapshot{
			Tool:                model.ToolHermes,
			Key:                 id,
			Model:               mdl,
			SessionID:           id,
			Project:             metaProject,
			ObservedTime:        now,
			InputTokens:         in,
			OutputTokens:        out,
			CacheCreationTokens: cCreate,
			CacheReadTokens:     cRead,
			ReasoningTokens:     reasoning,
			TotalTokens:         total,
			SourcePath:          src.Path,
			Raw:                 rawJSON(provider.String, startedAt.String),
		})
	}
	if err := rows.Err(); err != nil {
		return adapter.Observation{Snapshots: snaps}, fmt.Errorf("hermes: iterate %s: %w", src.Path, err)
	}
	if skipped > 0 {
		return adapter.Observation{Snapshots: snaps}, fmt.Errorf("hermes: skipped %d malformed session row(s) in %s", skipped, src.Path)
	}
	return adapter.Observation{Snapshots: snaps}, nil
}

// openReadOnly opens a SQLite database strictly read-only. The mode=ro DSN
// prevents any write/create/lock; busy_timeout keeps a transient lock from
// failing the poll.
func openReadOnly(path string) (*sql.DB, error) {
	// immutable=1 (matching the opencode adapter) is required: opening a WAL-mode
	// DB with mode=ro alone makes SQLite create -wal/-shm sidecar files in the
	// agent's directory, which would breach the strictly-read-only guarantee.
	// immutable=1 promises the file will not change under us and suppresses that.
	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// rawJSON builds the audit blob carrying provider + start time. Built by hand
// (no encoding/json) since both values are short, controlled strings.
func rawJSON(provider, startedAt string) string {
	var b strings.Builder
	b.WriteString(`{"billing_provider":`)
	b.WriteString(quote(provider))
	b.WriteString(`,"started_at":`)
	b.WriteString(quote(startedAt))
	b.WriteByte('}')
	return b.String()
}

// quote returns a minimally-escaped JSON string literal.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// nonNeg clamps a possibly-negative counter to zero.
func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// isFile reports whether path exists and is a regular file.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
