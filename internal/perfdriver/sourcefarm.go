package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/adapter/all"
	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

const (
	sourceFarmProfile         = "source-farm-v1"
	sourceFarmSources         = 2_500
	sourceFarmRecords         = 10_000
	sourceFarmUnchangedCycles = 100
)

var sourceFarmTools = []string{
	model.ToolClaudeCode, model.ToolCodex, model.ToolCopilot, model.ToolOpenCode,
	model.ToolHermes, model.ToolAgy, model.ToolCline, model.ToolCrush,
	model.ToolDSH, model.ToolGoose, model.ToolKimiCode, model.ToolPi,
	model.ToolOpenClaw, model.ToolQwenCode, model.ToolReasonix,
}

var sourceFarmFixturePaths = []string{
	"adapter/agy/testdata/live-2026-08-31.jsonl",
	"adapter/copilot/testdata/otel.jsonl",
	"adapter/copilot/testdata/schema.sql",
	"adapter/copilot/testdata/secrets.sql",
	"adapter/copilot/testdata/usage.sql",
	"adapter/copilot/testdata/session-state",
	"adapter/clinecli/testdata/live",
	"adapter/crush/testdata/schema.sql",
	"adapter/crush/testdata/costly.sql",
	"adapter/dsh/testdata/session.jsonl",
	"adapter/goose/testdata/sessions.sql",
	"adapter/kimicode/testdata/home/.kimi-code",
	"adapter/pi/testdata/pi/agent",
	"adapter/pi/testdata/openclaw/.openclaw",
	"adapter/qwencode/testdata/live",
	"adapter/reasonix/testdata/live-2026-08-16.jsonl",
}

// These are every environment variable that may redirect built-in adapter
// discovery. Source-farm commands clear them for their process and rely only on
// the explicit per-tool roots below. Otherwise a developer's live Codex tree,
// or a CI image with one stray XDG variable, silently changes the workload.
var sourceFarmDiscoveryEnv = []string{
	"CLAUDE_CONFIG_DIR", "CLINE_DIR", "CLINE_DATA_DIR", "CLINE_SESSION_DATA_DIR",
	"CLINE_DB_DATA_DIR", "CODEX_HOME", "COPILOT_OTEL_FILE_EXPORTER_PATH",
	"CRUSH_GLOBAL_DATA", "XDG_DATA_HOME", "DSH_HOME", "GOOSE_PATH_ROOT",
	"HERMES_HOME", "KIMI_CODE_HOME", "KIMI_CODE_DATA_DIR", "OPENCODE_DATA_DIR",
	"PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "OPENCLAW_STATE_DIR",
	"OPENCLAW_HOME", "OPENCLAW_AGENT_DIR", "QWEN_RUNTIME_DIR", "QWEN_HOME",
	"REASONIX_STATE_HOME", "REASONIX_HOME",
}

type sourceFarmManifest struct {
	Profile              string         `json:"profile"`
	Sources              int            `json:"sources"`
	Records              int            `json:"records"`
	Adapters             int            `json:"adapters"`
	SourcesByTool        map[string]int `json:"sources_by_tool"`
	CanonicalManifest    string         `json:"canonical_manifest"`
	CanonicalFixtureRoot string         `json:"canonical_fixture_root"`
	FixturePaths         []string       `json:"fixture_paths"`
}

type sourceFarmSummary struct {
	Profile              string `json:"profile"`
	Adapters             int    `json:"adapters"`
	Sources              int    `json:"sources"`
	Records              int    `json:"records"`
	EventsInserted       int    `json:"events_inserted"`
	ActivityInserted     int    `json:"activity_inserted"`
	TurnContextsInserted int    `json:"turn_contexts_inserted"`
}

type sourceFarmContractReport struct {
	Schema          string            `json:"schema"`
	Profile         string            `json:"profile"`
	Initial         sourceFarmSummary `json:"initial"`
	Changed         sourceFarmSummary `json:"changed"`
	ChangedTools    []string          `json:"changed_tools"`
	ChangedShapes   []string          `json:"changed_shapes"`
	UnchangedCycles int               `json:"unchanged_cycles"`
}

