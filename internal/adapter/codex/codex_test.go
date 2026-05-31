package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// codexHome returns the default codex home (<home>/.codex) for a user home dir,
// matching production discovery (CODEX_HOME unset => ~/.codex).
func codexHome(home string) string { return filepath.Join(home, ".codex") }

// writeSession writes lines (already JSON-encoded strings) to a .jsonl file.
func writeSession(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var content string
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func collectAll(t *testing.T, cfg adapter.DiscoverConfig) []model.UsageEvent {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var evs []model.UsageEvent
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("Collect(%s): %v", s.Path, err)
		}
		evs = append(evs, obs.Events...)
	}
	return evs
}

// TestLastTokenUsageMapping verifies cached⊆input mapping when info carries a
// per-turn last_token_usage delta.
func TestLastTokenUsageMapping(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "2026", "rollout-abc.jsonl")
	writeSession(t, sess, []string{
		`{"type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		`{"type":"event_msg","timestamp":"2026-05-29T10:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":200,"reasoning_output_tokens":50,"total_tokens":1200}}}}`,
	})

	cfg := adapter.DiscoverConfig{Home: home}
	evs := collectAll(t, cfg)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	e := evs[0]

	if e.Tool != model.ToolCodex {
		t.Errorf("tool = %q", e.Tool)
	}
	if e.Model != "gpt-5-codex" {
		t.Errorf("model carry-forward failed: %q", e.Model)
	}
	// Input = input - cached = 1000 - 400 = 600
	if e.InputTokens != 600 {
		t.Errorf("input = %d, want 600", e.InputTokens)
	}
	if e.CacheReadTokens != 400 {
		t.Errorf("cacheRead = %d, want 400", e.CacheReadTokens)
	}
	if e.CacheCreationTokens != 0 {
		t.Errorf("cacheCreation = %d, want 0", e.CacheCreationTokens)
	}
	if e.OutputTokens != 200 {
		t.Errorf("output = %d, want 200", e.OutputTokens)
	}
	if e.ReasoningTokens != 50 {
		t.Errorf("reasoning = %d, want 50", e.ReasoningTokens)
	}
	if e.TotalTokens != 1200 {
		t.Errorf("total = %d, want 1200", e.TotalTokens)
	}
	// Component-sum invariant: (input-cached)+output+0+cached == raw input+output.
	// raw input 1000 + output 200 = 1200.
	if got := e.InputTokens + e.OutputTokens + e.CacheCreationTokens + e.CacheReadTokens; got != 1200 {
		t.Errorf("component sum = %d, want 1200 (raw input+output)", got)
	}
	if e.Project != "" {
		t.Errorf("project = %q, want empty", e.Project)
	}
	// session = rel to sessions dir, ext stripped, /-joined.
	if e.SessionID != "2026/rollout-abc" {
		t.Errorf("session = %q, want 2026/rollout-abc", e.SessionID)
	}
}

// TestCumulativeDelta verifies saturating per-field deltas from total_token_usage
// when last_token_usage is absent, with carry-forward of previous totals.
func TestCumulativeDelta(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "cum.jsonl")
	writeSession(t, sess, []string{
		`{"type":"turn_context","payload":{"model":"gpt-5"}}`,
		// First cumulative snapshot: no previous -> taken as-is.
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":100,"total_tokens":1100}}}}`,
		// Second cumulative snapshot: delta vs first.
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1500,"cached_input_tokens":300,"output_tokens":250,"total_tokens":1750}}}}`,
	})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evs))
	}

	// Event 1: raw input 1000, cached 200 -> input 800, cacheRead 200, output 100, total 1100.
	e1 := evs[0]
	if e1.InputTokens != 800 || e1.CacheReadTokens != 200 || e1.OutputTokens != 100 || e1.TotalTokens != 1100 {
		t.Errorf("event1 = %+v", e1)
	}

	// Event 2: delta input 500, cached 100, output 150, total 650.
	// mapped: input = 500-100 = 400, cacheRead 100, output 150, total 650.
	e2 := evs[1]
	if e2.InputTokens != 400 {
		t.Errorf("event2 input = %d, want 400", e2.InputTokens)
	}
	if e2.CacheReadTokens != 100 {
		t.Errorf("event2 cacheRead = %d, want 100", e2.CacheReadTokens)
	}
	if e2.OutputTokens != 150 {
		t.Errorf("event2 output = %d, want 150", e2.OutputTokens)
	}
	if e2.TotalTokens != 650 {
		t.Errorf("event2 total = %d, want 650", e2.TotalTokens)
	}
}

// TestSaturatingReset verifies per-field saturating subtraction on cumulative
// totals: fields that drop contribute 0 (never negative), while fields that grow
// produce a real delta. A field-wise full reset (all fields lower) yields an
// all-zero delta and is therefore skipped.
func TestSaturatingReset(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "reset.jsonl")
	writeSession(t, sess, []string{
		`{"type":"turn_context","payload":{"model":"gpt-5"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":2000,"output_tokens":500,"total_tokens":2500}}}}`,
		// Partial reset: input drops (delta floors at 0) but output grows by 100.
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":300,"output_tokens":600,"total_tokens":900}}}}`,
		// Full reset: every field lower than prev cumulative -> all-zero -> skipped.
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}`,
	})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 2 {
		t.Fatalf("expected 2 events (full reset skipped), got %d", len(evs))
	}
	// Event 2: input satSub(300,2000)=0, output satSub(600,500)=100, total satSub(900,2500)=0
	// -> total falls back to raw input(300)+output(600)=900 (since satSub total is 0).
	e2 := evs[1]
	if e2.InputTokens != 0 {
		t.Errorf("event2 input delta = %d, want 0 (floored)", e2.InputTokens)
	}
	if e2.OutputTokens != 100 {
		t.Errorf("event2 output delta = %d, want 100", e2.OutputTokens)
	}
	// raw total delta floored to 0 -> fallback to delta-input(0)+delta-output(100) = 100.
	if e2.TotalTokens != 100 {
		t.Errorf("event2 total = %d, want 100 (fallback to in+out delta)", e2.TotalTokens)
	}
}

// TestStringNumericsAndWhitespaceType verifies string-encoded numbers and a
// whitespace-padded "type" field are tolerated.
func TestStringNumericsAndWhitespaceType(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "str.jsonl")
	writeSession(t, sess, []string{
		`{"type":"turn_context","payload":{"model":"gpt-5"}}`,
		`{"type":"  event_msg  ","payload":{"type":" token_count ","info":{"last_token_usage":{"input_tokens":"500","cached_input_tokens":" 100 ","output_tokens":"50","total_tokens":"550"}}}}`,
	})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	e := evs[0]
	if e.InputTokens != 400 || e.CacheReadTokens != 100 || e.OutputTokens != 50 || e.TotalTokens != 550 {
		t.Errorf("event = %+v, want input400/cacheRead100/output50/total550", e)
	}
}

// TestTotalFallback verifies total falls back to input+output when total_tokens
// is missing or zero.
func TestTotalFallback(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "fb.jsonl")
	writeSession(t, sess, []string{
		`{"type":"event_msg","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":700,"cached_input_tokens":100,"output_tokens":300}}}}`,
	})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	// total = input+output = 700+300 = 1000
	if evs[0].TotalTokens != 1000 {
		t.Errorf("total fallback = %d, want 1000", evs[0].TotalTokens)
	}
	if evs[0].Model != "gpt-5" {
		t.Errorf("model from payload = %q", evs[0].Model)
	}
}

// TestModelFallback verifies the default model is used when none is present.
func TestModelFallback(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "nomodel.jsonl")
	writeSession(t, sess, []string{
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}`,
	})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].Model != defaultModel {
		t.Errorf("model = %q, want %q", evs[0].Model, defaultModel)
	}
}

