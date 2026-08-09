package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// writeFixture creates <root>/projects/<seg>/<session>.jsonl with the given lines.
func writeFixture(t *testing.T, root, seg, session string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, "projects", seg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func collect(t *testing.T, root string) []model.UsageEvent {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{
		Overrides: map[string]string{model.ToolClaudeCode: root},
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("want 1 source, got %d", len(srcs))
	}
	obs, err := a.Collect(context.Background(), srcs[0])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return obs.Events
}

func byMessageID(evs []model.UsageEvent) map[string]model.UsageEvent {
	m := make(map[string]model.UsageEvent, len(evs))
	for _, e := range evs {
		m[e.MessageID] = e
	}
	return m
}

func TestCollectParsesMapsAndDedupes(t *testing.T) {
	root := t.TempDir()
	lines := []string{
		// 1. normal assistant usage (cache additive) — kept.
		`{"type":"assistant","timestamp":"2026-05-10T13:14:19.329Z","cwd":"/home/dev/projects/foo","sessionId":"sess-1","requestId":"req-1","message":{"id":"msg-1","model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}}`,
		// 2. user line, no usage — skipped by the fast marker.
		`{"type":"user","timestamp":"2026-05-10T13:14:20Z","message":{"role":"user","content":"hi"}}`,
		// 3. <synthetic> model with usage — skipped.
		`{"type":"assistant","timestamp":"2026-05-10T13:14:21Z","message":{"id":"msg-x","model":"<synthetic>","usage":{"input_tokens":9,"output_tokens":9}}}`,
		// 4. sidechain replay of msg-1 (diff requestId, fewer tokens) — deduped; non-sidechain wins.
		`{"type":"assistant","timestamp":"2026-05-10T13:14:22Z","sessionId":"sess-1","requestId":"req-2","isSidechain":true,"message":{"id":"msg-1","model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":1}}}`,
		// 5. fast speed -> model gets -fast suffix.
		`{"type":"assistant","timestamp":"2026-05-10T13:14:23Z","cwd":"/home/dev/projects/foo","sessionId":"sess-1","requestId":"req-3","message":{"id":"msg-2","model":"claude-sonnet-4-6","usage":{"input_tokens":20,"output_tokens":10,"speed":"fast"}}}`,
		// 6. null in a guarded key (cache_read_input_tokens) -> skipped.
		`{"type":"assistant","timestamp":"2026-05-10T13:14:24Z","requestId":"req-4","message":{"id":"msg-3","model":"claude-opus-4-7","usage":{"input_tokens":5,"output_tokens":5,"cache_read_input_tokens":null}}}`,
	}
	writeFixture(t, root, "home-dev-projects-foo", "sess-1", lines)

	evs := collect(t, root)
	if len(evs) != 2 {
		t.Fatalf("want 2 events (msg-1 deduped, msg-2), got %d: %+v", len(evs), evs)
	}
	m := byMessageID(evs)

	e1, ok := m["msg-1"]
	if !ok {
		t.Fatal("msg-1 missing")
	}
	if e1.InputTokens != 100 || e1.OutputTokens != 50 || e1.CacheCreationTokens != 10 || e1.CacheReadTokens != 5 {
		t.Errorf("msg-1 token mapping wrong: %+v", e1)
	}
	if e1.TotalTokens != 165 { // cache additive: 100+50+10+5
		t.Errorf("msg-1 total = %d, want 165 (cache additive)", e1.TotalTokens)
	}
	if e1.Model != "claude-opus-4-7" {
		t.Errorf("msg-1 model = %q", e1.Model)
	}
	if e1.Project != "/home/dev/projects/foo" {
		t.Errorf("msg-1 project = %q, want cwd", e1.Project)
	}
	if e1.SessionID != "sess-1" {
		t.Errorf("msg-1 session = %q", e1.SessionID)
	}
	if e1.DedupKey != "claude-code|msg-1" {
		t.Errorf("msg-1 dedup key = %q, want claude-code|msg-1", e1.DedupKey)
	}

	e2, ok := m["msg-2"]
	if !ok {
		t.Fatal("msg-2 missing")
	}
	if e2.Model != "claude-sonnet-4-6-fast" {
		t.Errorf("msg-2 model = %q, want -fast suffix", e2.Model)
	}
	if e2.TotalTokens != 30 {
		t.Errorf("msg-2 total = %d, want 30", e2.TotalTokens)
	}

	if _, bad := m["msg-x"]; bad {
		t.Error("synthetic-model event should have been dropped")
	}
	if _, bad := m["msg-3"]; bad {
		t.Error("null-guarded-key event should have been dropped")
	}
}

func TestDedupKeyStableAcrossReads(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"assistant","timestamp":"2026-05-10T13:14:19Z","cwd":"/p","sessionId":"s","requestId":"r","message":{"id":"m-stable","model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":2}}}`
	writeFixture(t, root, "p", "s", []string{line})

	first := collect(t, root)
	second := collect(t, root)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want 1 event each read, got %d and %d", len(first), len(second))
	}
	if first[0].DedupKey != second[0].DedupKey {
		t.Errorf("dedup key not stable across reads: %q vs %q", first[0].DedupKey, second[0].DedupKey)
	}
}

func TestNoMessageIDKeptWithHashKey(t *testing.T) {
	root := t.TempDir()
	// assistant usage line WITHOUT message.id -> never deduped, hashed key.
	line := `{"type":"assistant","timestamp":"2026-05-10T13:14:19Z","sessionId":"s","message":{"model":"claude-opus-4-7","usage":{"input_tokens":7,"output_tokens":3}}}`
	writeFixture(t, root, "p", "noid", []string{line})

	evs := collect(t, root)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if !strings.HasPrefix(evs[0].DedupKey, "claude-code|") || !strings.Contains(evs[0].DedupKey, ".jsonl|") {
		t.Errorf("no-id event should use hashed path key, got %q", evs[0].DedupKey)
	}
}

func TestDiscoverSkipsRootWithoutProjects(t *testing.T) {
	root := t.TempDir() // no projects/ dir
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{
		Overrides: map[string]string{model.ToolClaudeCode: root},
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources (no projects/ dir), got %d", len(srcs))
	}
}

// TestDiscoverResolvesSymlinkedRoot: the no-message-id dedup fallback embeds
// transcript paths, so a symlinked config root must resolve to its target or a
// re-point would mint new keys for those lines.
func TestDiscoverResolvesSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("eval real root: %v", err)
	}

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{
		Overrides: map[string]string{model.ToolClaudeCode: link},
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("want 1 source, got %d", len(srcs))
	}
	if srcs[0].Path != resolved {
		t.Errorf("Path = %q, want resolved %q", srcs[0].Path, resolved)
	}
}

// benchLine is a representative assistant usage line for the parse hot path.
var benchLine = []byte(`{"type":"assistant","timestamp":"2026-05-10T13:14:19.329Z","cwd":"/home/dev/projects/foo","sessionId":"sess-1","requestId":"req-1","isSidechain":false,"version":"2.0.1","costUSD":0.42,"message":{"id":"msg-1","model":"claude-opus-4-7","usage":{"input_tokens":1200,"output_tokens":350,"cache_creation_input_tokens":210,"cache_read_input_tokens":10500}}}`)

func BenchmarkParseLine(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := parseLine(benchLine, "/p/f.jsonl", "seg", "sess"); !ok {
			b.Fatal("parse failed")
		}
	}
}

// TestIncrementalManifestGate: an unchanged projects tree is skipped without
// parsing; any file change re-parses the whole root (the deduper spans all
// files) and refreshes the manifest checkpoint.
func TestIncrementalManifestGate(t *testing.T) {
	root := t.TempDir()
	trans := filepath.Join(root, "projects", "proj", "sess.jsonl")
	if err := os.MkdirAll(filepath.Dir(trans), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	line1 := `{"timestamp":"2026-05-29T10:00:00Z","sessionId":"s","requestId":"r1","message":{"id":"m1","model":"claude-opus","usage":{"input_tokens":400,"output_tokens":600}}}` + "\n"
	if err := os.WriteFile(trans, []byte(line1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := Adapter{}
	src := adapter.Source{Tool: model.ToolClaudeCode, Class: model.EventLevel, Path: root}

	obs1, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(obs1.Events) != 1 || obs1.Checkpoint == nil {
		t.Fatalf("full read: err=%v events=%d cp=%v", err, len(obs1.Events), obs1.Checkpoint)
	}

	obs2, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil || len(obs2.Events) != 0 || obs2.Checkpoint != nil {
		t.Fatalf("unchanged skip: err=%v events=%d cp=%+v", err, len(obs2.Events), obs2.Checkpoint)
	}

	// Append: the whole root re-parses (both lines emitted; the store dedups).
	f, err := os.OpenFile(trans, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	line2 := `{"timestamp":"2026-05-29T10:01:00Z","sessionId":"s","requestId":"r2","message":{"id":"m2","model":"claude-opus","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if _, err := f.WriteString(line2); err != nil {
		t.Fatalf("write append: %v", err)
	}
	f.Close()

	obs3, err := a.CollectIncremental(context.Background(), src, obs1.Checkpoint)
	if err != nil || len(obs3.Events) != 2 || obs3.Checkpoint == nil {
		t.Fatalf("changed reparse: err=%v events=%d cp=%v", err, len(obs3.Events), obs3.Checkpoint)
	}

	// A NEW file also breaks the gate (additions must not be missed).
	trans2 := filepath.Join(root, "projects", "proj2", "other.jsonl")
	if err := os.MkdirAll(filepath.Dir(trans2), 0o755); err != nil {
		t.Fatalf("mkdir2: %v", err)
	}
	if err := os.WriteFile(trans2, []byte(line1), 0o644); err != nil {
		t.Fatalf("write2: %v", err)
	}
	obs4, err := a.CollectIncremental(context.Background(), src, obs3.Checkpoint)
	if err != nil || len(obs4.Events) == 0 {
		t.Fatalf("new file not detected: err=%v events=%d", err, len(obs4.Events))
	}
}