func withNeutralSourceFarmEnvironment(fn func() error) error {
	type savedValue struct {
		value string
		set   bool
	}
	saved := make(map[string]savedValue, len(sourceFarmDiscoveryEnv))
	for _, key := range sourceFarmDiscoveryEnv {
		value, set := os.LookupEnv(key)
		saved[key] = savedValue{value: value, set: set}
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("clear %s: %w", key, err)
		}
	}
	defer func() {
		for _, key := range sourceFarmDiscoveryEnv {
			prior := saved[key]
			if prior.set {
				_ = os.Setenv(key, prior.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}()
	return fn()
}

func sourceFarmConfig(root string) adapter.DiscoverConfig {
	toolRoot := func(tool string) string { return filepath.Join(root, "sources", tool) }
	return adapter.DiscoverConfig{
		Home: filepath.Join(root, "empty-home"),
		Overrides: map[string]string{
			model.ToolClaudeCode: toolRoot(model.ToolClaudeCode),
			model.ToolCodex:      toolRoot(model.ToolCodex),
			model.ToolCopilot:    toolRoot(model.ToolCopilot),
			model.ToolOpenCode:   toolRoot(model.ToolOpenCode),
			model.ToolHermes:     toolRoot(model.ToolHermes),
			model.ToolAgy:        toolRoot(model.ToolAgy),
			model.ToolCline:      toolRoot(model.ToolCline),
			model.ToolCrush:      filepath.Join(root, "sources", "crush-global"),
			model.ToolDSH:        toolRoot(model.ToolDSH),
			model.ToolGoose:      toolRoot(model.ToolGoose),
			model.ToolKimiCode:   toolRoot(model.ToolKimiCode),
			model.ToolPi:         toolRoot(model.ToolPi),
			model.ToolOpenClaw:   toolRoot(model.ToolOpenClaw),
			model.ToolQwenCode:   toolRoot(model.ToolQwenCode),
			model.ToolReasonix:   toolRoot(model.ToolReasonix),
		},
	}
}

func generateSourceFarm(root, fixtures, manifestPath string, expectedSources, expectedRecords int) error {
	if root == "" || fixtures == "" || manifestPath == "" || expectedSources <= 0 || expectedRecords <= 0 {
		return errors.New("generate-source-farm requires --root, --fixtures, and --manifest")
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		return fmt.Errorf("refusing to replace non-empty source farm %s", root)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect source farm %s: %w", root, err)
	}
	if err := requireSourceFarmFixtures(fixtures); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "empty-home"), 0o755); err != nil {
		return fmt.Errorf("create source farm: %w", err)
	}
	if err := buildNonCodexFarm(root, fixtures); err != nil {
		return err
	}

	var initial sourceFarmSummary
	err := withNeutralSourceFarmEnvironment(func() error {
		var err error
		initial, err = runSourceFarmCatchup(root, "")
		return err
	})
	if err != nil {
		return fmt.Errorf("measure non-Codex farm: %w", err)
	}
	codexSources := expectedSources - initial.Sources
	codexRecords := expectedRecords - initial.Records
	if codexSources <= 0 || codexRecords < codexSources {
		return fmt.Errorf("non-Codex fixture leaves invalid Codex budget: sources=%d records=%d", codexSources, codexRecords)
	}
	if err := buildCodexFarm(filepath.Join(root, "sources", model.ToolCodex), codexSources, codexRecords); err != nil {
		return err
	}

	var final sourceFarmSummary
	err = withNeutralSourceFarmEnvironment(func() error {
		var err error
		final, err = runSourceFarmCatchup(root, "")
		return err
	})
	if err != nil {
		return fmt.Errorf("validate source farm: %w", err)
	}
	if final.Adapters != len(sourceFarmTools) || final.Sources != expectedSources || final.Records != expectedRecords {
		return fmt.Errorf("source farm shape = adapters %d, sources %d, records %d; want %d, %d, %d",
			final.Adapters, final.Sources, final.Records, len(sourceFarmTools), expectedSources, expectedRecords)
	}

	var byTool map[string]int
	if err := withNeutralSourceFarmEnvironment(func() error {
		var err error
		byTool, err = discoverSourceCounts(context.Background(), root)
		return err
	}); err != nil {
		return err
	}
	manifest := sourceFarmManifest{
		Profile:              sourceFarmProfile,
		Sources:              final.Sources,
		Records:              final.Records,
		Adapters:             final.Adapters,
		SourcesByTool:        byTool,
		CanonicalManifest:    "adapter/compatibility.json",
		CanonicalFixtureRoot: ".",
		FixturePaths:         append([]string(nil), sourceFarmFixturePaths...),
	}
	if err := writeJSON(manifestPath, manifest); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(manifest)
}

