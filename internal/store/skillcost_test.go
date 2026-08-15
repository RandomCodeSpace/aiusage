package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// skillCtx builds a skill-context row for a usage event's dedup key.
func skillCtx(usageKey, skill string, et time.Time) model.SkillContext {
	return model.SkillContext{
		UsageDedupKey: usageKey,
		Tool:          model.ToolClaudeCode,
		Skill:         skill,
		SessionID:     "s",
		Project:       "/p",
		Model:         "m",
		EventTime:     et,
	}
}

// skillFixture seeds the window every attribution test below reasons about. It
// is built to contain each case that could break the arithmetic at once:
//
//	u1  1000 tok / 10000 micro  skill=alpha  TWO tool calls  (multi-call turn
//	                                                          under a skill)
//	u2   500 tok /  5000 micro  skill=alpha  ZERO tool calls (context-only turn:
//	                                                          invisible to the
//	                                                          activity ledger)
//	u3   300 tok /  3000 micro  skill=beta   ONE tool call
//	u4   200 tok /  2000 micro  no skill     ONE tool call   (turn outside every
//	                                                          skill)
//
// Ledger for the window: 2000 tokens, 20000 micro-USD.
func skillFixture(t *testing.T, st *SQLite, base time.Time) {
	t.Helper()
	ctx := context.Background()

	type turn struct {
		key   string
		total int64
		skill string
		calls []string
	}
	turns := []turn{
		{"u1", 1000, "alpha", []string{"Bash", "Read"}},
		{"u2", 500, "alpha", nil},
		{"u3", 300, "beta", []string{"Bash"}},
		{"u4", 200, "", []string{"Grep"}},
	}

	var (
		events   []model.UsageEvent
		activity []model.ActivityEvent
		ctxs     []model.SkillContext
	)
	for i, tn := range turns {
		when := base.Add(time.Duration(i) * time.Minute)
		e := ev(tn.key, model.ToolClaudeCode, when, tn.total)
		e.SetCost(tn.total*10, "test")
		events = append(events, e)
		for c, name := range tn.calls {
			activity = append(activity, act(
				fmt.Sprintf("a-%s-%d", tn.key, c), name, model.ActivityTool,
				when, tn.key, c, len(tn.calls)))
		}
		if tn.skill != "" {
			ctxs = append(ctxs, skillCtx(tn.key, tn.skill, when))
		}
	}

	applied, err := st.ApplyBatch(ctx, ObservationBatch{
		Events: events, Activity: activity, SkillContexts: ctxs,
	})
	if err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if applied.Events != 4 || applied.Activity != 4 || applied.SkillContexts != 3 {
		t.Fatalf("applied = %+v, want 4 events / 4 activity / 3 skill contexts", applied)
	}
}

