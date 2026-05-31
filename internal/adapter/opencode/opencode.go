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
// The persisted dedup key is "opencode|<message id>", so the SQLite row and the
// JSON file for the same message collapse to one stored event (DB is discovered
// first, so it wins on INSERT OR IGNORE).
//
// CRITICAL: strictly read-only. JSON files are opened O_RDONLY; the database is
// opened with a read-only, immutable DSN (mode=ro&immutable=1). Nothing under
// the agent's directories is created, locked, or modified.
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

	"aiusage/internal/adapter"
	"aiusage/internal/model"
	"aiusage/internal/tokenutil"
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
		if dir == "" || !isDir(dir) {
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
		if isDir(msgDir) {
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
	switch kindOf(src) {
	case kindDB:
		return collectDB(ctx, src)
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

// collectDB reads `SELECT id, session_id, data FROM message` read-only. The
// `id`/`session_id` columns are authoritative; the `data` JSON supplies tokens,
// model, timestamp and project. A malformed or missing column never fails the
// whole source — the row is skipped.
func collectDB(ctx context.Context, src adapter.Source) (adapter.Observation, error) {
	dsn := "file:" + src.Path + "?mode=ro&immutable=1&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: open db %s: %w", src.Path, err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT id, session_id, data FROM message")
	if err != nil {
		return adapter.Observation{}, fmt.Errorf("opencode: query %s: %w", src.Path, err)
	}
	defer rows.Close()

	var events []model.UsageEvent
	for rows.Next() {
		if ctx.Err() != nil {
			return adapter.Observation{Events: events}, ctx.Err()
		}
		var (
			id        sql.NullString
			sessionID sql.NullString
			data      sql.NullString
		)
		if err := rows.Scan(&id, &sessionID, &data); err != nil {
			continue // skip unreadable row
		}
		if !data.Valid || data.String == "" {
			continue
		}
		ev, ok := buildEvent([]byte(data.String), id.String, sessionID.String, src.Path)
		if !ok {
			continue
		}
		events = append(events, ev)
	}
	// rows.Err() is intentionally non-fatal: keep best-effort results.
	return adapter.Observation{Events: events}, nil
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

	input := nonNeg(m.Tokens.Input)
	output := nonNeg(m.Tokens.Output)
	cacheCreation := nonNeg(m.Tokens.Cache.Write)
	cacheRead := nonNeg(m.Tokens.Cache.Read)
	reasoning := nonNeg(m.Tokens.Reasoning)
	total := nonNeg(m.Tokens.Total)

	// Reconcile against the provider total. opencode cache buckets are additive
	// (Anthropic-style), so they participate in the known sum; reasoning is a
	// subset of output and is NOT double-counted here. Any unexplained remainder
	// lands in output (when empty) or the extra bucket.
	output, extra := tokenutil.ApplyTotalFallback(input, output, cacheCreation, cacheRead, 0, total)

	// Authoritative stored total: prefer the provider total, else the sum of the
	// additive components (plus any overflow attributed to extra).
	storedTotal := total
	if storedTotal < input+output+cacheCreation+cacheRead+extra {
		storedTotal = input + output + cacheCreation + cacheRead + extra
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

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