func requireSourceFarmFixtures(root string) error {
	for _, rel := range append([]string{"adapter/compatibility.json"}, sourceFarmFixturePaths...) {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("canonical fixture %s: %w", rel, err)
		}
	}
	return nil
}

func buildNonCodexFarm(root, fixtures string) error {
	sources := filepath.Join(root, "sources")
	path := func(tool string, parts ...string) string {
		allParts := append([]string{sources, tool}, parts...)
		return filepath.Join(allParts...)
	}
	fixture := func(rel string) string { return filepath.Join(fixtures, filepath.FromSlash(rel)) }

	if err := writeFarmFile(path(model.ToolClaudeCode, "projects", "farm", "session.jsonl"),
		[]byte(`{"timestamp":"2026-08-31T00:00:00Z","sessionId":"claude-farm","requestId":"request-1","message":{"id":"message-1","model":"claude-sonnet-4-5","usage":{"input_tokens":40,"output_tokens":10}}}`+"\n")); err != nil {
		return err
	}

	copilotRoot := path(model.ToolCopilot, ".copilot")
	if err := copyFarmFile(fixture("adapter/copilot/testdata/otel.jsonl"), filepath.Join(copilotRoot, "otel", "otel.jsonl")); err != nil {
		return err
	}
	if err := copyFarmTree(fixture("adapter/copilot/testdata/session-state"), filepath.Join(copilotRoot, "session-state")); err != nil {
		return err
	}
	if err := buildSQLDatabase(filepath.Join(copilotRoot, "session-store.db"), true,
		fixture("adapter/copilot/testdata/schema.sql"),
		fixture("adapter/copilot/testdata/secrets.sql"),
		fixture("adapter/copilot/testdata/usage.sql")); err != nil {
		return fmt.Errorf("build Copilot fixture: %w", err)
	}

	opencodeRoot := path(model.ToolOpenCode)
	opencodeData := `{"id":"farm-db","sessionID":"opencode-farm","providerID":"openai","modelID":"gpt-5","time":{"created":1788134400000},"tokens":{"input":30,"output":10,"total":40},"path":{"cwd":"/fixture/opencode"}}`
	if err := buildInlineDatabase(filepath.Join(opencodeRoot, "opencode.db"), []string{
		`CREATE TABLE message (id TEXT, session_id TEXT, data TEXT)`,
		`CREATE TABLE part (id TEXT, message_id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
	}, []sqlStatement{{query: `INSERT INTO message VALUES (?,?,?)`, args: []any{"farm-db", "opencode-farm", opencodeData}}}); err != nil {
		return fmt.Errorf("build OpenCode fixture: %w", err)
	}
	opencodeJSON := `{"id":"farm-json","sessionID":"opencode-farm","providerID":"openai","modelID":"gpt-5","time":{"created":1788134401000},"tokens":{"input":20,"output":5,"total":25},"path":{"cwd":"/fixture/opencode"}}`
	if err := writeFarmFile(filepath.Join(opencodeRoot, "storage", "message", "opencode-farm", "farm-json.json"), []byte(opencodeJSON)); err != nil {
		return err
	}

	if err := buildInlineDatabase(path(model.ToolHermes, "state.db"), []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT, billing_provider TEXT, started_at TEXT, ended_at TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER)`,
	}, []sqlStatement{{query: `INSERT INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?)`, args: []any{"hermes-farm", "claude-sonnet-4-5", "anthropic", "2026-08-31T00:00:00Z", "", 50, 10, 2, 1, 3}}}); err != nil {
		return fmt.Errorf("build Hermes fixture: %w", err)
	}

	if err := copyFarmFile(fixture("adapter/agy/testdata/live-2026-08-31.jsonl"),
		path(model.ToolAgy, "aiusage-stream.jsonl")); err != nil {
		return err
	}

	if err := copyFarmTree(fixture("adapter/clinecli/testdata/live"), path(model.ToolCline)); err != nil {
		return err
	}

	crushProject := filepath.Join(sources, "crush-project")
	crushDB := filepath.Join(crushProject, ".crush", "crush.db")
	if err := buildSQLDatabase(crushDB, true,
		fixture("adapter/crush/testdata/schema.sql"), fixture("adapter/crush/testdata/costly.sql")); err != nil {
		return fmt.Errorf("build Crush fixture: %w", err)
	}
	if err := writeSourceFarmCrushIndex(root); err != nil {
		return err
	}

	if err := copyFarmFile(fixture("adapter/dsh/testdata/session.jsonl"),
		path(model.ToolDSH, "sessions", "farm-project", "farm-session", "session.jsonl")); err != nil {
		return err
	}
	if err := buildSQLDatabase(path(model.ToolGoose, "sessions", "sessions.db"), false,
		fixture("adapter/goose/testdata/sessions.sql")); err != nil {
		return fmt.Errorf("build Goose fixture: %w", err)
	}
	if err := copyFarmTree(fixture("adapter/kimicode/testdata/home/.kimi-code"), path(model.ToolKimiCode)); err != nil {
		return err
	}
	if err := copyFarmTree(fixture("adapter/pi/testdata/pi/agent"), path(model.ToolPi)); err != nil {
		return err
	}
	if err := copyFarmTree(fixture("adapter/pi/testdata/openclaw/.openclaw"), path(model.ToolOpenClaw)); err != nil {
		return err
	}
	if err := copyFarmTree(fixture("adapter/qwencode/testdata/live"), path(model.ToolQwenCode)); err != nil {
		return err
	}
	if err := copyFarmFile(fixture("adapter/reasonix/testdata/live-2026-08-16.jsonl"),
		path(model.ToolReasonix, "stats", "2026-08-16.jsonl")); err != nil {
		return err
	}
	return nil
}

