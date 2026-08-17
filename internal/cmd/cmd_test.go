package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/adapter"
	"github.com/RandomCodeSpace/aiusage/adapter/claudecode"
	"github.com/RandomCodeSpace/aiusage/collect"
	"github.com/RandomCodeSpace/aiusage/store"
)

// claudeFixture is one realistic Claude Code transcript line: a Direct-shape
// record with message.usage, model, id, a session and an event timestamp.
const claudeFixture = `{"timestamp":"2026-05-29T12:00:00Z","cwd":"/home/dev/projects/demo","sessionId":"sess-1","requestId":"req-1","message":{"id":"msg-1","model":"claude-opus-4","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}}`

// writeClaudeFixture lays down <home>/.claude/projects/<seg>/<session>.jsonl and
// returns the home dir.
func writeClaudeFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	file := filepath.Join(projDir, "sess-1.jsonl")
	if err := os.WriteFile(file, []byte(claudeFixture+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return home
}

// offlineConfig writes a config that disables the pricing refresh and returns
// its path. Collection commands ask the pricing ladder to refresh itself once
// per cycle; without this the whole CLI suite would reach out to the network,
// which makes tests slow, flaky and dependent on someone else's uptime. Every
// other setting stays at its default, so callers get the same behaviour the
// previous "absent config file" path gave them.
func offlineConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"pricing":{"refresh":false}}`), 0o600); err != nil {
		t.Fatalf("write offline config: %v", err)
	}
	return path
}

// isolateState points XDG_STATE_HOME at a temp dir so `once` takes its
// collection lock (and pid/log paths) in the test sandbox, not the user's real
// state dir — a daemon on the host must never make these tests contend.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return dir
}

// runCmd resets the global flags, wires fresh stdout/stderr buffers and runs the
// root command with the given args. Returns combined stdout.
func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	flags = globalFlags{} // reset persistent-flag state between invocations
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestOnceInsertsClaudeEvent runs `once` against a temp --home holding a minimal
// claude-code transcript and asserts at least one event lands in the temp --db.
func TestOnceInsertsClaudeEvent(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	isolateState(t)

	// Neutralise any ambient config/env that could redirect paths.
	t.Setenv("AIUSAGE_DB", "")
	t.Setenv("AIUSAGE_HOME", "")
	t.Setenv("AIUSAGE_INTERVAL", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	out, err := runCmd(t, "--db", db, "--home", home, "--config", offlineConfig(t), "once")
	if err != nil {
		t.Fatalf("once failed: %v\noutput:\n%s", err, out)
	}

	// Verify directly against the store that at least one event was inserted.
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sum, err := st.Summarize(t.Context(), store.Filter{})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if sum.Totals.Events < 1 {
		t.Fatalf("expected >=1 stored event, got %d (once output:\n%s)", sum.Totals.Events, out)
	}
	if sum.Totals.Total < 1 {
		t.Fatalf("expected positive stored total tokens, got %d", sum.Totals.Total)
	}
}

// TestSummaryJSONParses runs `once` then `summary --json` and asserts the JSON
// output unmarshals into a store.Summary.
func TestSummaryJSONParses(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	isolateState(t)

	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once failed: %v", err)
	}

	// --no-daemon: summary is now a data-facing command that would otherwise
	// auto-start the daemon; disable it so the test stays hermetic and stdout
	// carries only the JSON.
	out, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon",
		"summary", "--by", "tool,model", "--json")
	if err != nil {
		t.Fatalf("summary --json failed: %v\noutput:\n%s", err, out)
	}

	var sum store.Summary
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("summary JSON did not parse: %v\noutput:\n%s", err, out)
	}
	if len(sum.Buckets) == 0 {
		t.Fatalf("expected at least one bucket in summary, got none:\n%s", out)
	}
}

// TestParseSpan covers the `last` duration grammar ^([0-9]+)(m|h|d)$.
func TestParseSpan(t *testing.T) {
	cases := map[string]bool{
		"30m": true, "6h": true, "2d": true, "0m": true,
		"": false, "30": false, "m": false, "1w": false, "1.5h": false, "-3h": false,
	}
	for in, want := range cases {
		_, ok := parseSpan(in)
		if ok != want {
			t.Errorf("parseSpan(%q) ok=%v, want %v", in, ok, want)
		}
	}
}

// TestParseBy validates the --by dimension parser.
func TestParseBy(t *testing.T) {
	if dims, err := parseBy("day, tool ,model"); err != nil || len(dims) != 3 {
		t.Fatalf("parseBy valid: dims=%v err=%v", dims, err)
	}
	if dims, err := parseBy(""); err != nil || dims != nil {
		t.Fatalf("parseBy empty: dims=%v err=%v", dims, err)
	}
	if _, err := parseBy("nope"); err == nil {
		t.Fatalf("parseBy invalid dimension: expected error")
	}
}

// TestClampInterval verifies the flag-override clamp matches the documented
// [60,1800] bound.
func TestClampInterval(t *testing.T) {
	cases := map[int]int{10: 60, 60: 60, 300: 300, 1800: 1800, 5000: 1800}
	for in, want := range cases {
		if got := clampInterval(in); got != want {
			t.Errorf("clampInterval(%d)=%d, want %d", in, got, want)
		}
	}
}

// TestLoadConfigHomeFlagMovesDerivedPaths: the --home flag re-derives the
// DB/PID/log paths so a sandboxed home never shares the production DB or daemon
// lock; an explicit --db still wins over the derivation.
func TestLoadConfigHomeFlagMovesDerivedPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME",
		"AIUSAGE_DB", "AIUSAGE_HOME", "AIUSAGE_INTERVAL",
	} {
		t.Setenv(k, "")
	}

	home := t.TempDir()
	prev := flags
	t.Cleanup(func() { flags = prev })

	flags = globalFlags{home: home, config: offlineConfig(t)}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if want := filepath.Join(home, ".local", "share", "aiusage", "usage.db"); cfg.DBPath != want {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, want)
	}
	if want := filepath.Join(home, ".local", "state", "aiusage", "aiusage.pid"); cfg.PIDPath != want {
		t.Errorf("PIDPath = %q, want %q", cfg.PIDPath, want)
	}
	if want := filepath.Join(home, ".local", "state", "aiusage", "aiusage.log"); cfg.LogPath != want {
		t.Errorf("LogPath = %q, want %q", cfg.LogPath, want)
	}

	flags.db = "/pinned/usage.db"
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig with --db: %v", err)
	}
	if cfg.DBPath != "/pinned/usage.db" {
		t.Errorf("DBPath = %q, want the explicit --db to win", cfg.DBPath)
	}
	if want := filepath.Join(home, ".local", "state", "aiusage", "aiusage.pid"); cfg.PIDPath != want {
		t.Errorf("PIDPath = %q, want %q (still home-derived)", cfg.PIDPath, want)
	}
}

// TestExportIncludeRaw runs `once` then `export`: the default export must omit
// the Raw payload (the usage-object audit blob), and --include-raw must restore
// it under the historical "Raw" key.
func TestExportIncludeRaw(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	isolateState(t)

	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once failed: %v", err)
	}

	out, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon", "export")
	if err != nil {
		t.Fatalf("export failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(out, `"Raw"`) || strings.Contains(out, "input_tokens") {
		t.Errorf("default export leaked the raw payload:\n%s", out)
	}

	out, err = runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon", "export", "--include-raw")
	if err != nil {
		t.Fatalf("export --include-raw failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, `"Raw"`) || !strings.Contains(out, "input_tokens") {
		t.Errorf("--include-raw export is missing the raw payload:\n%s", out)
	}
}

// TestNoRawConfigStoresNoPayload runs the whole CLI path with
// privacy.no_raw set and asserts the stored events carry no audit payload:
// config -> cmd -> collect -> adapter, the wiring the switch depends on.
func TestNoRawConfigStoresNoPayload(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	cfg := filepath.Join(t.TempDir(), "config.json")
	body := `{"pricing":{"refresh":false},"privacy":{"no_raw":true}}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once failed: %v", err)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// WithRaw, or the assertion would pass on the projection rather than on
	// what privacy.no_raw actually stored.
	evs, err := st.ListEvents(t.Context(), store.Filter{}, store.WithRaw())
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no events stored; the fixture should have produced one")
	}
	for _, e := range evs {
		if e.Raw != "" {
			t.Errorf("event %q stored raw %q, want empty under privacy.no_raw", e.DedupKey, e.Raw)
		}
	}
}

