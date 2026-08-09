// Package opencode implements the event-level adapter for the opencode CLI.
//
// opencode stores per-message usage both in a SQLite database and as JSON
// files. Under each data directory we read BOTH:
//
//   - SQLite "opencode.db" (or the first "opencode-<token>.db") — table
//     `message(id, session_id, data)` where `data` is the message JSON.
//   - JSON files under "storage/message/**/*.json" (the same shape).
//
// Both carry the same per-message `data` payload:
//
//	{id, sessionID, providerID, modelID, time:{created:<ms>},
//	 tokens:{input, output, reasoning, cache:{read, write}, total},
//	 cost, path:{cwd, root}}
//
// Token mapping (opencode reports cache read/write as separate buckets, like
// Anthropic): Input=tokens.input, Output=tokens.output,
// CacheCreation=tokens.cache.write, CacheRead=tokens.cache.read,
// Reasoning=tokens.reasoning, and Total is reconciled against tokens.total via
// tokenutil.ApplyTotalFallback.
//
// Reasoning is ADDITIVE here, not a subset of output: every local message row
// satisfies total = input + output + reasoning + cache.read + cache.write, and
// rows with reasoning > output exist. It therefore participates in the known
// sum handed to the fallback (see buildEvent).
//
// The persisted dedup key is "opencode|<message id>", so the SQLite row and the
// JSON file for the same message collapse to one stored event (DB is discovered
// first, so it wins on INSERT OR IGNORE).
//
// CRITICAL: strictly read-only. JSON files are opened O_RDONLY; the database is
// opened with a read-only DSN (mode=ro plus query_only(1)) — never immutable=1,
// because opencode writes this database live and keeps a large WAL an immutable
// reader cannot see (see collectDB). Nothing under the agent's directories is
// created, locked, or modified.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0)

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
	"github.com/RandomCodeSpace/aiusage/internal/tokenutil"
)

const (
	dataDirEnv     = "OPENCODE_DATA_DIR"
	primaryDBName  = "opencode.db"
	dbPrefix       = "opencode-"
	dbSuffix       = ".db"
	messageDirName = "message"
	storageDirName = "storage"

	// Source kinds carried in Source.Meta["kind"].
	kindDB   = "db"
	kindJSON = "json"
)

// Adapter reads opencode CLI message usage. Read-only.
type Adapter struct{}

// New returns an opencode adapter.
func New() adapter.Adapter { return Adapter{} }

// ID returns the stable tool identifier.
func (Adapter) ID() string { return model.ToolOpenCode }

// DisplayName returns the human-friendly name.
func (Adapter) DisplayName() string { return "opencode" }

// dataDirs returns the configured opencode data directories. OPENCODE_DATA_DIR
// may be a comma-separated list that fully REPLACES the default; otherwise the
// discovery root (override or ~/.local/share/opencode) is used.
func (a Adapter) dataDirs(cfg adapter.DiscoverConfig) []string {
	if env := strings.TrimSpace(os.Getenv(dataDirEnv)); env != "" {
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
		def = filepath.Join(cfg.Home, ".local", "share", "opencode")
	}
	return []string{cfg.Root(model.ToolOpenCode, def)}
}

// Discover locates, per data dir, the SQLite database (if any) and the JSON
// message tree (if any). The database is discovered FIRST so that, on a dedup
// collision with the JSON copy, the DB row wins (INSERT OR IGNORE).
func (a Adapter) Discover(ctx context.Context, cfg adapter.DiscoverConfig) ([]adapter.Source, error) {
	seen := make(map[string]struct{})
	var srcs []adapter.Source

	for _, dir := range a.dataDirs(cfg) {
		if ctx.Err() != nil {
			return srcs, ctx.Err()
		}
		if dir == "" || !adapter.IsDir(dir) {
			continue
		}

		if dbPath := findDB(dir); dbPath != "" {
			if _, dup := seen[dbPath]; !dup {
				seen[dbPath] = struct{}{}
				srcs = append(srcs, adapter.Source{
					Tool:  model.ToolOpenCode,
					Class: model.EventLevel,
					Path:  dbPath,
					Label: "opencode db " + filepath.Base(dbPath),
					Meta:  map[string]string{"kind": kindDB},
				})
			}
		}

		msgDir := filepath.Join(dir, storageDirName, messageDirName)
		if adapter.IsDir(msgDir) {
			if _, dup := seen[msgDir]; !dup {
				seen[msgDir] = struct{}{}
				srcs = append(srcs, adapter.Source{
					Tool:  model.ToolOpenCode,
					Class: model.EventLevel,
					Path:  msgDir,
					Label: "opencode messages " + dir,
					Meta:  map[string]string{"kind": kindJSON},
				})
			}
		}
	}
	return srcs, nil
}

