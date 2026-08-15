package claudecode

import (
	"context"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/adapter"
	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// collectObs runs a full collection over a fixture root and returns both
// streams, so a test can assert the pairing between them.
func collectObs(t *testing.T, root string) adapter.Observation {
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
	return obs
}

// secretCommand is planted in every tool input a fixture carries. No assertion
// in this file may ever find it in an activity row.
const secretCommand = "rm -rf /home/dev/secret-project && cat ~/.ssh/id_ed25519"

// assistantWithTools builds an assistant transcript line carrying a usage block
// and the given tool_use blocks, each with a real-looking (and forbidden) input.
func assistantWithTools(msgID, ts string, blocks string) string {
	return `{"type":"assistant","uuid":"u-` + msgID + `","timestamp":"` + ts + `",` +
		`"sessionId":"sess-1","cwd":"/home/dev/proj","requestId":"req-` + msgID + `",` +
		`"message":{"id":"` + msgID + `","model":"claude-sonnet-4-6","role":"assistant",` +
		`"content":[` + blocks + `],` +
		`"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10}}}`
}

func toolBlock(id, name string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"` + name + `",` +
		`"input":{"command":"` + secretCommand + `","description":"do the thing"}}`
}

// TestActivityJoinsItsOwnUsageRecord pins the exact join the attribution rests
// on: the tool call and the usage object come from ONE record, so the activity
// row names that record's event and counts how many calls shared it.
func TestActivityJoinsItsOwnUsageRecord(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantWithTools("msg-1", "2026-05-29T17:14:22.354Z", toolBlock("toolu_1", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(obs.Events))
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	a := obs.Activity[0]
	if a.UsageDedupKey != obs.Events[0].DedupKey {
		t.Errorf("activity names usage key %q, want the event's own %q",
			a.UsageDedupKey, obs.Events[0].DedupKey)
	}
	if a.Name != "Bash" || a.Kind != model.ActivityTool {
		t.Errorf("got kind=%s name=%s, want tool/Bash", a.Kind, a.Name)
	}
	if a.CallsInTurn != 1 || a.TurnSeq != 0 {
		t.Errorf("turn position = %d/%d, want 0/1", a.TurnSeq, a.CallsInTurn)
	}
	if !a.EventTime.Equal(obs.Events[0].EventTime) {
		t.Errorf("activity time %s != usage time %s; the two ledgers would not window together",
			a.EventTime, obs.Events[0].EventTime)
	}
	if a.SessionID != "sess-1" || a.Project != "/home/dev/proj" || a.Model != "claude-sonnet-4-6" {
		t.Errorf("attributes not inherited from the usage event: %+v", a)
	}
	if a.DedupKey != "claude-code|call|toolu_1" {
		t.Errorf("dedup key = %q, want the provider block id", a.DedupKey)
	}
}

// TestMultiCallTurnCountsItsCalls is the case that would inflate every cost
// headline if it counted wrong: two calls, ONE usage object.
func TestMultiCallTurnCountsItsCalls(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantWithTools("msg-1", "2026-05-29T17:14:22.354Z",
			toolBlock("toolu_1", "Read")+","+toolBlock("toolu_2", "Edit")),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 usage event (one turn), got %d", len(obs.Events))
	}
	if len(obs.Activity) != 2 {
		t.Fatalf("want 2 activity rows, got %d", len(obs.Activity))
	}
	for i, a := range obs.Activity {
		if a.CallsInTurn != 2 {
			t.Errorf("row %d: calls_in_turn = %d, want 2 — the read path divides by "+
				"this, so a wrong value double counts the turn", i, a.CallsInTurn)
		}
		if a.TurnSeq != i {
			t.Errorf("row %d: turn_seq = %d, want %d", i, a.TurnSeq, i)
		}
		if a.UsageDedupKey != obs.Events[0].DedupKey {
			t.Errorf("row %d does not name the shared usage event", i)
		}
	}
}

// TestSkillCallRecordsTheSkillNotTheTool: "Skill" is never the interesting
// fact; which skill ran is.
func TestSkillCallRecordsTheSkillNotTheTool(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"assistant","uuid":"u-1","timestamp":"2026-05-29T17:14:22.354Z",` +
		`"sessionId":"sess-1","cwd":"/home/dev/proj",` +
		`"message":{"id":"msg-1","model":"claude-sonnet-4-6",` +
		`"content":[{"type":"tool_use","id":"toolu_1","name":"Skill",` +
		`"input":{"skill":"artifact-design","args":"` + secretCommand + `"}}],` +
		`"usage":{"input_tokens":10,"output_tokens":5}}}`
	writeFixture(t, root, "proj", "sess-1", []string{line})

	obs := collectObs(t, root)
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	a := obs.Activity[0]
	if a.Kind != model.ActivitySkill {
		t.Errorf("kind = %s, want skill", a.Kind)
	}
	if a.Name != "artifact-design" {
		t.Errorf("name = %q, want the skill name", a.Name)
	}
}