func buildCodexFarm(root string, sourceCount, recordCount int) error {
	globalRecord := 0
	for i := 0; i < sourceCount; i++ {
		n := recordCount / sourceCount
		if i < recordCount%sourceCount {
			n++
		}
		var b strings.Builder
		b.WriteString(`{"type":"turn_context","payload":{"model":"gpt-5-codex"}}` + "\n")
		for j := 1; j <= n; j++ {
			globalRecord++
			input := int64(j * 10)
			output := int64(j)
			fmt.Fprintf(&b,
				`{"type":"event_msg","timestamp":"%s","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}}`+"\n",
				time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC).Add(time.Duration(globalRecord)*time.Second).Format(time.RFC3339Nano),
				input, output, input+output)
		}
		if err := writeFarmFile(filepath.Join(root, "sessions", fmt.Sprintf("session-%04d.jsonl", i)), []byte(b.String())); err != nil {
			return err
		}
	}
	return nil
}

func discoverSourceCounts(ctx context.Context, root string) (map[string]int, error) {
	counts := make(map[string]int, len(sourceFarmTools))
	reg := all.Default()
	for _, ad := range reg.All() {
		sources, err := ad.Discover(ctx, sourceFarmConfig(root))
		if err != nil {
			return nil, fmt.Errorf("discover %s: %w", ad.ID(), err)
		}
		counts[ad.ID()] = len(sources)
	}
	if len(counts) != len(sourceFarmTools) {
		return nil, fmt.Errorf("source farm discovered %d adapters, want %d", len(counts), len(sourceFarmTools))
	}
	for _, tool := range sourceFarmTools {
		if counts[tool] == 0 {
			return nil, fmt.Errorf("source farm has no %s source", tool)
		}
	}
	return counts, nil
}

func sourceFarmDiscoverySummary(root string) (sourceFarmSummary, error) {
	counts, err := discoverSourceCounts(context.Background(), root)
	if err != nil {
		return sourceFarmSummary{}, err
	}
	total := 0
	for _, count := range counts {
		total += count
	}
	return sourceFarmSummary{Profile: sourceFarmProfile, Adapters: len(counts), Sources: total}, nil
}

func runSourceFarmCatchup(root, dbPath string) (sourceFarmSummary, error) {
	removeDB := false
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "aiusage-source-farm-catchup-")
		if err != nil {
			return sourceFarmSummary{}, err
		}
		defer os.RemoveAll(dir)
		dbPath = filepath.Join(dir, "usage.db")
		removeDB = true
	}
	if !removeDB {
		if _, err := os.Stat(dbPath); err == nil {
			return sourceFarmSummary{}, fmt.Errorf("refusing to replace source-farm database %s", dbPath)
		} else if !os.IsNotExist(err) {
			return sourceFarmSummary{}, err
		}
	}
	ledger, err := store.Open(dbPath)
	if err != nil {
		return sourceFarmSummary{}, fmt.Errorf("open source-farm store: %w", err)
	}
	stats, runErr := collect.RunOnce(context.Background(), all.Default(), ledger, sourceFarmConfig(root))
	closeErr := ledger.Close()
	if runErr != nil {
		return sourceFarmSummary{}, runErr
	}
	if closeErr != nil {
		return sourceFarmSummary{}, closeErr
	}
	if len(stats.Errors) > 0 {
		return sourceFarmSummary{}, fmt.Errorf("source-farm catchup errors: %s", strings.Join(stats.Errors, "; "))
	}
	return summarizeSourceFarm(stats), nil
}