// TestSkipAllZeroAndMalformed verifies all-zero events are skipped and a
// malformed line does not abort the file (best-effort, never panics).
func TestSkipAllZeroAndMalformed(t *testing.T) {
	home := t.TempDir()
	sess := filepath.Join(codexHome(home), "sessions", "mix.jsonl")
	writeSession(t, sess, []string{
		`{"type":"event_msg","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}}}`,
		`{ this is not valid json`,
	})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	// Only the one non-zero event before the malformed line survives.
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].TotalTokens != 150 {
		t.Errorf("total = %d, want 150", evs[0].TotalTokens)
	}
}

// TestArchivedSessions verifies archived_sessions is also scanned and that the
// live + archived copies of the same record collapse via the stable dedup key.
func TestArchivedSessions(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"event_msg","timestamp":"2026-05-29T10:00:00Z","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":250,"total_tokens":750}}}}`

	// Same record under sessions and archived_sessions (Codex moves files).
	writeSession(t, filepath.Join(codexHome(home), "sessions", "live.jsonl"), []string{line})
	writeSession(t, filepath.Join(codexHome(home), "archived_sessions", "live.jsonl"), []string{line})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 2 {
		t.Fatalf("expected 2 raw events (pre-dedup), got %d", len(evs))
	}

	// Dedup key excludes session id, so both collapse to one key.
	if evs[0].DedupKey != evs[1].DedupKey {
		t.Errorf("dedup keys differ across live/archived: %q vs %q", evs[0].DedupKey, evs[1].DedupKey)
	}
}

// TestDedupStableAcrossRereadAndExcludesSession verifies the dedup key is stable
// across re-reads and does not depend on the session id (branch-copied
// histories count once).
func TestDedupStableAcrossRereadAndExcludesSession(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"event_msg","timestamp":"2026-05-29T12:34:56Z","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":800,"cached_input_tokens":200,"output_tokens":400,"reasoning_output_tokens":40,"total_tokens":1200}}}}`

	// Two distinct session files (different session ids) with identical record.
	writeSession(t, filepath.Join(codexHome(home), "sessions", "alpha.jsonl"), []string{line})
	writeSession(t, filepath.Join(codexHome(home), "sessions", "branch", "beta.jsonl"), []string{line})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evs))
	}
	if evs[0].SessionID == evs[1].SessionID {
		t.Fatalf("sessions should differ: both %q", evs[0].SessionID)
	}
	if evs[0].DedupKey != evs[1].DedupKey {
		t.Errorf("dedup key should be session-independent: %q vs %q", evs[0].DedupKey, evs[1].DedupKey)
	}

	// Re-read the same files: keys must be identical (stable).
	firstKey := evs[0].DedupKey
	evs2 := collectAll(t, adapter.DiscoverConfig{Home: home})
	for _, e := range evs2 {
		if e.DedupKey != firstKey {
			t.Errorf("dedup key not stable across re-read: %q != %q", e.DedupKey, firstKey)
		}
	}
}

