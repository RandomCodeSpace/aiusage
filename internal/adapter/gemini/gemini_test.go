package gemini

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// writeFile writes content to dir/name and returns the full path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// collectAll discovers and collects every source for the adapter under root,
// returning the flattened snapshots.
func collectAll(t *testing.T, root string) []model.AggregateSnapshot {
	t.Helper()
	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{toolID: root}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var snaps []model.AggregateSnapshot
	for _, s := range srcs {
		obs, _ := a.Collect(context.Background(), s)
		snaps = append(snaps, obs.Snapshots...)
	}
	return snaps
}

// TestCumulativePerID verifies that two cumulative records for the SAME id are
// collapsed into one snapshot holding the max (final) totals, and that Total
// derives as input+output+thoughts when tokens.total is absent.
func TestCumulativePerID(t *testing.T) {
	dir := t.TempDir()
	// Two JSONL records for id "t1"; the second is the larger cumulative
	// snapshot. No tokens.total field -> Total must derive = input+output+thoughts.
	content := `{"id":"t1","model":"gemini-2.0","type":"gemini","timestamp":"2026-05-29T10:00:00Z","tokens":{"input":100,"output":20,"cached":0,"thoughts":5,"tool":0}}
{"id":"t1","model":"gemini-2.0","type":"gemini","timestamp":"2026-05-29T10:00:05Z","tokens":{"input":300,"output":80,"cached":0,"thoughts":15,"tool":0}}
`
	writeFile(t, dir, "session.jsonl", content)

	snaps := collectAll(t, dir)
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot, got %d: %+v", len(snaps), snaps)
	}
	s := snaps[0]

	if s.Tool != toolID {
		t.Errorf("Tool = %q, want %q", s.Tool, toolID)
	}
	if s.Model != "gemini-2.0" {
		t.Errorf("Model = %q, want gemini-2.0", s.Model)
	}
	if s.Project != metaProject {
		t.Errorf("Project = %q, want %q", s.Project, metaProject)
	}
	// Max snapshot: input=300, output=80, thoughts=15.
	if s.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", s.InputTokens)
	}
	if s.OutputTokens != 80 {
		t.Errorf("OutputTokens = %d, want 80", s.OutputTokens)
	}
	if s.ReasoningTokens != 15 {
		t.Errorf("ReasoningTokens = %d, want 15", s.ReasoningTokens)
	}
	// Total = input + output + thoughts = 300 + 80 + 15 = 395 (no tokens.total).
	if want := int64(395); s.TotalTokens != want {
		t.Errorf("TotalTokens = %d, want %d", s.TotalTokens, want)
	}
	// Key = sourcePath + "|" + id.
	if want := s.SourcePath + "|t1"; s.Key != want {
		t.Errorf("Key = %q, want %q", s.Key, want)
	}
	// Timestamp from the (chosen) record drives ObservedTime.
	if got := s.ObservedTime.Format("2006-01-02T15:04:05Z"); got != "2026-05-29T10:00:05Z" {
		t.Errorf("ObservedTime = %q, want 2026-05-29T10:00:05Z", got)
	}
}

// TestReportedTotalAndCachedOverlap verifies that a provider tokens.total is
// used verbatim, cached maps to CacheRead (excluded from total), and Input is
// (input+tool) minus the cached overlap.
func TestReportedTotalAndCachedOverlap(t *testing.T) {
	dir := t.TempDir()
	// input=200, tool=50, cached=40 -> Input = 200+50-40 = 210.
	// total reported = 260 (input+output+thoughts = 200+30+10 = 240; provider
	// total is authoritative and used verbatim).
	content := `{"id":"x","model":"m","type":"gemini","tokens":{"input":200,"output":30,"cached":40,"thoughts":10,"tool":50,"total":260}}`
	writeFile(t, dir, "single.json", content)

	snaps := collectAll(t, dir)
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(snaps))
	}
	s := snaps[0]
	if s.InputTokens != 210 {
		t.Errorf("InputTokens = %d, want 210 (200+50-40)", s.InputTokens)
	}
	if s.CacheReadTokens != 40 {
		t.Errorf("CacheReadTokens = %d, want 40", s.CacheReadTokens)
	}
	if s.CacheCreationTokens != 0 {
		t.Errorf("CacheCreationTokens = %d, want 0", s.CacheCreationTokens)
	}
	if s.TotalTokens != 260 {
		t.Errorf("TotalTokens = %d, want 260 (provider total, cached excluded)", s.TotalTokens)
	}
}

