package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSummaryBreakdownEmitsComponentColumns is the issue #3 regression: the
// --breakdown flag was registered and then never read, so it rendered exactly
// the default table. It must now split the combined Cache column into the
// stored components, and the default must stay untouched.
func TestSummaryBreakdownEmitsComponentColumns(t *testing.T) {
	home := writeClaudeFixture(t)
	db := filepath.Join(t.TempDir(), "usage.db")
	cfg := offlineConfig(t)
	isolateState(t)

	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if _, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "once"); err != nil {
		t.Fatalf("once failed: %v", err)
	}

	plain, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon",
		"summary", "--by", "tool")
	if err != nil {
		t.Fatalf("summary failed: %v\noutput:\n%s", err, plain)
	}
	broken, err := runCmd(t, "--db", db, "--home", home, "--config", cfg, "--no-daemon",
		"summary", "--by", "tool", "--breakdown")
	if err != nil {
		t.Fatalf("summary --breakdown failed: %v\noutput:\n%s", err, broken)
	}

	if plain == broken {
		t.Fatalf("--breakdown rendered the default table:\n%s", broken)
	}
	for _, want := range []string{"Reasoning", "CacheW", "CacheR"} {
		if !strings.Contains(broken, want) {
			t.Errorf("--breakdown output missing the %q column:\n%s", want, broken)
		}
		if strings.Contains(plain, want) {
			t.Errorf("default output leaked the %q column:\n%s", want, plain)
		}
	}
}
