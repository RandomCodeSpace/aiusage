package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// secretSkillInput is planted in the input of every Skill call and in the skill
// argument blobs a fixture carries. No field of any emitted record may contain
// it: the skill NAME is a fact worth recording, everything the skill was handed
// is content.
const secretSkillInput = "API_KEY=sk-live-4f2a deploy --to prod --db /home/dev/secret.db"

// assistantUnderSkill builds an assistant record carrying a usage block, the
// top-level attributionSkill field, and optional tool_use blocks — the exact
// shape Claude Code writes for a turn executing inside a skill.
func assistantUnderSkill(msgID, ts, skill, blocks string) string {
	return `{"type":"assistant","uuid":"u-` + msgID + `","timestamp":"` + ts + `",` +
		`"sessionId":"sess-1","cwd":"/home/dev/proj","requestId":"req-` + msgID + `",` +
		`"attributionSkill":"` + skill + `",` +
		`"attributionAgent":"general-purpose","attributionPlugin":"someplugin",` +
		`"message":{"id":"` + msgID + `","model":"claude-sonnet-4-6","role":"assistant",` +
		`"content":[` + blocks + `],` +
		`"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10}}}`
}

// TestSkillContextPairsWithItsOwnUsageEvent pins the join. The context names the
// dedup key of the usage event from the SAME record, which is what makes the
// store's 1:1 join to the ledger sound.
func TestSkillContextPairsWithItsOwnUsageEvent(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantUnderSkill("msg-1", "2026-05-29T17:14:22.354Z", "adhd",
			toolBlock("toolu_1", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(obs.Events))
	}
	if len(obs.SkillContexts) != 1 {
		t.Fatalf("want 1 skill context, got %d", len(obs.SkillContexts))
	}
	c := obs.SkillContexts[0]
	if c.UsageDedupKey != obs.Events[0].DedupKey {
		t.Errorf("context names usage key %q, want the event's own %q",
			c.UsageDedupKey, obs.Events[0].DedupKey)
	}
	if c.Skill != "adhd" {
		t.Errorf("skill = %q, want adhd", c.Skill)
	}
	if c.Tool != model.ToolClaudeCode {
		t.Errorf("tool = %q, want %q", c.Tool, model.ToolClaudeCode)
	}
	if !c.EventTime.Equal(obs.Events[0].EventTime) {
		t.Errorf("context time %s != usage time %s; the two would not window together",
			c.EventTime, obs.Events[0].EventTime)
	}
	if c.SessionID != obs.Events[0].SessionID || c.Project != obs.Events[0].Project ||
		c.Model != obs.Events[0].Model {
		t.Errorf("context dimensions %+v do not match the event they describe", c)
	}

	// The tool call is unaffected: skill context does not consume a call slot,
	// so the divisor stays at the number of real calls.
	if len(obs.Activity) != 1 || obs.Activity[0].CallsInTurn != 1 {
		t.Fatalf("activity = %+v, want one Bash call with CallsInTurn=1", obs.Activity)
	}
}

// TestSkillContextOnTurnWithNoToolCalls is the case the activity ledger cannot
// see at all: a turn that ran inside a skill and called nothing. Locally these
// are 3,361 of 8,039 skill records, so losing them would lose 41.8% of the
// answer.
func TestSkillContextOnTurnWithNoToolCalls(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantUnderSkill("msg-1", "2026-05-29T17:14:22.354Z", "repo-recon",
			`{"type":"text","text":"thinking about it"}`),
	})

	obs := collectObs(t, root)
	if len(obs.Activity) != 0 {
		t.Fatalf("want 0 activity rows for a turn with no tool calls, got %d", len(obs.Activity))
	}
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 usage event, got %d", len(obs.Events))
	}
	if len(obs.SkillContexts) != 1 || obs.SkillContexts[0].Skill != "repo-recon" {
		t.Fatalf("context-only turn produced %+v, want one repo-recon context", obs.SkillContexts)
	}
}