// TestDefaultRawIsUsageObjectOnly is the end-to-end counterpart: with the
// default config the stored payload is the usage sub-object, never the cwd the
// transcript line carries.
func TestDefaultRawIsUsageObjectOnly(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", offlineConfig(t), "once"); err != nil {
		t.Fatalf("once failed: %v", err)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// WithRaw: this asserts on what was STORED, which the default projection
	// deliberately no longer carries.
	evs, err := st.ListEvents(t.Context(), store.Filter{}, store.WithRaw())
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no events stored; the fixture should have produced one")
	}
	for _, e := range evs {
		if !strings.Contains(e.Raw, `"input_tokens":100`) {
			t.Errorf("stored raw is missing the usage block: %q", e.Raw)
		}
		if strings.Contains(e.Raw, "/home/dev/projects/demo") {
			t.Errorf("stored raw leaked the transcript cwd: %q", e.Raw)
		}
	}
}

// failingAdapter discovers one source and fails every Collect.
type failingAdapter struct{}

func (failingAdapter) ID() string          { return "failing" }
func (failingAdapter) DisplayName() string { return "failing" }
func (failingAdapter) Discover(context.Context, adapter.DiscoverConfig) ([]adapter.Source, error) {
	return []adapter.Source{{Tool: "failing", Path: "failing/src", Label: "failing"}}, nil
}
func (failingAdapter) Collect(context.Context, adapter.Source) (adapter.Observation, error) {
	return adapter.Observation{}, errors.New("source unreadable")
}

