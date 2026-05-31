package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/store"
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

	// Neutralise any ambient config/env that could redirect paths.
	t.Setenv("AIUSAGE_DB", "")
	t.Setenv("AIUSAGE_HOME", "")
	t.Setenv("AIUSAGE_INTERVAL", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	out, err := runCmd(t, "--db", db, "--home", home, "--config", filepath.Join(t.TempDir(), "absent.json"), "once")
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
	cfg := filepath.Join(t.TempDir(), "absent.json")

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