// TestNoSkillContextWithoutTheField checks the absence path. The overwhelming
// majority of records carry no attributionSkill, and none of them may acquire a
// context by inference from a nearby skill invocation — "the last skill I saw"
// would keep charging a skill long after it returned.
func TestNoSkillContextWithoutTheField(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		// A turn that INVOKES a skill is not a turn running inside one.
		assistantWithTools("msg-1", "2026-05-29T17:14:22.354Z",
			`{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"adhd","args":"`+secretSkillInput+`"}}`),
		assistantWithTools("msg-2", "2026-05-29T17:15:22.354Z", toolBlock("toolu_2", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.SkillContexts) != 0 {
		t.Fatalf("records with no attributionSkill produced %d contexts, want 0: %+v",
			len(obs.SkillContexts), obs.SkillContexts)
	}
	// The v5 behaviour is untouched: the invocation is still a kind=skill row.
	var found bool
	for _, a := range obs.Activity {
		if a.Kind == model.ActivitySkill && a.Name == "adhd" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the Skill invocation row disappeared: %+v", obs.Activity)
	}
}

// TestSkillContextNestingRecordsTheInnermost documents what the source gives us
// for a skill that invokes another. Once the inner skill is running, the field
// names ONLY the inner one, so its turns are charged to it and the outer skill
// is not credited with its callee's spend. Five such records exist locally; the
// alternative — inventing a parent chain the transcript does not record — would
// be a guess wearing a number.
func TestSkillContextNestingRecordsTheInnermost(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		// Outer skill runs, and from inside it invokes another skill.
		assistantUnderSkill("msg-1", "2026-05-29T17:14:22.354Z", "superpowers:brainstorming",
			`{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"superpowers:writing-plans","args":"`+secretSkillInput+`"}}`),
		// The inner skill's own work: attributed to the inner skill alone.
		assistantUnderSkill("msg-2", "2026-05-29T17:15:22.354Z", "superpowers:writing-plans",
			toolBlock("toolu_2", "Write")),
	})

	obs := collectObs(t, root)
	if len(obs.SkillContexts) != 2 {
		t.Fatalf("want 2 contexts, got %d: %+v", len(obs.SkillContexts), obs.SkillContexts)
	}
	got := map[string]string{}
	for _, c := range obs.SkillContexts {
		got[c.UsageDedupKey] = c.Skill
	}
	// Exactly one context per usage event, and the second turn belongs to the
	// inner skill only.
	if len(got) != 2 {
		t.Fatalf("contexts collided on a usage key: %+v", obs.SkillContexts)
	}
	var inner int
	for _, s := range got {
		if s == "superpowers:writing-plans" {
			inner++
		}
	}
	if inner != 1 {
		t.Fatalf("skills = %+v, want exactly one turn attributed to the inner skill", got)
	}
}

// TestSkillContextPrivacy is the hard privacy assertion: a secret planted in the
// skill's inputs must not survive into ANY emitted field of ANY stream. The
// enforcement is at decode — the input struct has one field, so encoding/json
// discards the rest as it parses and the secret never becomes a value in this
// process — and this test is what proves the shape holds end to end.
func TestSkillContextPrivacy(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantUnderSkill("msg-1", "2026-05-29T17:14:22.354Z", "adhd",
			`{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"nested","prompt":"`+secretSkillInput+`","command":"`+secretCommand+`"}}`),
		assistantUnderSkill("msg-2", "2026-05-29T17:15:22.354Z", "adhd",
			toolBlock("toolu_2", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.SkillContexts) == 0 {
		t.Fatal("fixture produced no skill contexts; the privacy assertion would be vacuous")
	}

	// Serialise every stream in full and search the bytes. A field-by-field
	// check would miss a leak into a field added later; this cannot.
	for name, v := range map[string]any{
		"skill contexts": obs.SkillContexts,
		"activity":       obs.Activity,
		"events":         obs.Events,
	} {
		blob, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		for _, secret := range []string{secretSkillInput, secretCommand} {
			if strings.Contains(string(blob), secret) {
				t.Fatalf("%s leaked skill input content: %s", name, blob)
			}
		}
		// Fragments too: a partial copy is still a leak.
		for _, frag := range []string{"sk-live-4f2a", "secret.db", "id_ed25519"} {
			if strings.Contains(string(blob), frag) {
				t.Fatalf("%s leaked the fragment %q: %s", name, frag, blob)
			}
		}
	}

	// The names themselves DID survive — otherwise the test above passes by
	// recording nothing at all.
	if obs.SkillContexts[0].Skill != "adhd" {
		t.Fatalf("skill name = %q, want adhd", obs.SkillContexts[0].Skill)
	}
}

// TestSkillContextRawIsUnaffected checks the audit payload allow-list did not
// quietly grow a skill field. Raw is usage-object-only; the skill context has
// its own column and does not belong there.
func TestSkillContextRawIsUnaffected(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantUnderSkill("msg-1", "2026-05-29T17:14:22.354Z", "adhd",
			toolBlock("toolu_1", "Bash")),
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(obs.Events))
	}
	if strings.Contains(obs.Events[0].Raw, "adhd") ||
		strings.Contains(obs.Events[0].Raw, "attributionSkill") {
		t.Fatalf("raw audit payload grew a skill field: %s", obs.Events[0].Raw)
	}
}