func warmSourceFarm(root, dbPath string) (sourceFarmSummary, error) {
	if dbPath == "" {
		return sourceFarmSummary{}, errors.New("source-farm-warm requires --db")
	}
	initial, err := runSourceFarmCatchup(root, dbPath)
	if err != nil {
		return sourceFarmSummary{}, err
	}
	ledger, err := store.Open(dbPath)
	if err != nil {
		return sourceFarmSummary{}, err
	}
	defer ledger.Close()
	for i := 0; i < 2; i++ {
		stats, err := collect.RunOnce(context.Background(), all.Default(), ledger, sourceFarmConfig(root))
		if err != nil || len(stats.Errors) > 0 {
			return sourceFarmSummary{}, fmt.Errorf("warm source farm pass %d: err=%v errors=%v", i+1, err, stats.Errors)
		}
		if stats.EventsInserted != 0 || stats.ActivityInserted != 0 || stats.TurnContextsInserted != 0 {
			return sourceFarmSummary{}, fmt.Errorf("warm source farm pass %d inserted usage=%d activity=%d contexts=%d",
				i+1, stats.EventsInserted, stats.ActivityInserted, stats.TurnContextsInserted)
		}
	}
	return initial, nil
}

func runSourceFarmUnchanged(root, dbPath string, cycles int) (sourceFarmSummary, error) {
	if dbPath == "" || cycles <= 0 {
		return sourceFarmSummary{}, errors.New("source-farm-unchanged requires --db and positive --cycles")
	}
	ledger, err := store.Open(dbPath)
	if err != nil {
		return sourceFarmSummary{}, err
	}
	defer ledger.Close()
	var last sourceFarmSummary
	for i := 0; i < cycles; i++ {
		stats, err := collect.RunOnce(context.Background(), all.Default(), ledger, sourceFarmConfig(root))
		if err != nil || len(stats.Errors) > 0 {
			return sourceFarmSummary{}, fmt.Errorf("unchanged source farm pass %d: err=%v errors=%v", i+1, err, stats.Errors)
		}
		if stats.EventsInserted != 0 || stats.ActivityInserted != 0 || stats.TurnContextsInserted != 0 {
			return sourceFarmSummary{}, fmt.Errorf("unchanged source farm pass %d inserted usage=%d activity=%d contexts=%d",
				i+1, stats.EventsInserted, stats.ActivityInserted, stats.TurnContextsInserted)
		}
		last = summarizeSourceFarm(stats)
	}
	return last, nil
}

func summarizeSourceFarm(stats collect.CycleStats) sourceFarmSummary {
	return sourceFarmSummary{
		Profile:              sourceFarmProfile,
		Adapters:             stats.Adapters,
		Sources:              stats.Sources,
		Records:              stats.EventsInserted + stats.ActivityInserted + stats.TurnContextsInserted,
		EventsInserted:       stats.EventsInserted,
		ActivityInserted:     stats.ActivityInserted,
		TurnContextsInserted: stats.TurnContextsInserted,
	}
}

