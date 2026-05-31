package hermes

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"aiusage/internal/adapter"
	"aiusage/internal/model"
)

// makeStateDB creates a Hermes-shaped state.db with a sessions table and runs
// the supplied seed callback to insert rows. It returns the db path.
func makeStateDB(t *testing.T, home string, seed func(*sql.DB)) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	dbPath := filepath.Join(home, dbName)
	db, err := sql.Open(driverName, "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		model TEXT,
		billing_provider TEXT,
		started_at TEXT,
		ended_at TEXT,
		input_tokens INTEGER,
		output_tokens INTEGER,
		cache_read_tokens INTEGER,
		cache_write_tokens INTEGER,
		reasoning_tokens INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if seed != nil {
		seed(db)
	}
	return dbPath
}

func insertSession(t *testing.T, db *sql.DB, id, mdl, provider, startedAt string, in, out, cRead, cWrite, reason int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO sessions
		(id, model, billing_provider, started_at, ended_at,
		 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, mdl, provider, startedAt, "", in, out, cRead, cWrite, reason)
	if err != nil {
		t.Fatalf("insert session %s: %v", id, err)
	}
}

func TestCollectSingleSession(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".hermes")
	dbPath := makeStateDB(t, home, func(db *sql.DB) {
		// in=100 out=200 cacheRead=30 cacheWrite=40 reasoning=15
		insertSession(t, db, "sess-1", "claude-3-5-sonnet", "anthropic", "2026-05-29T10:00:00Z",
			100, 200, 30, 40, 15)
	})

	a := New()
	if a.ID() != model.ToolHermes {
		t.Fatalf("ID = %q, want %q", a.ID(), model.ToolHermes)
	}

	cfg := adapter.DiscoverConfig{Home: dir}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("Discover returned %d sources, want 1", len(srcs))
	}
	src := srcs[0]
	if src.Class != model.Aggregate {
		t.Fatalf("Source.Class = %q, want %q", src.Class, model.Aggregate)
	}
	if src.Path != dbPath {
		t.Fatalf("Source.Path = %q, want %q", src.Path, dbPath)
	}

	obs, err := a.Collect(context.Background(), src)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Events) != 0 {
		t.Fatalf("Collect produced %d events, want 0 (aggregate uses Snapshots)", len(obs.Events))
	}
	if len(obs.Snapshots) != 1 {
		t.Fatalf("Collect produced %d snapshots, want 1", len(obs.Snapshots))
	}

	s := obs.Snapshots[0]
	if s.Key != "sess-1" {
		t.Errorf("Key = %q, want sess-1", s.Key)
	}
	if s.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", s.SessionID)
	}
	if s.Tool != model.ToolHermes {
		t.Errorf("Tool = %q, want %q", s.Tool, model.ToolHermes)
	}
	if s.Model != "claude-3-5-sonnet" {
		t.Errorf("Model = %q, want claude-3-5-sonnet", s.Model)
	}
	if s.Project != metaProject {
		t.Errorf("Project = %q, want %q", s.Project, metaProject)
	}
	if s.SourcePath != dbPath {
		t.Errorf("SourcePath = %q, want %q", s.SourcePath, dbPath)
	}
	if s.InputTokens != 100 || s.OutputTokens != 200 {
		t.Errorf("in/out = %d/%d, want 100/200", s.InputTokens, s.OutputTokens)
	}
	// cache_write_tokens -> CacheCreation; cache_read_tokens -> CacheRead.
	if s.CacheCreationTokens != 40 {
		t.Errorf("CacheCreation = %d, want 40 (from cache_write_tokens)", s.CacheCreationTokens)
	}
	if s.CacheReadTokens != 30 {
		t.Errorf("CacheRead = %d, want 30 (from cache_read_tokens)", s.CacheReadTokens)
	}
	if s.ReasoningTokens != 15 {
		t.Errorf("Reasoning = %d, want 15", s.ReasoningTokens)
	}
	// Total = in + out + cacheCreation + cacheRead = 100+200+40+30 = 370.
	if s.TotalTokens != 370 {
		t.Errorf("Total = %d, want 370 (in+out+cacheC+cacheR; reasoning excluded)", s.TotalTokens)
	}
	if s.ObservedTime.IsZero() {
		t.Errorf("ObservedTime is zero, want set to now")
	}
	if !strings.Contains(s.Raw, `"billing_provider":"anthropic"`) {
		t.Errorf("Raw missing billing_provider: %q", s.Raw)
	}
	if !strings.Contains(s.Raw, `"started_at":"2026-05-29T10:00:00Z"`) {
		t.Errorf("Raw missing started_at: %q", s.Raw)
	}
}

func TestCollectFiltersBlankModelAndCountsRows(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".hermes")
	makeStateDB(t, home, func(db *sql.DB) {
		insertSession(t, db, "ok-1", "gpt-5", "openai", "", 10, 20, 0, 0, 0)
		insertSession(t, db, "ok-2", "gpt-5", "openai", "", 5, 5, 0, 0, 0)
		// model is whitespace-only -> excluded by query's TRIM(model) != ''.
		insertSession(t, db, "blank", "   ", "openai", "", 999, 999, 999, 999, 999)
		// model is NULL -> excluded.
		_, err := db.Exec(`INSERT INTO sessions (id, model) VALUES ('nullmodel', NULL)`)
		if err != nil {
			t.Fatalf("insert null model: %v", err)
		}
	})

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: dir})
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover: err=%v sources=%d", err, len(srcs))
	}
	obs, err := a.Collect(context.Background(), srcs[0])
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(obs.Snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2 (blank/null model excluded)", len(obs.Snapshots))
	}
	keys := map[string]bool{}
	for _, s := range obs.Snapshots {
		keys[s.Key] = true
	}
	if !keys["ok-1"] || !keys["ok-2"] {
		t.Errorf("expected keys ok-1 and ok-2, got %v", keys)
	}
}

func TestDiscoverHonorsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	envHome := filepath.Join(dir, "custom-hermes")
	dbPath := makeStateDB(t, envHome, func(db *sql.DB) {
		insertSession(t, db, "e-1", "model-x", "prov", "", 1, 1, 0, 0, 0)
	})

	t.Setenv(homeEnv, envHome)
	a := New()
	// Home points elsewhere; HERMES_HOME must win.
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: filepath.Join(dir, "ignored")})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Path != dbPath {
		t.Fatalf("env override not honored: %+v", srcs)
	}
}

func TestDiscoverNoDBReturnsEmpty(t *testing.T) {
	dir := t.TempDir() // no .hermes/state.db
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: dir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("got %d sources, want 0 when no state.db", len(srcs))
	}
}

func TestCollectReadOnlyDoesNotCreateDB(t *testing.T) {
	// Collect against a non-existent path must NOT create it (mode=ro).
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope", "state.db")
	a := New()
	src := adapter.Source{Tool: model.ToolHermes, Class: model.Aggregate, Path: missing}
	if _, err := a.Collect(context.Background(), src); err == nil {
		t.Fatalf("Collect on missing db should error under mode=ro")
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatalf("read-only Collect created the db file at %s", missing)
	}
}