// TestMalformedAttributionSkillKeepsTheUsageRow is the degradation guarantee.
// Enrichment must never become a way to LOSE a usage row: a null, a number or an
// object where a string was expected yields no context and leaves the event
// exactly as it would have been before this field existed.
func TestMalformedAttributionSkillKeepsTheUsageRow(t *testing.T) {
	shapes := []string{`null`, `123`, `{"name":"adhd"}`, `["adhd"]`, `""`, `"   "`}
	for i, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			line := `{"type":"assistant","uuid":"u-x","timestamp":"2026-05-29T17:14:22.354Z",` +
				`"sessionId":"sess-1","cwd":"/home/dev/proj","requestId":"req-x",` +
				`"attributionSkill":` + shape + `,` +
				`"message":{"id":"msg-` + fmt.Sprint(i) + `","model":"claude-sonnet-4-6","role":"assistant",` +
				`"content":[],"usage":{"input_tokens":100,"output_tokens":50}}}`
			writeFixture(t, root, "proj", "sess-1", []string{line})

			obs := collectObs(t, root)
			if len(obs.Events) != 1 {
				t.Fatalf("attributionSkill=%s cost the usage row: got %d events", shape, len(obs.Events))
			}
			if obs.Events[0].TotalTokens != 150 {
				t.Fatalf("usage accounting changed: total = %d, want 150", obs.Events[0].TotalTokens)
			}
			if len(obs.SkillContexts) != 0 {
				t.Fatalf("attributionSkill=%s produced a context: %+v", shape, obs.SkillContexts)
			}
		})
	}
}

// TestSkillContextFollowsTheDedupWinner checks a sidechain replay does not
// produce a context pointing at a usage row that events() never emitted. The
// context must name the WINNING candidate's key, or it would join nothing and
// the surviving turn would show no skill at all.
func TestSkillContextFollowsTheDedupWinner(t *testing.T) {
	root := t.TempDir()
	replay := `{"type":"assistant","uuid":"u-replay","timestamp":"2026-05-29T17:14:22.354Z",` +
		`"sessionId":"sess-1","cwd":"/home/dev/proj","requestId":"req-other","isSidechain":true,` +
		`"attributionSkill":"adhd",` +
		`"message":{"id":"msg-1","model":"claude-sonnet-4-6","role":"assistant",` +
		`"content":[],"usage":{"input_tokens":100,"output_tokens":50}}}`
	writeFixture(t, root, "proj", "sess-1", []string{
		assistantUnderSkill("msg-1", "2026-05-29T17:14:22.354Z", "adhd", ""),
		replay,
	})

	obs := collectObs(t, root)
	if len(obs.Events) != 1 {
		t.Fatalf("dedup left %d events, want 1", len(obs.Events))
	}
	if len(obs.SkillContexts) != 1 {
		t.Fatalf("dedup left %d contexts, want 1", len(obs.SkillContexts))
	}
	if obs.SkillContexts[0].UsageDedupKey != obs.Events[0].DedupKey {
		t.Fatalf("context names %q but the surviving event is %q",
			obs.SkillContexts[0].UsageDedupKey, obs.Events[0].DedupKey)
	}
}