func runSourceFarmContract(root, outPath string, expectedSources, expectedRecords, unchangedCycles int) error {
	if root == "" || outPath == "" || expectedSources <= 0 || expectedRecords <= 0 || unchangedCycles <= 0 {
		return errors.New("source-farm-contract requires --root and --out")
	}
	work, err := os.MkdirTemp("", "aiusage-source-farm-contract-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	copyRoot := filepath.Join(work, "farm")
	if err := copyFarmTree(root, copyRoot); err != nil {
		return fmt.Errorf("copy source farm: %w", err)
	}
	if err := writeSourceFarmCrushIndex(copyRoot); err != nil {
		return err
	}
	dbPath := filepath.Join(work, "usage.db")
	initial, err := warmSourceFarm(copyRoot, dbPath)
	if err != nil {
		return err
	}
	if initial.Adapters != len(sourceFarmTools) || initial.Sources != expectedSources || initial.Records != expectedRecords {
		return fmt.Errorf("initial source farm = %+v", initial)
	}
	before, err := usageEventsByTool(dbPath)
	if err != nil {
		return err
	}
	shapes, err := mutateSourceFarm(copyRoot)
	if err != nil {
		return err
	}
	ledger, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	stats, runErr := collect.RunOnce(context.Background(), all.Default(), ledger, sourceFarmConfig(copyRoot))
	closeErr := ledger.Close()
	if runErr != nil || closeErr != nil || len(stats.Errors) > 0 {
		return fmt.Errorf("changed source farm: run=%v close=%v errors=%v", runErr, closeErr, stats.Errors)
	}
	after, err := usageEventsByTool(dbPath)
	if err != nil {
		return err
	}
	var changedTools []string
	for _, tool := range sourceFarmTools {
		if after[tool] <= before[tool] {
			return fmt.Errorf("source-farm mutation produced no new %s usage event (before=%d after=%d; changed cycle inserted usage=%d activity=%d contexts=%d)",
				tool, before[tool], after[tool], stats.EventsInserted, stats.ActivityInserted, stats.TurnContextsInserted)
		}
		changedTools = append(changedTools, tool)
	}
	if _, err := runSourceFarmUnchanged(copyRoot, dbPath, unchangedCycles); err != nil {
		return err
	}
	report := sourceFarmContractReport{
		Schema:          "source-farm-contract-v1",
		Profile:         sourceFarmProfile,
		Initial:         initial,
		Changed:         summarizeSourceFarm(stats),
		ChangedTools:    changedTools,
		ChangedShapes:   shapes,
		UnchangedCycles: unchangedCycles,
	}
	if err := writeJSON(outPath, report); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

func usageEventsByTool(dbPath string) (map[string]int64, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT tool, COUNT(*) FROM usage_events GROUP BY tool`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64, len(sourceFarmTools))
	for rows.Next() {
		var tool string
		var count int64
		if err := rows.Scan(&tool, &count); err != nil {
			return nil, err
		}
		out[tool] = count
	}
	return out, rows.Err()
}

func writeSourceFarmCrushIndex(root string) error {
	project := filepath.Join(root, "sources", "crush-project")
	index := map[string]any{"projects": []map[string]string{{
		"path": project, "data_dir": filepath.Join(project, ".crush"),
	}}}
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return writeFarmFile(filepath.Join(root, "sources", "crush-global", "projects.json"), raw)
}

func mutateSourceFarm(root string) ([]string, error) {
	path := func(tool string, parts ...string) string {
		allParts := append([]string{root, "sources", tool}, parts...)
		return filepath.Join(allParts...)
	}
	var changed []string
	appendShape := func(name, file, line string) error {
		if err := appendFarmFile(file, []byte(line+"\n")); err != nil {
			return err
		}
		changed = append(changed, name)
		return nil
	}

	if err := appendShape("claude-code/project-transcript", path(model.ToolClaudeCode, "projects", "farm", "session.jsonl"),
		`{"timestamp":"2026-08-31T00:10:00Z","sessionId":"claude-farm","requestId":"request-2","message":{"id":"message-2","model":"claude-sonnet-4-5","usage":{"input_tokens":20,"output_tokens":5}}}`); err != nil {
		return nil, err
	}
	codexFile := path(model.ToolCodex, "sessions", "session-0000.jsonl")
	count, err := countMarker(codexFile, `"token_count"`)
	if err != nil {
		return nil, err
	}
	input, output := int64((count+1)*10), int64(count+1)
	if err := appendShape("codex/session-rollout", codexFile,
		fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-09-01T00:00:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}}`, input, output, input+output)); err != nil {
		return nil, err
	}
	if err := appendShape("copilot/otel-jsonl", path(model.ToolCopilot, ".copilot", "otel", "otel.jsonl"),
		`{"type":"span","traceId":"farm-trace","spanId":"farm-span","name":"chat farm","endTime":[1788135000,0],"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"gpt-5","gen_ai.conversation.id":"farm-conversation","gen_ai.usage.input_tokens":12,"gen_ai.usage.output_tokens":3}}`); err != nil {
		return nil, err
	}
	copilotState, err := firstMatchingFile(path(model.ToolCopilot, ".copilot", "session-state"), "events.jsonl")
	if err != nil {
		return nil, err
	}
	if err := appendShape("copilot/session-state", copilotState,
		`{"type":"skill.invoked","id":"farm-skill-event","timestamp":"2026-08-31T00:11:00Z","data":{"name":"source-farm","source":"local","trigger":"test"}}`); err != nil {
		return nil, err
	}

	opencodeDB := path(model.ToolOpenCode, "opencode.db")
	if err := execStatements(opencodeDB, []sqlStatement{{query: `INSERT INTO message VALUES (?,?,?)`, args: []any{"farm-db-appended", "opencode-farm", `{"id":"farm-db-appended","sessionID":"opencode-farm","providerID":"openai","modelID":"gpt-5","time":{"created":1788135000000},"tokens":{"input":9,"output":2,"total":11}}`}}}); err != nil {
		return nil, err
	}
	changed = append(changed, "opencode/sqlite-message")
	if err := writeFarmFile(path(model.ToolOpenCode, "storage", "message", "opencode-farm", "farm-json-appended.json"),
		[]byte(`{"id":"farm-json-appended","sessionID":"opencode-farm","providerID":"openai","modelID":"gpt-5","time":{"created":1788135001000},"tokens":{"input":8,"output":2,"total":10}}`)); err != nil {
		return nil, err
	}
	changed = append(changed, "opencode/json-message-tree")
	if err := execStatements(path(model.ToolHermes, "state.db"), []sqlStatement{{query: `UPDATE sessions SET input_tokens=input_tokens+7, output_tokens=output_tokens+2 WHERE id='hermes-farm'`}}); err != nil {
		return nil, err
	}
	changed = append(changed, "hermes/session-counters")
	if err := appendShape("agy/stream-result-cumulative", path(model.ToolAgy, "aiusage-stream.jsonl"),
		`{"event":"result","result":{"conversation_id":"11111111-2222-4333-8444-555555555555","status":"SUCCESS","response":"SANITIZED","duration_seconds":0,"num_turns":3,"usage":{"input_tokens":35199,"output_tokens":6620,"thinking_tokens":6595,"cache_read_tokens":0,"total_tokens":41819}}}`); err != nil {
		return nil, err
	}

	clineFile, err := firstMatchingFile(path(model.ToolCline), ".messages.json")
	if err != nil {
		return nil, err
	}
	if err := appendClineMessage(clineFile); err != nil {
		return nil, err
	}
	changed = append(changed, "cline/session-message-rewrite")
	if err := execStatements(filepath.Join(root, "sources", "crush-project", ".crush", "crush.db"), []sqlStatement{{query: `UPDATE sessions SET cost=cost+0.01 WHERE id='sess-paid-single'`}}); err != nil {
		return nil, err
	}
	changed = append(changed, "crush/project-cost-row")
	if err := appendShape("dsh/session-transcript", path(model.ToolDSH, "sessions", "farm-project", "farm-session", "session.jsonl"),
		`{"type":"assistant/message","seq":9999,"time":1788135200000,"data":{"turn":999,"step":1,"message":{"role":"assistant","content":[],"source":{"kind":"model","provider":"openai","model":"gpt-5"},"id":"farm-dsh-appended"},"usage":{"inputTokens":6,"outputTokens":2}}}`); err != nil {
		return nil, err
	}
	if err := execStatements(path(model.ToolGoose, "sessions", "sessions.db"), []sqlStatement{{query: `INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens, output_tokens, total_tokens, cost, cost_source, is_compaction) VALUES (9000, 'farm-goose', 1788135300, 'gpt-5', 6, 2, 8, NULL, NULL, 0)`}}); err != nil {
		return nil, err
	}
	changed = append(changed, "goose/usage-ledger-row")

	kimiFile, err := firstMatchingFile(path(model.ToolKimiCode), "wire.jsonl")
	if err != nil {
		return nil, err
	}
	if err := appendFarmFile(kimiFile, []byte(
		`{"type":"llm.request","provider":"openai","model":"gpt-5","modelAlias":"farm","turnStep":"999.1","time":1788135400000}`+"\n"+
			`{"type":"usage.record","model":"farm","usage":{"inputOther":6,"output":2,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":1788135401000}`+"\n")); err != nil {
		return nil, err
	}
	changed = append(changed, "kimi-code/wire-log")
	piFile, err := firstMatchingFile(path(model.ToolPi), ".jsonl")
	if err != nil {
		return nil, err
	}
	if err := appendShape("pi/session-transcript", piFile, piMutation("farm-pi-appended", 1788135500000)); err != nil {
		return nil, err
	}
	openClawFile, err := firstMatchingFile(path(model.ToolOpenClaw), ".jsonl")
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(openClawFile, ".trajectory.jsonl") {
		return nil, fmt.Errorf("selected excluded OpenClaw sidecar %s", openClawFile)
	}
	if err := appendShape("openclaw/session-transcript", openClawFile, piMutation("farm-openclaw-appended", 1788135600000)); err != nil {
		return nil, err
	}
	if err := appendShape("qwen-code/usage-ledger", path(model.ToolQwenCode, "usage", "token-usage-2026-08.jsonl"),
		`{"schemaVersion":1,"id":"farm-qwen-appended","timestamp":"2026-08-31T00:21:00Z","localDate":"2026-08-31","localMonth":"2026-08","sessionId":"farm-qwen","model":"qwen3-coder","authType":"openai","source":"main","inputTokens":7,"outputTokens":2,"cachedTokens":0,"thoughtsTokens":0,"totalTokens":9,"apiDurationMs":10}`); err != nil {
		return nil, err
	}
	if err := appendShape("reasonix/daily-ledger", path(model.ToolReasonix, "stats", "2026-08-16.jsonl"),
		`{"ts":"2026-08-31T00:22:00Z","model":"openai/gpt-5","source":"cli","prompt":7,"completion":2,"cache_miss":7,"total":9,"requests":1,"usage_source":"executor","cost_complete":false,"display_complete":false,"display_status":"unavailable","cost_estimated":true,"incomplete_reason":"no_price"}`); err != nil {
		return nil, err
	}
	sort.Strings(changed)
	return changed, nil
}