// TestSkillWithoutASkillNameStaysATool: a Skill call whose input names no skill
// must not become a nameless skill row.
func TestSkillWithoutASkillNameStaysATool(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"assistant","uuid":"u-1","timestamp":"2026-05-29T17:14:22.354Z",` +
		`"sessionId":"sess-1","message":{"id":"msg-1","model":"claude-sonnet-4-6",` +
		`"content":[{"type":"tool_use","id":"toolu_1","name":"Skill","input":{}}],` +
		`"usage":{"input_tokens":10,"output_tokens":5}}}`
	writeFixture(t, root, "proj", "sess-1", []string{line})

	obs := collectObs(t, root)
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(obs.Activity))
	}
	if obs.Activity[0].Kind != model.ActivityTool || obs.Activity[0].Name != "Skill" {
		t.Errorf("got %s/%s, want tool/Skill", obs.Activity[0].Kind, obs.Activity[0].Name)
	}
}

// TestMCPToolNamesAreKept: knowing which MCP server got called is the point.
func TestMCPToolNamesAreKept(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantWithTools("msg-1", "2026-05-29T17:14:22.354Z",
			toolBlock("toolu_1", "mcp__plugin_playwright_playwright__browser_click")),
	})

	obs := collectObs(t, root)
	if len(obs.Activity) != 1 ||
		obs.Activity[0].Name != "mcp__plugin_playwright_playwright__browser_click" {
		t.Fatalf("MCP tool name not preserved: %+v", obs.Activity)
	}
}

// TestActivityNeverCarriesToolInput is the privacy invariant, asserted against
// the whole emitted row rather than against the fields a reviewer remembered to
// check. The input is not stripped after parsing — the decode shape has no
// field for it — so this fails loudly the day someone widens that struct.
func TestActivityNeverCarriesToolInput(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantWithTools("msg-1", "2026-05-29T17:14:22.354Z",
			toolBlock("toolu_1", "Bash")+","+toolBlock("toolu_2", "Write")),
	})

	obs := collectObs(t, root)
	if len(obs.Activity) == 0 {
		t.Fatal("no activity collected; the privacy assertion would be vacuous")
	}
	for _, a := range obs.Activity {
		for field, v := range map[string]string{
			"Name": a.Name, "SessionID": a.SessionID, "Project": a.Project,
			"Model": a.Model, "DedupKey": a.DedupKey, "UsageDedupKey": a.UsageDedupKey,
			"MessageID": a.MessageID, "RequestID": a.RequestID, "SourcePath": a.SourcePath,
		} {
			if strings.Contains(v, secretCommand) || strings.Contains(v, "do the thing") {
				t.Fatalf("activity field %s leaked tool input: %q", field, v)
			}
		}
	}
}

// TestHookRecordsAreCollected covers the type=="system" record, which carries
// no usage block at all and so is invisible to the usage marker.
func TestHookRecordsAreCollected(t *testing.T) {
	root := t.TempDir()
	hook := `{"type":"system","subtype":"stop_hook_summary","uuid":"hook-uuid-1",` +
		`"timestamp":"2026-05-29T17:20:00.000Z","sessionId":"sess-1","cwd":"/home/dev/proj",` +
		`"hookCount":2,"hookInfos":[{"command":"` + secretCommand + `","durationMs":12},` +
		`{"command":"` + secretCommand + `"}],"hookErrors":[]}`
	writeFixture(t, root, "proj", "sess-1", []string{hook})

	obs := collectObs(t, root)
	if len(obs.Events) != 0 {
		t.Fatalf("a hook record produced %d usage events, want 0", len(obs.Events))
	}
	if len(obs.Activity) != 2 {
		t.Fatalf("want 2 hook rows (one per hook that fired), got %d", len(obs.Activity))
	}
	for i, a := range obs.Activity {
		if a.Kind != model.ActivityHook || a.Name != hookEventName {
			t.Errorf("row %d = %s/%s, want hook/%s", i, a.Kind, a.Name, hookEventName)
		}
		if a.UsageDedupKey != "" {
			t.Errorf("row %d attributes cost to %q; a hook has no usage to attribute",
				i, a.UsageDedupKey)
		}
		if strings.Contains(a.Name, "rm -rf") || strings.Contains(a.DedupKey, "rm -rf") {
			t.Fatalf("row %d leaked the hook command", i)
		}
		if a.SessionID != "sess-1" || a.Project != "/home/dev/proj" {
			t.Errorf("row %d lost its context: %+v", i, a)
		}
	}
	if obs.Activity[0].DedupKey == obs.Activity[1].DedupKey {
		t.Error("both hook rows share a dedup key; one of them would be dropped")
	}
}

// TestSidechainReplayDoesNotDoubleCountCalls: a replayed record repeats its
// tool_use blocks, and emitting both copies would attribute the same turn twice
// and count the call twice.
func TestSidechainReplayDoesNotDoubleCountCalls(t *testing.T) {
	root := t.TempDir()
	primary := `{"type":"assistant","uuid":"u-1","timestamp":"2026-05-29T17:14:22.354Z",` +
		`"sessionId":"sess-1","requestId":"req-1",` +
		`"message":{"id":"msg-1","model":"claude-sonnet-4-6",` +
		`"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}],` +
		`"usage":{"input_tokens":100,"output_tokens":50}}}`
	replay := `{"type":"assistant","uuid":"u-2","timestamp":"2026-05-29T17:14:22.354Z",` +
		`"sessionId":"sess-1","requestId":"req-2","isSidechain":true,` +
		`"message":{"id":"msg-1","model":"claude-sonnet-4-6",` +
		`"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}],` +
		`"usage":{"input_tokens":100,"output_tokens":50}}}`
	writeFixture(t, root, "proj", "sess-1", []string{primary, replay})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 deduped usage event, got %d", len(obs.Events))
	}
	if len(obs.Activity) != 1 {
		t.Fatalf("want 1 activity row after replay dedup, got %d", len(obs.Activity))
	}
	if obs.Activity[0].UsageDedupKey != obs.Events[0].DedupKey {
		t.Error("the surviving call does not name the surviving usage event")
	}
}

