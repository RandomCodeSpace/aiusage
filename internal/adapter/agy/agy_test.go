package agy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aiusage/internal/adapter"
	"aiusage/internal/model"
)

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

// TestDiscoverNoTokenFiles is the headline case: an Antigravity dir holding
// only content-only blobs (no token fields) yields zero sources and no error —
// the current real (unauthenticated) state.
func TestDiscoverNoTokenFiles(t *testing.T) {
	dir := t.TempDir()
	// A conversation-style blob with no token usage anywhere.
	writeFile(t, dir, "conversation.json", `{"id":"c1","messages":[{"role":"user","content":"hi"}]}`)
	// A JSONL log with content only.
	writeFile(t, dir, "events.jsonl", `{"id":"e1","type":"agy","text":"hello"}`+"\n")

	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolAgy: dir}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources for token-free Antigravity dir, got %d: %+v", len(srcs), srcs)
	}
}

// TestDiscoverEmptyDir verifies an empty Antigravity dir yields no sources.
func TestDiscoverEmptyDir(t *testing.T) {
	dir := t.TempDir()
	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolAgy: dir}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources for empty dir, got %d", len(srcs))
	}
}

// TestDiscoverMissingRoots verifies that a home with none of the candidate dirs
// present yields no sources and no error.
func TestDiscoverMissingRoots(t *testing.T) {
	home := t.TempDir() // no .gemini/antigravity-cli, .antigravitycli, .cache/antigravity
	a := New()
	cfg := adapter.DiscoverConfig{Home: home}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources, got %d", len(srcs))
	}
}

// TestParseGeminiShapedUsage verifies that IF Antigravity ever emits a Gemini-
// shaped usage file, it is discovered and parsed with tool="agy". This exercises
// the forward-looking parser without asserting the live (token-free) state.
func TestParseGeminiShapedUsage(t *testing.T) {
	dir := t.TempDir()
	// Two cumulative records for the same id -> one max snapshot.
	content := `{"id":"t1","model":"antigravity","type":"gemini","tokens":{"input":50,"output":10,"thoughts":2}}
{"id":"t1","model":"antigravity","type":"gemini","tokens":{"input":150,"output":40,"thoughts":8}}
`
	writeFile(t, dir, "usage.jsonl", content)

	a := New()
	cfg := adapter.DiscoverConfig{Overrides: map[string]string{model.ToolAgy: dir}}
	srcs, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("want 1 source for usage-bearing file, got %d", len(srcs))
	}

	obs, _ := a.Collect(context.Background(), srcs[0])
	if len(obs.Snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(obs.Snapshots))
	}
	s := obs.Snapshots[0]
	if s.Tool != model.ToolAgy {
		t.Errorf("Tool = %q, want %q", s.Tool, model.ToolAgy)
	}
	if s.InputTokens != 150 || s.OutputTokens != 40 || s.ReasoningTokens != 8 {
		t.Errorf("max snapshot wrong: in=%d out=%d thoughts=%d", s.InputTokens, s.OutputTokens, s.ReasoningTokens)
	}
	// Total = input+output+thoughts = 150+40+8 = 198.
	if want := int64(198); s.TotalTokens != want {
		t.Errorf("TotalTokens = %d, want %d", s.TotalTokens, want)
	}
	if want := s.SourcePath + "|t1"; s.Key != want {
		t.Errorf("Key = %q, want %q", s.Key, want)
	}
}