// TestFailedParseWithholdsCheckpoint guards the checkpoint contract: when one
// transcript cannot be read, the manifest must not land, or the gate would
// skip that file's content until anything under the root next changes.
func TestFailedParseWithholdsCheckpoint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based open failure does not apply to root")
	}
	root := t.TempDir()
	good := `{"timestamp":"2026-05-29T10:00:00Z","sessionId":"s","requestId":"r1","message":{"id":"m1","model":"claude-opus","usage":{"input_tokens":400,"output_tokens":600}}}`
	bad := `{"timestamp":"2026-05-29T10:01:00Z","sessionId":"s2","requestId":"r2","message":{"id":"m2","model":"claude-opus","usage":{"input_tokens":10,"output_tokens":20}}}`
	writeFixture(t, root, "proj", "good", []string{good})
	writeFixture(t, root, "proj", "bad", []string{bad})
	badPath := filepath.Join(root, "projects", "proj", "bad.jsonl")
	if err := os.Chmod(badPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	a := Adapter{}
	src := adapter.Source{Tool: model.ToolClaudeCode, Class: model.EventLevel, Path: root}

	obs, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("collect: %v", err) // per-file errors stay non-fatal
	}
	if len(obs.Events) != 1 || obs.Events[0].MessageID != "m1" {
		t.Fatalf("want only the readable file's event, got %d", len(obs.Events))
	}
	if obs.Checkpoint != nil {
		t.Fatalf("checkpoint must be withheld on a failed parse, got %+v", obs.Checkpoint)
	}

	// Once readable again, the quiescent root must still be collected in full.
	if err := os.Chmod(badPath, 0o644); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	obs2, err := a.CollectIncremental(context.Background(), src, nil)
	if err != nil || len(obs2.Events) != 2 || obs2.Checkpoint == nil {
		t.Fatalf("recovery read: err=%v events=%d cp=%v", err, len(obs2.Events), obs2.Checkpoint)
	}
}