func piMutation(id string, timestamp int64) string {
	return fmt.Sprintf(`{"type":"message","id":"%s","timestamp":"2026-08-31T00:20:00Z","message":{"role":"assistant","content":[{"type":"text","text":"fixture"}],"api":"openai-completions","provider":"openai","model":"gpt-5","usage":{"input":6,"output":2,"cacheRead":0,"cacheWrite":0,"reasoning":0,"totalTokens":8,"cost":{"input":0.000001,"output":0.000001,"cacheRead":0,"cacheWrite":0,"total":0.000002}},"stopReason":"stop","timestamp":%d}}`, id, timestamp)
}

func appendClineMessage(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	messages, ok := doc["messages"].([]any)
	if !ok {
		return fmt.Errorf("cline fixture %s has no messages array", path)
	}
	doc["messages"] = append(messages, map[string]any{
		"id": "farm-cline-appended", "role": "assistant", "ts": float64(1788135100000),
		"modelInfo": map[string]any{"id": "gpt-5", "provider": "openai"},
		"metrics":   map[string]any{"inputTokens": float64(6), "outputTokens": float64(2), "cacheReadTokens": float64(0), "cacheWriteTokens": float64(0)},
	})
	updated, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, updated, 0o644)
}

type sqlStatement struct {
	query string
	args  []any
}