// TestSkillAndToolAttributionNeverExceedLedger is THE invariant of this feature.
//
// Two different attributions now read the same dollars: activity_events splits a
// turn's cost among the calls that shared it, and usage_skill_context assigns
// the whole turn to the one skill it ran under. Each must stay inside the
// window's real ledger total on its own, and neither may be inflated by the
// other's existence.
//
// The exact expected values are asserted, not just the bound. A bound alone is
// satisfied by attributing nothing at all, which is the failure mode that would
// silently make every skill look free.
//
// Note what the numbers do NOT do: 18000 + 15000 = 33000, well over the ledger's
// 20000. That is not a bug, it is the reason the two live in separate tables —
// they are two partitions of one budget ("which skill was running" and "which
// tool was called"), each complete on its own and meaningless summed together.
// No query in this package can produce that 33000, and this test exists to keep
// it that way.
func TestSkillAndToolAttributionNeverExceedLedger(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	skillFixture(t, st, base)

	since, until := base.Add(-time.Hour), base.Add(time.Hour)

	ledger, err := st.Summarize(ctx, Filter{Since: since, Until: until})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if ledger.Totals.Total != 2000 {
		t.Fatalf("ledger tokens = %d, want 2000", ledger.Totals.Total)
	}
	ledgerCost := ledger.Totals.CostMicroUSD
	if ledgerCost != 20000 {
		t.Fatalf("ledger cost = %d, want 20000", ledgerCost)
	}

	af := ActivityFilter{Since: since, Until: until}

	// Skill side: whole turns, no divisor. alpha = u1+u2, beta = u3; u4 ran
	// under no skill and is attributed to none, which is why 1800 < 2000.
	skills, err := st.SummarizeSkillCost(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeSkillCost: %v", err)
	}
	if got := skills.Totals.TotalTokens; got != 1800 {
		t.Fatalf("skill-attributed tokens = %d, want 1800", got)
	}
	if got := skills.Totals.CostMicroUSD; got != 18000 {
		t.Fatalf("skill-attributed cost = %d, want 18000", got)
	}
	if got := skills.Totals.Turns; got != 3 {
		t.Fatalf("skill-attributed turns = %d, want 3", got)
	}

	// Tool side: divided shares. u1's 1000 splits 500/500 across its two calls,
	// u2 contributes nothing (no calls at all), u3 300, u4 200.
	acts, err := st.SummarizeActivity(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if got := acts.Totals.AttributedTotal; got != 1500 {
		t.Fatalf("call-attributed tokens = %d, want 1500", got)
	}
	if got := acts.Totals.AttributedCostMicroUSD; got != 15000 {
		t.Fatalf("call-attributed cost = %d, want 15000", got)
	}

	// The bound, stated separately from the exact values so a future change that
	// alters the fixture still trips on the invariant itself.
	if skills.Totals.TotalTokens > ledger.Totals.Total {
		t.Fatalf("skill attribution %d exceeds ledger %d", skills.Totals.TotalTokens, ledger.Totals.Total)
	}
	if acts.Totals.AttributedTotal > ledger.Totals.Total {
		t.Fatalf("call attribution %d exceeds ledger %d", acts.Totals.AttributedTotal, ledger.Totals.Total)
	}
	if skills.Totals.CostMicroUSD > ledgerCost {
		t.Fatalf("skill cost %d exceeds ledger %d", skills.Totals.CostMicroUSD, ledgerCost)
	}
	if acts.Totals.AttributedCostMicroUSD > ledgerCost {
		t.Fatalf("call cost %d exceeds ledger %d", acts.Totals.AttributedCostMicroUSD, ledgerCost)
	}

	// Per-skill, the buckets partition the skill total exactly once each.
	perSkill, err := st.SummarizeSkillCost(ctx, ActivityFilter{
		Since: since, Until: until, GroupBy: []string{"skill"},
	})
	if err != nil {
		t.Fatalf("SummarizeSkillCost by skill: %v", err)
	}
	want := map[string]int64{"alpha": 15000, "beta": 3000}
	if len(perSkill.Buckets) != len(want) {
		t.Fatalf("got %d skill buckets, want %d: %+v", len(perSkill.Buckets), len(want), perSkill.Buckets)
	}
	var sum int64
	for _, b := range perSkill.Buckets {
		name := b.Keys["skill"]
		if b.CostMicroUSD != want[name] {
			t.Fatalf("skill %q cost = %d, want %d", name, b.CostMicroUSD, want[name])
		}
		sum += b.CostMicroUSD
	}
	if sum != 18000 {
		t.Fatalf("skill buckets sum to %d, want 18000", sum)
	}
}

// TestSkillContextsLeaveActivityTotalsUntouched is the structural half of the
// same claim, and the one a shared table would have failed.
//
// Had skill context been another activity_events kind, every skill turn would
// have gained a row and SummarizeActivity — which no caller filters by kind —
// would have counted it a second time. Here the numbers are taken with the
// contexts absent and again with them present, and they must be identical.
func TestSkillContextsLeaveActivityTotalsUntouched(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	af := ActivityFilter{Since: base.Add(-time.Hour), Until: base.Add(time.Hour)}

	// Same fixture, once without the skill contexts and once with.
	bare := openTemp(t)
	withCtx := openTemp(t)
	skillFixture(t, withCtx, base)

	var (
		events   []model.UsageEvent
		activity []model.ActivityEvent
	)
	rows, err := withCtx.ListActivity(ctx, ActivityFilter{})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	activity = append(activity, rows...)
	evs, err := withCtx.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	events = append(events, evs...)
	if _, err := bare.ApplyObservation(ctx, events, activity, nil); err != nil {
		t.Fatalf("seed bare store: %v", err)
	}

	a, err := bare.SummarizeActivity(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeActivity (bare): %v", err)
	}
	b, err := withCtx.SummarizeActivity(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeActivity (with contexts): %v", err)
	}
	// ActivityBucket holds a map, so compare the metrics field by field.
	for _, m := range []struct {
		name string
		a, b int64
	}{
		{"calls", a.Totals.Calls, b.Totals.Calls},
		{"sessions", a.Totals.Sessions, b.Totals.Sessions},
		{"input", a.Totals.AttributedInput, b.Totals.AttributedInput},
		{"output", a.Totals.AttributedOutput, b.Totals.AttributedOutput},
		{"total", a.Totals.AttributedTotal, b.Totals.AttributedTotal},
		{"cost", a.Totals.AttributedCostMicroUSD, b.Totals.AttributedCostMicroUSD},
		{"unattributed", a.Totals.UnattributedCalls, b.Totals.UnattributedCalls},
		{"unpriced", a.Totals.UnpricedCalls, b.Totals.UnpricedCalls},
	} {
		if m.a != m.b {
			t.Fatalf("skill contexts changed activity %s: without=%d with=%d", m.name, m.a, m.b)
		}
	}
	if a.Totals.AttributedCostMicroUSD != 15000 {
		t.Fatalf("activity cost = %d, want 15000; a zero would satisfy the equality trivially",
			a.Totals.AttributedCostMicroUSD)
	}
}

// TestSkillCostCountsContextOnlyTurns covers the 41.8% of real skill records
// that call no tool at all. They emit no activity row, so a skill_context COLUMN
// on activity_events would have had nowhere to record them and their cost would
// have vanished from every per-skill answer.
func TestSkillCostCountsContextOnlyTurns(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	skillFixture(t, st, base)

	af := ActivityFilter{
		Since: base.Add(-time.Hour), Until: base.Add(time.Hour),
		Skills: []string{"alpha"},
	}
	sum, err := st.SummarizeSkillCost(ctx, af)
	if err != nil {
		t.Fatalf("SummarizeSkillCost: %v", err)
	}
	// u1 (with calls) AND u2 (context-only) — 1000+500 tokens, 10000+5000 micro.
	if sum.Totals.Turns != 2 {
		t.Fatalf("alpha turns = %d, want 2 (the context-only turn must count)", sum.Totals.Turns)
	}
	if sum.Totals.TotalTokens != 1500 || sum.Totals.CostMicroUSD != 15000 {
		t.Fatalf("alpha = %d tok / %d micro, want 1500 / 15000", sum.Totals.TotalTokens, sum.Totals.CostMicroUSD)
	}

	// The activity ledger sees only u1's two calls for alpha's turns, which is
	// exactly the blind spot this table fills.
	acts, err := st.SummarizeActivity(ctx, ActivityFilter{
		Since: af.Since, Until: af.Until, Names: []string{"Bash", "Read"},
	})
	if err != nil {
		t.Fatalf("SummarizeActivity: %v", err)
	}
	if acts.Totals.Calls != 3 {
		t.Fatalf("activity calls = %d, want 3; the fixture's context-only turn must NOT appear here", acts.Totals.Calls)
	}
}

// TestSkillContextIsOnePerTurn pins the constraint the whole no-double-count
// argument rests on. The source cannot express two skills for one record, and
// the table cannot store them: a second context for the same usage row is
// absorbed by the primary key, so even a buggy adapter emitting both an outer
// and an inner skill for one turn cannot make that turn cost twice.
//
// This is also how NESTING behaves. A skill may invoke another (observed 5 times
// locally), and once the inner one runs the transcript names only the inner
// skill — so cost lands on the innermost active skill and the outer one is not
// credited with what its callee spent.
func TestSkillContextIsOnePerTurn(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	e := ev("u-nested", model.ToolClaudeCode, base, 900)
	e.SetCost(9000, "test")

	// The outer skill is recorded first; a later sighting claiming the inner one
	// for the SAME turn must not add a second helping of its cost.
	applied, err := st.ApplyBatch(ctx, ObservationBatch{
		Events:        []model.UsageEvent{e},
		SkillContexts: []model.SkillContext{skillCtx("u-nested", "outer", base)},
	})
	if err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if applied.SkillContexts != 1 {
		t.Fatalf("first apply inserted %d contexts, want 1", applied.SkillContexts)
	}

	again, err := st.ApplyBatch(ctx, ObservationBatch{
		SkillContexts: []model.SkillContext{
			skillCtx("u-nested", "inner", base),
			skillCtx("u-nested", "outer", base),
		},
	})
	if err != nil {
		t.Fatalf("ApplyBatch (repeat): %v", err)
	}
	if again.SkillContexts != 0 {
		t.Fatalf("re-applying contexts for one turn inserted %d, want 0", again.SkillContexts)
	}

	sum, err := st.SummarizeSkillCost(ctx, ActivityFilter{GroupBy: []string{"skill"}})
	if err != nil {
		t.Fatalf("SummarizeSkillCost: %v", err)
	}
	if len(sum.Buckets) != 1 || sum.Buckets[0].Keys["skill"] != "outer" {
		t.Fatalf("buckets = %+v, want exactly one for the first-recorded skill", sum.Buckets)
	}
	if sum.Totals.CostMicroUSD != 9000 {
		t.Fatalf("total cost = %d, want 9000 (the turn counted exactly once)", sum.Totals.CostMicroUSD)
	}
}

// TestTopSkillCostRanksByCost checks the "which skill is expensive" query
// answers with the skill that actually cost most, capped in SQL.
func TestTopSkillCostRanksByCost(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	skillFixture(t, st, base)

	top, err := st.TopSkillCost(ctx, ActivityFilter{GroupBy: []string{"skill"}}, ActivityByCost, 1)
	if err != nil {
		t.Fatalf("TopSkillCost: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("got %d rows, want 1", len(top))
	}
	if top[0].Keys["skill"] != "alpha" || top[0].CostMicroUSD != 15000 {
		t.Fatalf("top skill = %+v, want alpha at 15000", top[0])
	}

	if _, err := st.TopSkillCost(ctx, ActivityFilter{}, ActivityByCost, 1); err == nil {
		t.Fatal("TopSkillCost with no group dimension should refuse")
	}
}

// TestSkillCostRefusesCallLevelFilters covers the deliberate refusal. Honouring
// Kinds/Names would mean joining activity_events, at which point a turn with two
// matching calls joins twice and its cost doubles — the exact inflation this
// design removes. Ignoring them silently would answer a different question while
// looking obedient, so both are errors.
func TestSkillCostRefusesCallLevelFilters(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		f    ActivityFilter
	}{
		{"kinds", ActivityFilter{Kinds: []string{"tool"}}},
		{"names", ActivityFilter{Names: []string{"Bash"}}},
		{"groupby kind", ActivityFilter{GroupBy: []string{"kind"}}},
		{"groupby name", ActivityFilter{GroupBy: []string{"name"}}},
		{"groupby unknown", ActivityFilter{GroupBy: []string{"nope"}}},
	} {
		if _, err := st.SummarizeSkillCost(ctx, tc.f); err == nil {
			t.Fatalf("SummarizeSkillCost accepted %s, want refusal", tc.name)
		}
	}

	// The activity queries keep their full vocabulary: Skills is inert there,
	// not an error, because it names a dimension that table does not have.
	if _, err := st.SummarizeActivity(ctx, ActivityFilter{
		Kinds: []string{"tool"}, Names: []string{"Bash"}, Skills: []string{"alpha"},
	}); err != nil {
		t.Fatalf("SummarizeActivity must still accept its own filters: %v", err)
	}
}

// TestSkillContextAppendOnly holds the table to the same terms as the two
// ledgers: an observation of an immutable transcript line is never rewritten.
func TestSkillContextAppendOnly(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	skillFixture(t, st, base)

	if _, err := st.db.ExecContext(ctx,
		`UPDATE usage_skill_context SET skill='rewritten'`); err == nil {
		t.Fatal("UPDATE on usage_skill_context succeeded; the append-only trigger is missing")
	}
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM usage_skill_context`); err == nil {
		t.Fatal("DELETE on usage_skill_context succeeded; the append-only trigger is missing")
	}
}

// TestSkillContextRejectsEmptySkill checks the CHECK constraints reach the
// caller as a skip rather than being swallowed: ON CONFLICT DO NOTHING is
// scoped to the primary key, and a blanket OR IGNORE would have hidden this.
func TestSkillContextRejectsEmptySkill(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	e := ev("u-empty", model.ToolClaudeCode, base, 100)
	res, err := st.ApplyBatch(ctx, ObservationBatch{
		Events: []model.UsageEvent{e},
		SkillContexts: []model.SkillContext{
			{UsageDedupKey: "u-empty", Tool: model.ToolClaudeCode, Skill: "", EventTime: base},
			skillCtx("u-empty", "real", base),
		},
	})
	// The bad row is reported, not swallowed — and the good one still committed,
	// so one poison row cannot cost a batch that is re-derived every cycle.
	if err == nil {
		t.Fatal("empty skill was accepted; the CHECK constraint is missing or swallowed")
	}
	if !strings.Contains(err.Error(), "skipped 1 of 2") {
		t.Fatalf("error = %v, want a per-row skip report", err)
	}
	if res.SkillContexts != 1 {
		t.Fatalf("inserted %d contexts, want 1 (the empty skill must be skipped)", res.SkillContexts)
	}
	if res.Events != 1 {
		t.Fatalf("inserted %d events, want 1 (the batch must still commit)", res.Events)
	}
}

// TestMigrateV5ToV6CreatesSkillContext verifies the step against a database
// stamped at v5, and states the thing that cannot be fixed later: the table
// arrives EMPTY and no backfill is possible. The fact it records lives only on
// the source transcript — usage_events never carried it — and the no-UPDATE
// trigger forbids adding it to a stored row regardless. Usage already collected
// stays permanently unattributed to any skill.
func TestMigrateV5ToV6CreatesSkillContext(t *testing.T) {
	ctx := context.Background()
	st, err := Open(legacyDB(t, 5))
	if err != nil {
		t.Fatalf("migrate v5 database: %v", err)
	}
	defer st.Close()

	v, err := readSchemaVersion(ctx, st.db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("post-migration version = %d, want %d", v, SchemaVersion)
	}

	// The pre-existing ledger row survived untouched.
	events, err := st.ListEvents(ctx, Filter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].DedupKey != "legacy-1" || events[0].TotalTokens != 30 {
		t.Fatalf("ledger after migration = %+v, want the seeded legacy-1 row intact", events)
	}

	// The new table exists, is queryable, and is empty: the legacy row is in the
	// ledger with no skill context and will never gain one.
	sum, err := st.SummarizeSkillCost(ctx, ActivityFilter{})
	if err != nil {
		t.Fatalf("SummarizeSkillCost on a migrated database: %v", err)
	}
	if sum.Totals.Turns != 0 {
		t.Fatalf("migrated skill-context table holds %d turns, want 0 (v6 creates it empty)", sum.Totals.Turns)
	}

	// And it accepts new rows, so the migration produced a usable table rather
	// than merely a present one.
	e := ev("fresh-1", model.ToolClaudeCode, time.Unix(1750000100, 0).UTC(), 42)
	if _, err := st.ApplyBatch(ctx, ObservationBatch{
		Events:        []model.UsageEvent{e},
		SkillContexts: []model.SkillContext{skillCtx("fresh-1", "alpha", time.Unix(1750000100, 0).UTC())},
	}); err != nil {
		t.Fatalf("write to migrated skill-context table: %v", err)
	}
}

// TestSkillContextTableMatchesFreshSchema guards the drift two DDL copies always
// eventually produce: the v6 migration's table, indexes and triggers and
// schema.sql's must match, or a migrated database and a fresh one would carry
// different tables under the same version stamp.
func TestSkillContextTableMatchesFreshSchema(t *testing.T) {
	fresh := openTemp(t)
	migrated, err := Open(legacyDB(t, 5))
	if err != nil {
		t.Fatalf("migrate v5 db: %v", err)
	}
	defer migrated.Close()

	if a, b := skillColumnSpec(t, fresh), skillColumnSpec(t, migrated); a != b {
		t.Fatalf("usage_skill_context drifted between schema.sql and the v6 migration:\nfresh:    %s\nmigrated: %s", a, b)
	}
	if a, b := skillObjectSpec(t, fresh), skillObjectSpec(t, migrated); a != b {
		t.Fatalf("skill-context indexes/triggers drifted between schema.sql and the v6 migration:\nfresh:    %s\nmigrated: %s", a, b)
	}
}

// skillColumnSpec renders usage_skill_context's column names, types, NOT NULL
// flags and primary-key positions as one comparable string.
func skillColumnSpec(t *testing.T, st *SQLite) string {
	t.Helper()
	rows, err := st.db.Query(
		`SELECT name, type, "notnull", pk FROM pragma_table_info('usage_skill_context') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read skill context columns: %v", err)
	}
	defer rows.Close()
	var spec string
	for rows.Next() {
		var name, typ string
		var notNull, pk int
		if err := rows.Scan(&name, &typ, &notNull, &pk); err != nil {
			t.Fatalf("scan skill context column: %v", err)
		}
		spec += fmt.Sprintf("%s:%s:%d:%d;", name, typ, notNull, pk)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("skill context column rows: %v", err)
	}
	if spec == "" {
		t.Fatal("usage_skill_context has no columns")
	}
	// The primary key is the whole no-double-count argument; assert it rather
	// than trusting the comparison to notice its absence on both sides.
	if !strings.Contains(spec, "usage_dedup_key:TEXT:1:1;") {
		t.Fatalf("usage_dedup_key is not the NOT NULL primary key: %s", spec)
	}
	return spec
}

// skillObjectSpec renders the names of every index and trigger attached to
// usage_skill_context.
func skillObjectSpec(t *testing.T, st *SQLite) string {
	t.Helper()
	rows, err := st.db.Query(
		`SELECT type, name FROM sqlite_master WHERE tbl_name='usage_skill_context'
		 AND type IN ('index','trigger') ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read skill context objects: %v", err)
	}
	defer rows.Close()
	var spec string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("scan skill context object: %v", err)
		}
		spec += typ + ":" + name + ";"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("skill context object rows: %v", err)
	}
	return spec
}