// TestOnceFailsFastWhenDaemonHoldsLock: `once` must refuse to run while the
// daemon holds the collection lock (racing it double-counts aggregate deltas)
// and must say so.
func TestOnceFailsFastWhenDaemonHoldsLock(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	stateDir := isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	pidPath := filepath.Join(stateDir, "aiusage", "aiusage.pid")
	release, err := collect.AcquireCollectionLock(pidPath, "fake-daemon-build")
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer release()

	out, err := runCmd(t, "--db", db, "--home", home, "--config", offlineConfig(t), "once")
	if err == nil {
		t.Fatalf("once should fail while the daemon holds the lock; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "already collecting") {
		t.Fatalf("lock error does not mention the collecting daemon: %v", err)
	}
}

// TestOnceExitsNonzeroWhenAllSourcesFail covers the cron contract: a cycle in
// which every source fails must exit nonzero, with the errors still printed.
func TestOnceExitsNonzeroWhenAllSourcesFail(t *testing.T) {
	db := filepath.Join(t.TempDir(), "usage.db")
	isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	prev := onceRegistry
	onceRegistry = func() *adapter.Registry { return adapter.NewRegistry(failingAdapter{}) }
	defer func() { onceRegistry = prev }()

	out, err := runCmd(t, "--db", db, "--home", t.TempDir(), "--config", offlineConfig(t), "once")
	if err == nil {
		t.Fatalf("once should exit nonzero when every source fails; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "every source failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "source unreadable") {
		t.Fatalf("per-source errors not visible in output:\n%s", out)
	}
}

// TestOncePartialFailureExitsZero: one failing adapter plus a working
// claude-code fixture keeps exit 0 while the failure stays visible.
func TestOncePartialFailureExitsZero(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	prev := onceRegistry
	onceRegistry = func() *adapter.Registry { return adapter.NewRegistry(failingAdapter{}, claudecode.New()) }
	defer func() { onceRegistry = prev }()

	out, err := runCmd(t, "--db", db, "--home", home, "--config", offlineConfig(t), "once")
	if err != nil {
		t.Fatalf("partial failure must keep exit 0, got: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "source unreadable") {
		t.Fatalf("partial-failure errors not visible in output:\n%s", out)
	}
}

// TestDoctorReportsAbsentSourceAndEnablement runs doctor against an empty home
// so every adapter discovers nothing. A count of zero would read as "you used
// nothing", so the absent state must be stated in words, and the opt-in tool
// (Copilot, issue #28) must get the checklist that turns its telemetry on.
func TestDoctorReportsAbsentSourceAndEnablement(t *testing.T) {
	home := t.TempDir()
	db := filepath.Join(t.TempDir(), "usage.db")

	// Neutralise ambient env that would hand an adapter a real source.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("COPILOT_OTEL_FILE_EXPORTER_PATH", "")
	t.Setenv("OPENCODE_DATA_DIR", "")

	out, err := runCmd(t, "--db", db, "--home", home, "--config", offlineConfig(t),
		"--no-daemon", "doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, absentStatus) {
		t.Errorf("doctor rendered a zero count instead of %q:\n%s", absentStatus, out)
	}
	for _, want := range []string{
		"export COPILOT_OTEL_ENABLED=true",
		"export COPILOT_OTEL_EXPORTER_TYPE=file",
		`export COPILOT_OTEL_FILE_EXPORTER_PATH="$HOME/.copilot/otel/copilot-otel-$(date +%Y%m%d-%H%M%S).jsonl"`,
		"Do NOT set OTEL_EXPORTER_OTLP_ENDPOINT",
		"Not retroactive",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("enablement checklist missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorWarnsOnLoosePerms loosens the data dir and DB the way pre-#25
// releases created them and expects doctor to flag both.
func TestDoctorWarnsOnLoosePerms(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	db := filepath.Join(dataDir, "usage.db")

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	st.Close()
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatalf("chmod db: %v", err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")

	out, err := runCmd(t, "--db", db, "--home", home, "--config", offlineConfig(t),
		"--no-daemon", "doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "warning: data dir") {
		t.Errorf("doctor did not warn about the data dir:\n%s", out)
	}
	if !strings.Contains(out, "warning: database") {
		t.Errorf("doctor did not warn about the database:\n%s", out)
	}
}

// TestOnceReportsARollupRebuild is the missing half of the daemon's log line
// (finding D): `once` runs the same cycle, rebuilds the same rollup after a
// migration, and used to spend that time silently.
func TestOnceReportsARollupRebuild(t *testing.T) {
	rebuilt := runPrintCycleStats(t, collect.CycleStats{Adapters: 1, Sources: 2, RollupRebuilt: true})
	if !strings.Contains(rebuilt, "rebuilt the derived rollup") {
		t.Errorf("a rebuilt rollup was not reported:\n%s", rebuilt)
	}
	if !strings.Contains(rebuilt, "adapters=1 sources=2") {
		t.Errorf("the cycle counts went missing:\n%s", rebuilt)
	}
	if lines := strings.Count(strings.TrimSpace(rebuilt), "\n"); lines != 1 {
		t.Errorf("want exactly two lines (notice + counts), got:\n%s", rebuilt)
	}

	quiet := runPrintCycleStats(t, collect.CycleStats{Adapters: 1, Sources: 2})
	if strings.Contains(quiet, "rebuilt") {
		t.Errorf("a cycle that rebuilt nothing said it did:\n%s", quiet)
	}
}

// runPrintCycleStats captures what one cycle prints.
func runPrintCycleStats(t *testing.T, s collect.CycleStats) string {
	t.Helper()
	c := &cobra.Command{}
	var out bytes.Buffer
	c.SetOut(&out)
	printCycleStats(c, s)
	return out.String()
}
