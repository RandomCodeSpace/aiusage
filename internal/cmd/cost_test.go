package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pricedFixture is a claude-code transcript line whose model is in the embedded
// price snapshot, with round token counts so the resulting cost is exact and
// hand-checkable: 1,000,000 input tokens at $3/M = $3.00.
const pricedFixture = `{"timestamp":"2026-05-29T12:00:00Z","cwd":"/home/dev/projects/demo","sessionId":"sess-1","requestId":"req-1","message":{"id":"msg-1","model":"claude-sonnet-4-6","usage":{"input_tokens":1000000,"output_tokens":0}}}`

// unknownModelFixture is the same shape with a model no price table knows, so
// the row lands unpriced and can never be valued.
const unknownModelFixture = `{"timestamp":"2026-05-29T12:05:00Z","cwd":"/home/dev/projects/demo","sessionId":"sess-2","requestId":"req-2","message":{"id":"msg-2","model":"totally-made-up-model","usage":{"input_tokens":1000,"output_tokens":10}}}`

// writeTranscripts lays down one claude-code transcript per line given and
// returns the home dir to point --home at.
func writeTranscripts(t *testing.T, lines ...string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	for i, line := range lines {
		file := filepath.Join(dir, "sess-"+string(rune('1'+i))+".jsonl")
		if err := os.WriteFile(file, []byte(line+"\n"), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	return home
}

// TestSummaryStampsAndShowsExactCost is the end-to-end proof that a collected
// event is priced at ingest from the embedded snapshot and rendered as an exact
// dollar amount — no tilde, because nothing needed display pricing.
func TestSummaryStampsAndShowsExactCost(t *testing.T) {
	home := writeTranscripts(t, pricedFixture)
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once: %v", err)
	}
	out, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon",
		"summary", "--by", "tool")
	if err != nil {
		t.Fatalf("summary: %v\n%s", err, out)
	}

	if !strings.Contains(out, "Cost") {
		t.Fatalf("summary has no Cost column:\n%s", out)
	}
	if !strings.Contains(out, "$3.00") {
		t.Errorf("summary missing the exact $3.00 cost:\n%s", out)
	}
	if strings.Contains(out, "~") {
		t.Errorf("a fully stamped summary must not be marked approximate:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("summary rendered a $0.00 cost:\n%s", out)
	}
}

// TestSummaryMarksUnpriceableRowsApproximate checks the other half of the
// contract: a bucket still holding rows no table can value is a floor, so the
// rendered total wears the tilde instead of claiming precision.
func TestSummaryMarksUnpriceableRowsApproximate(t *testing.T) {
	home := writeTranscripts(t, pricedFixture, unknownModelFixture)
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once: %v", err)
	}
	out, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon",
		"summary", "--by", "tool")
	if err != nil {
		t.Fatalf("summary: %v\n%s", err, out)
	}
	if !strings.Contains(out, "~$3.00") {
		t.Errorf("mixed summary missing the approximate marker:\n%s", out)
	}
	if !strings.Contains(out, "priced at display time") {
		t.Errorf("approximate summary missing its explanatory note:\n%s", out)
	}
}

// TestExportCarriesCostColumns checks the CSV export gains the stored pricing
// columns, with the exact micro value and its decimal twin.
func TestExportCarriesCostColumns(t *testing.T) {
	home := writeTranscripts(t, pricedFixture)
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	isolateState(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once: %v", err)
	}
	out, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon",
		"export", "--format", "csv")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	for _, want := range []string{
		"provider", "service_tier", "cost_micro_usd", "cost_usd", "price_source",
		"anthropic", "3000000", "3.000000", "embedded-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("csv export missing %q:\n%s", want, out)
		}
	}
}