// TestCODEXHomeEnvAndNoSessionsDir verifies CODEX_HOME override and the fallback
// to scanning <home> directly when no sessions/ dir exists.
func TestCODEXHomeEnvAndNoSessionsDir(t *testing.T) {
	home := t.TempDir()
	// No sessions/ dir; file directly under home.
	writeSession(t, filepath.Join(home, "direct.jsonl"), []string{
		`{"type":"event_msg","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}`,
	})
	t.Setenv("CODEX_HOME", home)

	// cfg.Home points elsewhere; env must win.
	evs := collectAll(t, adapter.DiscoverConfig{Home: t.TempDir()})
	if len(evs) != 1 {
		t.Fatalf("expected 1 event via CODEX_HOME, got %d", len(evs))
	}
	// session relative to <home> (the scan root) = "direct".
	if evs[0].SessionID != "direct" {
		t.Errorf("session = %q, want direct", evs[0].SessionID)
	}
}

// TestTimestampFallbackToMTime verifies that a line without a timestamp uses the
// file mtime as the event time.
func TestTimestampFallbackToMTime(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(codexHome(home), "sessions", "nots.jsonl")
	writeSession(t, path, []string{
		`{"type":"event_msg","payload":{"type":"token_count","model":"gpt-5","info":{"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}`,
	})

	evs := collectAll(t, adapter.DiscoverConfig{Home: home})
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].EventTime.IsZero() {
		t.Error("event time should fall back to file mtime, got zero")
	}
}