// findDB returns the primary opencode.db if present, else the first
// opencode-<token>.db (lexically ordered for determinism), else "".
func findDB(dir string) string {
	primary := filepath.Join(dir, primaryDBName)
	if isFile(primary) {
		return primary
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries { // ReadDir returns entries sorted by name
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, dbPrefix) && strings.HasSuffix(name, dbSuffix) {
			return filepath.Join(dir, name)
		}
	}
	return ""
}

// Collect reads one discovered source (DB or JSON tree) read-only.
func (a Adapter) Collect(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	return a.CollectIncremental(ctx, src, nil)
}

// CollectIncremental reads the DB source with a rowid watermark (only message
// rows above the last consumed rowid are scanned; the message log is
// append-only). The JSON tree has no incremental path and is always read in
// full. A nil cp is a full read.
func (a Adapter) CollectIncremental(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	switch kindOf(src) {
	case kindDB:
		return collectDB(ctx, src, cp)
	case kindJSON:
		return collectJSON(ctx, src)
	default:
		return adapter.Observation{}, fmt.Errorf("opencode: unknown source kind for %s", src.Path)
	}
}

func kindOf(src adapter.Source) string {
	if src.Meta != nil {
		if k := src.Meta["kind"]; k != "" {
			return k
		}
	}
	return ""
}

// collectDB reads `SELECT rowid, id, session_id, data FROM message` read-only,
// restricted to rowids above the checkpoint watermark (the message log is
// append-only, so already-consumed rowids never change). The `id`/`session_id`
// columns are authoritative; the `data` JSON supplies tokens, model, timestamp
// and project. A malformed or missing column never fails the whole source —
// the row is skipped, but it also holds the watermark back so it is retried.
// The DSN is mode=ro (no create/write/lock) plus query_only(1) (the connection
// refuses any write statement) plus busy_timeout, matching the hermes adapter.
// immutable=1 is deliberately NOT used: opencode holds this database open and
// writes it live (three processes on the reference machine), journal_mode is
// wal, and the WAL held 74.65 MiB of un-checkpointed pages the immutable reader
// could not see — main-file mtime a full day behind the WAL. immutable=1 makes
// SQLite skip locking, skip change detection and ignore the WAL entirely, so
// the adapter reads the database as of its last checkpoint and is exposed to
// SQLite's documented wrong-result and torn-read behaviour whenever opencode
// writes mid-scan. None of that fails at open; it surfaces as silently stale
// rows or SQLITE_CORRUPT at query time.
func collectDB(ctx context.Context, src adapter.Source, cp *model.SourceCheckpoint) (adapter.Observation, error) {
	dsn := "file:" + src.Path + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: open db %s: %w", src.Path, err)
	}
	defer db.Close()

	watermark := int64(0)
	if cp != nil {
		watermark = cp.Watermark
	}

	rows, err := db.QueryContext(ctx,
		"SELECT rowid, id, session_id, data FROM message WHERE rowid > ? ORDER BY rowid", watermark)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: query %s: %w", src.Path, err)
	}
	defer rows.Close()

	var (
		events   []model.UsageEvent
		consumed = watermark // highest rowid fully processed, no gaps skipped
		clean    = true
	)
	for rows.Next() {
		if ctx.Err() != nil {
			return adapter.Observation{Events: events}, ctx.Err()
		}
		var (
			rowid     int64
			id        sql.NullString
			sessionID sql.NullString
			data      sql.NullString
		)
		if err := rows.Scan(&rowid, &id, &sessionID, &data); err != nil {
			clean = false // unreadable row: retry it next cycle
			continue
		}
		if data.Valid && data.String != "" {
			if ev, ok := buildEvent([]byte(data.String), id.String, sessionID.String, src.Path); ok {
				events = append(events, ev)
			}
		}
		if clean {
			consumed = rowid
		}
	}
	if rows.Err() != nil {
		clean = false // iteration broke: do not advance past what we saw cleanly
	}

	obs := adapter.Observation{Events: events}
	if consumed > watermark || (cp == nil && clean) {
		obs.Checkpoint = &model.SourceCheckpoint{
			Tool: model.ToolOpenCode, SourcePath: src.Path, Watermark: consumed,
		}
	}
	// rows.Err() is intentionally non-fatal: keep best-effort results.
	return obs, nil
}

