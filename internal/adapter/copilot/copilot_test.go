package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiusage/internal/adapter"
	"aiusage/internal/model"
)

// writeOTEL writes the given JSONL lines to <home>/.copilot/otel/<name> and
// returns the home directory.
func writeOTEL(t *testing.T, lines ...string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".copilot", "otel")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir otel: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copilot.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return home
}

func collectAll(t *testing.T, home string) []model.UsageEvent {
	t.Helper()
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var evs []model.UsageEvent
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		evs = append(evs, obs.Events...)
	}
	return evs
}

// TestChatSpanAndDuplicateInferenceSuppression is the headline case: a chat span
// and an inference log that share a traceId and response.id. The chat span wins;
// the inference log is suppressed. Token mapping is verified on the survivor.
func TestChatSpanAndDuplicateInferenceSuppression(t *testing.T) {
	chatSpan := `{"type":"span","traceId":"trace-1","spanId":"span-1","name":"chat claude-sonnet-4",` +
		`"endTime":[1775934264,967317833],"attributes":{` +
		`"gen_ai.operation.name":"chat",` +
		`"gen_ai.request.model":"claude-sonnet-4",` +
		`"gen_ai.response.model":"claude-sonnet-4",` +
		`"gen_ai.response.id":"resp-1",` +
		`"gen_ai.conversation.id":"conv-1",` +
		`"gen_ai.usage.input_tokens":19452,` +
		`"gen_ai.usage.output_tokens":281,` +
		`"gen_ai.usage.cache_read.input_tokens":123,` +
		`"gen_ai.usage.cache_creation.input_tokens":25,` +
		`"gen_ai.usage.reasoning.output_tokens":128}}`

	// Same trace + response id, lower priority -> must be suppressed.
	inferenceLog := `{"_body":"GenAI inference: claude-sonnet-4","hrTime":[1775934263,0],"attributes":{` +
		`"event.name":"gen_ai.client.inference.operation.details",` +
		`"gen_ai.response.model":"claude-sonnet-4",` +
		`"gen_ai.response.id":"resp-1",` +
		`"gen_ai.conversation.id":"conv-1",` +
		`"gen_ai.usage.input_tokens":80,` +
		`"gen_ai.usage.output_tokens":20}}`

	home := writeOTEL(t, chatSpan, inferenceLog)
	evs := collectAll(t, home)

	if len(evs) != 1 {
		t.Fatalf("want 1 surviving event, got %d: %+v", len(evs), evs)
	}
	e := evs[0]

	// Token map: Input = 19452 - min(19452,123) = 19329; cache_read separate.
	if e.InputTokens != 19329 {
		t.Errorf("InputTokens = %d, want 19329", e.InputTokens)
	}
	if e.OutputTokens != 281 {
		t.Errorf("OutputTokens = %d, want 281", e.OutputTokens)
	}
	if e.CacheReadTokens != 123 {
		t.Errorf("CacheReadTokens = %d, want 123", e.CacheReadTokens)
	}
	if e.CacheCreationTokens != 25 {
		t.Errorf("CacheCreationTokens = %d, want 25", e.CacheCreationTokens)
	}
	if e.ReasoningTokens != 128 {
		t.Errorf("ReasoningTokens = %d, want 128", e.ReasoningTokens)
	}
	// Provider-authoritative total = in+out+cacheC+cacheR+reasoning.
	wantTotal := int64(19329 + 281 + 25 + 123 + 128)
	if e.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", e.TotalTokens, wantTotal)
	}
	if e.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q, want claude-sonnet-4", e.Model)
	}
	if e.SessionID != "conv-1" {
		t.Errorf("SessionID = %q, want conv-1", e.SessionID)
	}
	if e.Project != "" {
		t.Errorf("Project = %q, want empty", e.Project)
	}
	if e.DedupKey != "copilot|trace-1:span-1" {
		t.Errorf("DedupKey = %q, want copilot|trace-1:span-1", e.DedupKey)
	}
	if e.Tool != model.ToolCopilot {
		t.Errorf("Tool = %q, want %q", e.Tool, model.ToolCopilot)
	}
	if e.EventTime.IsZero() {
		t.Error("EventTime is zero; expected parsed endTime")
	}
	if got := e.EventTime.UTC().Format("2006-01-02T15:04:05.000Z"); got != "2026-04-11T19:04:24.967Z" {
		t.Errorf("EventTime = %s, want 2026-04-11T19:04:24.967Z", got)
	}
}

// TestTotalTokenFallbackFillsOutput verifies that when only a grand total is
// present, the missing remainder fills the empty output gap.
func TestTotalTokenFallbackFillsOutput(t *testing.T) {
	span := `{"type":"span","traceId":"t","spanId":"s","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c","gen_ai.usage.total_tokens":567}}`
	home := writeOTEL(t, span)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].OutputTokens != 567 {
		t.Errorf("OutputTokens = %d, want 567 (total fallback)", evs[0].OutputTokens)
	}
	if evs[0].ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d, want 0", evs[0].ReasoningTokens)
	}
	if evs[0].TotalTokens != 567 {
		t.Errorf("TotalTokens = %d, want 567", evs[0].TotalTokens)
	}
}

