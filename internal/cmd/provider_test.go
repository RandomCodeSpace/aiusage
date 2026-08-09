package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// TestSummaryByProvider is the issue #38 CLI wiring: --by provider used to be
// rejected outright ("invalid --by dimension"), so the provider stamped on
// every event since schema v3 could not be reported on at all. The grouped
// table must now carry the dimension as its own column.
func TestSummaryByProvider(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	isolateState(t)

	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once failed: %v", err)
	}

	out, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon",
		"summary", "--by", "provider")
	if err != nil {
		t.Fatalf("summary --by provider failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "provider") {
		t.Errorf("summary --by provider has no provider column:\n%s", out)
	}
	if !strings.Contains(out, model.ProviderAnthropic) {
		t.Errorf("summary --by provider did not bucket the claude-code events under %q:\n%s",
			model.ProviderAnthropic, out)
	}
}

// TestParseByAcceptsProvider pins the flag validator itself, which is the piece
// that rejected the dimension before the store ever saw it.
func TestParseByAcceptsProvider(t *testing.T) {
	dims, err := parseBy("provider,day")
	if err != nil {
		t.Fatalf("parseBy(provider,day): %v", err)
	}
	if len(dims) != 2 || dims[0] != "provider" || dims[1] != "day" {
		t.Fatalf("parseBy(provider,day) = %v, want [provider day]", dims)
	}
}