// TestActivityIsStableAcrossReReads: the adapter re-parses every transcript of
// a root whenever any file under it changes, so re-derived rows must carry
// identical dedup keys or the ledger would grow a copy per cycle.
func TestActivityIsStableAcrossReReads(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantWithTools("msg-1", "2026-05-29T17:14:22.354Z", toolBlock("toolu_1", "Bash")),
		`{"type":"system","subtype":"stop_hook_summary","uuid":"hook-1",` +
			`"timestamp":"2026-05-29T17:20:00.000Z","sessionId":"sess-1","hookCount":1,` +
			`"hookInfos":[{"command":"x"}]}`,
	})

	first, second := collectObs(t, root), collectObs(t, root)
	if len(first.Activity) != 2 {
		t.Fatalf("want 2 activity rows, got %d", len(first.Activity))
	}
	if len(first.Activity) != len(second.Activity) {
		t.Fatalf("re-read produced %d rows, first read %d", len(second.Activity), len(first.Activity))
	}
	for i := range first.Activity {
		if first.Activity[i].DedupKey != second.Activity[i].DedupKey {
			t.Errorf("row %d dedup key changed between reads: %q then %q",
				i, first.Activity[i].DedupKey, second.Activity[i].DedupKey)
		}
	}
}

// TestNonToolContentBlocksAreIgnored: text and thinking blocks are not calls.
func TestNonToolContentBlocksAreIgnored(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"assistant","uuid":"u-1","timestamp":"2026-05-29T17:14:22.354Z",` +
		`"sessionId":"sess-1","message":{"id":"msg-1","model":"claude-sonnet-4-6",` +
		`"content":[{"type":"text","text":"` + secretCommand + `"},` +
		`{"type":"thinking","thinking":"secret reasoning"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5}}}`
	writeFixture(t, root, "proj", "sess-1", []string{line})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want the usage event, got %d", len(obs.Events))
	}
	if len(obs.Activity) != 0 {
		t.Fatalf("text/thinking blocks produced %d activity rows, want 0: %+v",
			len(obs.Activity), obs.Activity)
	}
}