func buildInlineDatabase(path string, schema []string, rows []sqlStatement) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	for _, stmt := range rows {
		if _, err := db.Exec(stmt.query, stmt.args...); err != nil {
			return err
		}
	}
	return nil
}

func buildSQLDatabase(path string, wal bool, scripts ...string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	if wal {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			return err
		}
	}
	for _, script := range scripts {
		raw, err := os.ReadFile(script)
		if err != nil {
			return err
		}
		if _, err := db.Exec(string(raw)); err != nil {
			return fmt.Errorf("apply %s: %w", script, err)
		}
	}
	return nil
}

func execStatements(path string, statements []sqlStatement) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt.query, stmt.args...); err != nil {
			return fmt.Errorf("execute %s: %w", path, err)
		}
	}
	return nil
}

func writeFarmFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func appendFarmFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func copyFarmFile(from, to string) error {
	raw, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return writeFarmFile(to, raw)
}

func copyFarmTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source-farm fixtures may not contain symlink %s", path)
		}
		return copyFarmFile(path, target)
	})
}

func countMarker(path, marker string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strings.Count(string(raw), marker), nil
}

func firstMatchingFile(root, suffix string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), suffix) {
			found = path
			return io.EOF
		}
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no %s file below %s", suffix, root)
	}
	return found, nil
}