// TestNumericStringValues confirms that stringified numbers are parsed.
func TestNumericStringValues(t *testing.T) {
	span := `{"type":"span","traceId":"t2","spanId":"s2","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c2",` +
		`"gen_ai.usage.input_tokens":"100",` +
		`"gen_ai.usage.output_tokens":"50",` +
		`"gen_ai.usage.cache_read.input_tokens":"10"}}`
	home := writeOTEL(t, span)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].InputTokens != 90 { // 100 - min(100,10)
		t.Errorf("InputTokens = %d, want 90", evs[0].InputTokens)
	}
	if evs[0].OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", evs[0].OutputTokens)
	}
	if evs[0].CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10", evs[0].CacheReadTokens)
	}
}

// TestEmptyWhenNoOtelDir verifies graceful behaviour when ~/.copilot/otel is
// absent: no sources, no error.
func TestEmptyWhenNoOtelDir(t *testing.T) {
	home := t.TempDir() // no .copilot/otel
	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover returned error: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("want 0 sources, got %d", len(srcs))
	}
}

// TestExporterEnvAdditive verifies the COPILOT_OTEL_FILE_EXPORTER_PATH single
// file is discovered additively to the default directory.
func TestExporterEnvAdditive(t *testing.T) {
	home := writeOTEL(t,
		`{"type":"span","traceId":"a","spanId":"a1","name":"chat m","endTime":[1775934264,0],`+
			`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",`+
			`"gen_ai.conversation.id":"ca","gen_ai.usage.input_tokens":10,"gen_ai.usage.output_tokens":5}}`)

	extraDir := t.TempDir()
	extraFile := filepath.Join(extraDir, "extra.jsonl")
	if err := os.WriteFile(extraFile, []byte(
		`{"type":"span","traceId":"b","spanId":"b1","name":"chat m","endTime":[1775934264,0],`+
			`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",`+
			`"gen_ai.conversation.id":"cb","gen_ai.usage.input_tokens":7,"gen_ai.usage.output_tokens":3}}`+"\n"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}
	t.Setenv(exporterEnv, extraFile)

	a := New()
	srcs, err := a.Discover(context.Background(), adapter.DiscoverConfig{Home: home})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("want 2 sources (dir file + env file), got %d: %+v", len(srcs), srcs)
	}

	var total int64
	for _, s := range srcs {
		obs, err := a.Collect(context.Background(), s)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		for _, e := range obs.Events {
			total += e.TotalTokens
		}
	}
	if total != (10+5)+(7+3) {
		t.Errorf("aggregate TotalTokens = %d, want 25", total)
	}
}

// TestAgentTurnSuppressedByInference verifies a deeper rung of the priority
// chain: an agent-turn log sharing a trace with an inference log is suppressed.
func TestAgentTurnSuppressedByInference(t *testing.T) {
	// Both records carry the shared traceId at the record top level (where the
	// parser looks). Inference outranks agent-turn, so the turn is suppressed.
	inference := `{"traceId":"shared","_body":"GenAI inference: m","hrTime":[1775934263,0],"attributes":{` +
		`"event.name":"gen_ai.client.inference.operation.details",` +
		`"gen_ai.response.model":"m","gen_ai.conversation.id":"c",` +
		`"gen_ai.usage.input_tokens":80,"gen_ai.usage.output_tokens":20}}`
	agentTurn := `{"traceId":"shared","_body":"copilot_chat.agent.turn","hrTime":[1775934262,0],"attributes":{` +
		`"event.name":"copilot_chat.agent.turn","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"c","turn.index":3,` +
		`"gen_ai.usage.input_tokens":40,"gen_ai.usage.output_tokens":10}}`

	home := writeOTEL(t, inference, agentTurn)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 surviving event (inference wins), got %d: %+v", len(evs), evs)
	}
	if !strings.HasPrefix(evs[0].DedupKey, "copilot|log:") {
		t.Errorf("survivor DedupKey = %q, want inference log key", evs[0].DedupKey)
	}
}

// TestMalformedLinesSkipped ensures bad JSON and lines without "attributes" do
// not fail the cycle and do not produce events.
func TestMalformedLinesSkipped(t *testing.T) {
	good := `{"type":"span","traceId":"g","spanId":"g1","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"cg","gen_ai.usage.input_tokens":10,"gen_ai.usage.output_tokens":5}}`
	home := writeOTEL(t,
		`{not valid json "attributes"}`,
		`{"type":"metric","name":"gen_ai.client.token.usage"}`, // no "attributes" marker
		`{"attributes":{"some":"thing"}}`,                      // attributes but no usage shape
		good,
	)
	evs := collectAll(t, home)
	if len(evs) != 1 {
		t.Fatalf("want 1 event (only the good span), got %d: %+v", len(evs), evs)
	}
	if evs[0].TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", evs[0].TotalTokens)
	}
}

// TestZeroTokenRecordDropped ensures records with no tokens are dropped.
func TestZeroTokenRecordDropped(t *testing.T) {
	span := `{"type":"span","traceId":"z","spanId":"z1","name":"chat m","endTime":[1775934264,0],` +
		`"attributes":{"gen_ai.operation.name":"chat","gen_ai.response.model":"m",` +
		`"gen_ai.conversation.id":"cz"}}`
	home := writeOTEL(t, span)
	evs := collectAll(t, home)
	if len(evs) != 0 {
		t.Fatalf("want 0 events (no tokens), got %d", len(evs))
	}
}