// collectJSON walks storage/message/**/*.json read-only, parsing each as a
// message `data` payload. The DB columns are unavailable here, so id/session
// come from the JSON itself.
func collectJSON(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	var events []model.UsageEvent
	_ = filepath.WalkDir(src.Path, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		raw, rerr := os.ReadFile(path) // read-only
		if rerr != nil {
			return nil // skip unreadable file
		}
		ev, ok := buildEvent(raw, "", "", path)
		if !ok {
			return nil
		}
		events = append(events, ev)
		return nil
	})
	return adapter.Observation{Events: events}, nil
}

// message is the per-message `data` JSON payload (DB column or JSON file).
type message struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Time       struct {
		Created int64 `json:"created"` // unix milliseconds
	} `json:"time"`
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
		Total int64 `json:"total"`
	} `json:"tokens"`
	Path struct {
		Cwd  string `json:"cwd"`
		Root string `json:"root"`
	} `json:"path"`
}

// buildEvent parses a message payload and maps it onto a UsageEvent.
//
// dbID/dbSession override the JSON id/sessionID when non-empty (DB columns are
// authoritative). Returns ok=false when the payload is unparseable, the model
// id is empty, or every token component is zero.
func buildEvent(raw []byte, dbID, dbSession, srcPath string) (model.UsageEvent, bool) {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return model.UsageEvent{}, false
	}

	mdl := strings.TrimSpace(m.ModelID)
	if mdl == "" {
		return model.UsageEvent{}, false // require non-empty modelID
	}

	id := strings.TrimSpace(m.ID)
	if dbID != "" {
		id = strings.TrimSpace(dbID)
	}
	if id == "" {
		return model.UsageEvent{}, false // need a stable dedup key
	}

	session := strings.TrimSpace(m.SessionID)
	if dbSession != "" {
		session = strings.TrimSpace(dbSession)
	}
	if session == "" {
		session = "unknown"
	}

	input := adapter.NonNeg(m.Tokens.Input)
	output := adapter.NonNeg(m.Tokens.Output)
	cacheCreation := adapter.NonNeg(m.Tokens.Cache.Write)
	cacheRead := adapter.NonNeg(m.Tokens.Cache.Read)
	reasoning := adapter.NonNeg(m.Tokens.Reasoning)
	total := adapter.NonNeg(m.Tokens.Total)

	// Reconcile against the provider total. opencode cache buckets are additive
	// (Anthropic-style) and so is reasoning, so all of them participate in the
	// known sum and only a genuinely unexplained remainder is redistributed.
	// Passing reasoning as the extra bucket is also what stops the gap-fill
	// branch from billing it twice: were reasoning left out of the known sum,
	// a row with output == 0 and reasoning > 0 would have the fallback copy the
	// reasoning count into OutputTokens while ReasoningTokens kept it as well.
	output, extra := tokenutil.ApplyTotalFallback(input, output, cacheCreation, cacheRead, reasoning, total)

	// Authoritative stored total: prefer the provider total, else the sum of the
	// additive components (extra already carries reasoning plus any overflow).
	storedTotal := total
	if sum := input + output + cacheCreation + cacheRead + extra; storedTotal < sum {
		storedTotal = sum
	}

	if input == 0 && output == 0 && cacheCreation == 0 && cacheRead == 0 &&
		reasoning == 0 && extra == 0 && storedTotal == 0 {
		return model.UsageEvent{}, false // drop all-zero records
	}

	project := strings.TrimSpace(m.Path.Cwd)
	if project == "" {
		project = "opencode"
	}

	when := time.Time{}
	if m.Time.Created > 0 {
		when = time.UnixMilli(m.Time.Created).UTC()
	}

	ev := model.UsageEvent{
		Tool:                model.ToolOpenCode,
		Model:               mdl,
		Provider:            strings.TrimSpace(m.ProviderID),
		SessionID:           session,
		Project:             project,
		EventTime:           when,
		InputTokens:         input,
		OutputTokens:        output,
		CacheCreationTokens: cacheCreation,
		CacheReadTokens:     cacheRead,
		ReasoningTokens:     reasoning,
		TotalTokens:         storedTotal,
		MessageID:           id,
		SourcePath:          srcPath,
		DedupKey:            "opencode|" + id,
		Kind:                model.KindUsage,
	}
	return ev, true
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