// TestSetWrapperAndMalformed verifies a $set envelope is unwrapped and that a
// malformed JSONL line is skipped without aborting the file.
func TestSetWrapperAndMalformed(t *testing.T) {
	dir := t.TempDir()
	content := `{"$set":{"id":"w","model":"m","tokens":{"input":10,"output":5,"thoughts":0}}}
this is not json
{"id":"v","model":"m","tokens":{"input":1,"output":1}}
`
	writeFile(t, dir, "mixed.jsonl", content)

	snaps := collectAll(t, dir)
	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots (w, v), got %d: %+v", len(snaps), snaps)
	}
	byID := map[string]model.AggregateSnapshot{}
	for _, s := range snaps {
		// Key suffix is the id.
		byID[s.Key[len(s.SourcePath)+1:]] = s
	}
	if w, ok := byID["w"]; !ok || w.InputTokens != 10 || w.TotalTokens != 15 {
		t.Errorf("unwrapped $set record wrong: %+v", w)
	}
	if _, ok := byID["v"]; !ok {
		t.Errorf("record after malformed line was lost")
	}
}

// TestAllZeroDropped verifies all-zero records are dropped.
func TestAllZeroDropped(t *testing.T) {
	dir := t.TempDir()
	content := `{"id":"z","model":"m","tokens":{"input":0,"output":0,"cached":0,"thoughts":0,"tool":0,"total":0}}`
	writeFile(t, dir, "zero.json", content)

	snaps := collectAll(t, dir)
	if len(snaps) != 0 {
		t.Fatalf("want 0 snapshots for all-zero record, got %d", len(snaps))
	}
}

// TestRealSessionShapeNoSpuriousSkip reproduces an actual Gemini-CLI chat file:
// a session-header line, a user turn, two {"$set":{...}} mutation entries, and
// one usage-bearing gemini turn. Only the gemini turn carries tokens; the other
// four are legitimate non-usage records and MUST be dropped silently (no
// "skipped/malformed" error), since they are neither malformed nor real usage.
func TestRealSessionShapeNoSpuriousSkip(t *testing.T) {
	dir := t.TempDir()
	content := `{"sessionId":"s1","projectHash":"abc","startTime":"2026-05-02T08:59:00Z","lastUpdated":"2026-05-02T08:59:50Z","kind":"session"}
{"id":"u1","timestamp":"2026-05-02T08:59:01Z","type":"user","content":"hi"}
{"$set":{"lastUpdated":"2026-05-02T08:59:32.017Z"}}
{"id":"g1","timestamp":"2026-05-02T08:59:32Z","type":"gemini","content":"pong","thoughts":[],"tokens":{"input":7566,"output":1,"cached":0,"thoughts":108,"tool":0,"total":7675},"model":"gemini-3-flash-preview"}
{"$set":{"lastUpdated":"2026-05-02T08:59:49.651Z"}}
`
	root := writeFile(t, filepath.Join(dir, "chats"), "session-real.jsonl", content)
	_ = root

	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{toolID: dir}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var snaps []model.AggregateSnapshot
	for _, s := range srcs {
		obs, cerr := a.Collect(context.Background(), s)
		if cerr != nil {
			t.Fatalf("collect returned a spurious error for benign non-usage records: %v", cerr)
		}
		snaps = append(snaps, obs.Snapshots...)
	}
	if len(snaps) != 1 {
		t.Fatalf("want exactly 1 usage snapshot (the gemini turn), got %d: %+v", len(snaps), snaps)
	}
	if snaps[0].TotalTokens != 7675 {
		t.Errorf("snapshot total = %d, want 7675 (cached excluded)", snaps[0].TotalTokens)
	}
}

// TestDiscoverMissingDir verifies a missing root yields no sources, no error.
func TestDiscoverMissingDir(t *testing.T) {
	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{toolID: filepath.Join(t.TempDir(), "nope")}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources for missing dir, got %d", len(srcs))
	}
}
