package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RandomCodeSpace/aiusage/internal/collect"
)

// TestServeDocumentsTheHostAllowlist: the flag is the only way a proxied
// deployment is reachable at all, and the reason it is needed (a proxy passes
// the public Host through) is not something an operator should have to infer
// from a 421. Both the flag help and the command's own text have to say it.
func TestServeDocumentsTheHostAllowlist(t *testing.T) {
	c := newServeCmd()
	flag := c.Flags().Lookup("allowed-hosts")
	if flag == nil {
		t.Fatal("serve has no --allowed-hosts flag")
	}
	if flag.DefValue != "[]" {
		t.Errorf("--allowed-hosts default = %q, want an empty list; a public name must never be a default", flag.DefValue)
	}
	for _, want := range []string{"localhost", "127.0.0.1", "::1", "reverse proxy", "aiusage.randomcodespace.dev"} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("--allowed-hosts help does not mention %q:\n%s", want, flag.Usage)
		}
	}
	for _, want := range []string{"421", "Caddy", "--allowed-hosts"} {
		if !strings.Contains(c.Long, want) {
			t.Errorf("serve help does not mention %q:\n%s", want, c.Long)
		}
	}
	// The domains are documentation, not configuration: a build that shipped
	// them as defaults would answer to them out of the box.
	if strings.Contains(flag.DefValue, "randomcodespace") {
		t.Error("a deployment domain is hardcoded as a flag default")
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
