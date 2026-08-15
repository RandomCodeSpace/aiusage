// Skill-context cost attribution: what did a skill actually cost.
//
// usage_events answers "what did this turn cost"; activity_events answers "what
// did the agent call". Neither answers "what did skill X cost", and the reason
// is worth stating because it looks like it should.
//
// An activity row of kind=skill records the turn that INVOKED a skill — one
// call, one row. The work the skill then goes on to do is thousands of further
// turns, and none of them is a Skill call. Locally that gap is 44 invocation
// rows against 8,039 records of actual work: asking activity_events which skill
// is expensive returns the cost of pressing the button, not the cost of the
// thing the button started.
//
// The missing fact lives on the source record. Claude Code stamps every
// assistant record emitted while operating inside a skill with a top-level
// `attributionSkill` string. That is a property of the TURN, not an extra call
// within it, and the whole design follows from that one observation.
//
// WHY THIS CANNOT DOUBLE-COUNT.
//
// A usage row carries at most one skill context — the source field is a scalar,
// so two are not expressible — and this table makes that a database constraint
// rather than a hope: usage_dedup_key is the PRIMARY KEY. usage_events.dedup_key
// is likewise UNIQUE. The join below is therefore 1:1 in both directions and
// cannot multiply a row, so SUM(u.cost_micro_usd) over it adds each turn's cost
// AT MOST ONCE. There is no divisor here and nothing to share, because nothing
// competes: unlike the tool-call split, which divides one turn's cost among the
// calls that shared it, a skill context IS the turn.
//
// That is also why this is a separate table instead of another activity kind.
// Tool-call attribution and skill-context attribution are two different
// PARTITIONS of the same dollars — "which tool was called" and "which skill was
// running" — the way cost-by-region and cost-by-product are two views of one
// budget: each honest alone, meaningless added together. Had these rows lived in
// activity_events under a new kind, SummarizeActivity grouped by tool with no
// kind filter would have silently counted every skill turn twice, and the
// correctness of every existing caller would have rested on a WHERE clause
// nobody was told to write. In a separate table the sum is not discouraged, it
// is unexpressible: no query in this package reads both tables at once.
//
// WHY NOT A COLUMN ON activity_events. Because 41.8% of the fact would vanish.
// A turn can run under a skill and call no tool at all — thinking, planning,
// reading its own output — and it emits no activity row to hang a column on.
// Measured locally: 3,361 of 8,039 skill-context records carry zero tool_use
// blocks. Keying on the usage row instead of on a call captures those for free,
// because the usage row is what exists in every case.
//
// WHAT THIS DELIBERATELY DOES NOT DO. It does not compose with the activity
// ledger's Kinds/Names filters, and SummarizeSkillCost refuses them rather than
// ignoring them. "Skill cost among turns that called Bash" sounds reasonable and
// is a trap: reaching it means joining activity_events, at which point a turn
// with two Bash calls joins twice and its cost doubles. The refusal is the
// guard rail, not tidiness.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/RandomCodeSpace/aiusage/internal/model"
)

// skillContextTableDDL is usage_skill_context's CREATE TABLE as the v6 migration
// applies it. schema.sql carries the same table (it always describes the full
// latest schema) and TestSkillContextTableMatchesFreshSchema compares the two,
// so a migrated database and a fresh one can never carry different tables under
// one version stamp.
//
// usage_dedup_key is the PRIMARY KEY and that is the load-bearing line of the
// whole feature: it is what makes "at most one skill per turn" a constraint the
// database enforces instead of an invariant a reader has to trust. It is
// deliberately NOT a foreign key, for the same reason activity_events' join
// column is not: a context is an observed fact even when its usage row was
// skipped as a poison row, and the read path left-joins so a missing partner
// contributes no cost instead of losing the observation.
const skillContextTableDDL = `CREATE TABLE IF NOT EXISTS usage_skill_context (
  usage_dedup_key    TEXT    NOT NULL PRIMARY KEY,
  tool               TEXT    NOT NULL,
  skill              TEXT    NOT NULL,
  session_id         TEXT    NOT NULL DEFAULT '',
  project            TEXT    NOT NULL DEFAULT '',
  model              TEXT    NOT NULL DEFAULT '',
  event_time_unix    INTEGER NOT NULL,
  observed_time_unix INTEGER NOT NULL,
  source_path        TEXT    NOT NULL DEFAULT '',
  CHECK (usage_dedup_key <> ''),
  CHECK (skill <> '')
)`

// skillContextIndexDDL and skillContextTriggerDDL complete the v6 step. The
// triggers make this table append-only on the same terms as the two ledgers: a
// skill context is an observation of an immutable transcript line, not working
// state, so there is no legitimate reason to rewrite one.
var (
	skillContextIndexDDL = []string{
		`CREATE INDEX IF NOT EXISTS idx_skillctx_skill_time ON usage_skill_context(skill, event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_skillctx_event_time ON usage_skill_context(event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_skillctx_session    ON usage_skill_context(session_id)`,
	}
	skillContextTriggerDDL = []string{
		`CREATE TRIGGER IF NOT EXISTS trg_skillctx_no_update
BEFORE UPDATE ON usage_skill_context
BEGIN SELECT RAISE(ABORT, 'usage_skill_context is append-only: UPDATE forbidden'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_skillctx_no_delete
BEFORE DELETE ON usage_skill_context
BEGIN SELECT RAISE(ABORT, 'usage_skill_context is append-only: DELETE forbidden'); END`,
	}
)

// skillContextV6Statements is the ordered v6 migration body: table, then
// indexes, then triggers.
func skillContextV6Statements() []string {
	out := make([]string, 0, 1+len(skillContextIndexDDL)+len(skillContextTriggerDDL))
	out = append(out, skillContextTableDDL)
	out = append(out, skillContextIndexDDL...)
	out = append(out, skillContextTriggerDDL...)
	return out
}

// SkillCostBucket is one grouped row of skill-context cost.
//
// The token and cost fields carry NO "Attributed" prefix, and the omission is
// deliberate: unlike ActivityBucket's divided shares, these are the joined usage
// rows' FULL counts. Each turn belongs to exactly one skill, so no share is
// taken and none is lost. A bucket's cost is the real ledger cost of the turns
// that ran under that skill, not an estimate of it.
type SkillCostBucket struct {
	// Keys maps each GroupBy dimension to its value for this bucket
	// (e.g. {"skill":"adhd"}). Ordered via OrderedKeys.
	Keys        map[string]string
	OrderedKeys []string

	// Turns counts usage rows that ran under this skill context — one per
	// provider record, not per tool call.
	Turns int64
	// Sessions is the distinct non-empty session count within the group.
	// Distinct counts do not add across buckets.
	Sessions int64

	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	// CostMicroUSD sums the costs STAMPED on the joined usage rows. Turns whose
	// usage row carries no stamped cost contribute nothing and are counted in
	// UnpricedTurns instead, so a bucket with UnpricedTurns > 0 is an
	// understatement until those are display-priced.
	CostMicroUSD int64

	// UnjoinedTurns counts context rows whose usage row is absent from the
	// ledger — a poison row the insert skipped, or a ledger pruned under the
	// context. It should be zero in practice, since a context is only ever
	// emitted alongside an accepted usage event, and it is reported rather than
	// assumed because a silent zero would be indistinguishable from free.
	UnjoinedTurns int64
	// UnpricedTurns counts turns that DID join a usage row carrying no stamped
	// cost (collected before v3, or a model the price ladder could not price).
	// Their tokens are counted; their cost is not.
	UnpricedTurns int64
}

// SkillCostSummary is the result of SummarizeSkillCost: grouped buckets plus a
// grand total.
type SkillCostSummary struct {
	GroupBy []string
	Buckets []SkillCostBucket
	Totals  SkillCostBucket
}

// skillCostSelectSQL is the metric half of both queries' select list, in the
// order querySkillCostBuckets scans it. Note the absence of any division: see the
// package comment on why a skill context has no divisor.
const skillCostSelectSQL = `COUNT(*),
	COUNT(DISTINCT CASE WHEN c.session_id <> '' THEN c.session_id END),
	COALESCE(SUM(u.input_tokens),0),
	COALESCE(SUM(u.output_tokens),0),
	COALESCE(SUM(u.total_tokens),0),
	COALESCE(SUM(u.cost_micro_usd),0),
	COALESCE(SUM(CASE WHEN u.dedup_key IS NULL THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN u.dedup_key IS NOT NULL AND u.cost_micro_usd IS NULL THEN 1 ELSE 0 END),0)`

// skillCostFromSQL is the joined source. Both sides of the ON are unique keys —
// usage_skill_context's PRIMARY KEY and usage_events' UNIQUE dedup_key — so this
// join is 1:1 and structurally incapable of counting a turn twice.
const skillCostFromSQL = ` FROM usage_skill_context c
	LEFT JOIN usage_events u ON u.dedup_key = c.usage_dedup_key`

// skillCostOrderExpr maps a rank metric to its SQL ordering expression, reusing
// ActivityOrder's vocabulary. ActivityByCalls ranks by TURNS here: a skill
// context counts turns, and there is no per-call row to count.
func skillCostOrderExpr(o ActivityOrder) (string, error) {
	switch o {
	case ActivityByCalls, "":
		return "COUNT(*)", nil
	case ActivityByCost:
		return "COALESCE(SUM(u.cost_micro_usd),0)", nil
	case ActivityByTokens:
		return "COALESCE(SUM(u.total_tokens),0)", nil
	default:
		return "", fmt.Errorf("store: invalid activity order %q", o)
	}
}

// skillGroupExpr maps a GroupBy dimension to its SQL select/group expression.
// The time dimensions use the SAME layouts groupExpr and activityGroupExpr use,
// so a skill bucket key and a usage bucket key mean the same thing and the two
// can be put side by side. As there, 'localtime' follows the SYSTEM timezone
// rather than Go's time.Local.
//
// "kind" and "name" are absent on purpose and are refused by name below: they
// are activity_events dimensions, and this table holds no calls to group by.
func skillGroupExpr(dim string) (string, error) {
	switch dim {
	case "hour":
		return "strftime('%Y-%m-%d %H', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "day":
		return "strftime('%Y-%m-%d', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "week":
		return "strftime('%Y-W%W', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "month":
		return "strftime('%Y-%m', c.event_time_unix, 'unixepoch', 'localtime')", nil
	case "tool":
		return "c.tool", nil
	case "skill":
		return "c.skill", nil
	case "project":
		return "c.project", nil
	case "session":
		return "c.session_id", nil
	case "model":
		return "c.model", nil
	case "kind", "name":
		return "", fmt.Errorf("store: %q groups activity calls, not skill contexts; group by \"skill\" instead", dim)
	default:
		return "", fmt.Errorf("store: invalid skill-cost group dimension %q", dim)
	}
}

// skillGroupExprs resolves every dimension of a filter, refusing the whole query
// on the first unusable one.
func skillGroupExprs(f ActivityFilter) ([]string, error) {
	out := make([]string, 0, len(f.GroupBy))
	for _, dim := range f.GroupBy {
		expr, err := skillGroupExpr(dim)
		if err != nil {
			return nil, err
		}
		out = append(out, expr)
	}
	return out, nil
}

// checkSkillFilter refuses the activity-only dimensions of an ActivityFilter
// rather than ignoring them. Silently dropping a filter the caller set would
// answer a different question than the one asked while looking like it had
// obeyed, and in this particular case honouring them would require a join to
// activity_events that re-introduces the multi-call double count the whole
// design exists to prevent.
func checkSkillFilter(f ActivityFilter) error {
	if len(f.Kinds) > 0 {
		return fmt.Errorf("store: ActivityFilter.Kinds selects activity kinds and cannot restrict skill contexts; drop it")
	}
	if len(f.Names) > 0 {
		return fmt.Errorf("store: ActivityFilter.Names selects tool/skill/hook call names and cannot restrict skill contexts; use Skills instead")
	}
	return nil
}

// buildSkillWhere builds the WHERE clause (with a leading " WHERE " when
// non-empty) and the positional args for an ActivityFilter, over the context
// table's own columns.
func buildSkillWhere(f ActivityFilter) (string, []any) {
	var conds []string
	var args []any

	if !f.Since.IsZero() {
		conds = append(conds, "c.event_time_unix >= ?")
		args = append(args, f.Since.UTC().Unix())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "c.event_time_unix < ?")
		args = append(args, f.Until.UTC().Unix())
	}

	addIn := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		conds = append(conds, col+" IN ("+strings.Join(ph, ",")+")")
	}
	addIn("c.tool", f.Tools)
	addIn("c.skill", f.Skills)
	addIn("c.project", f.Projects)
	addIn("c.session_id", f.Sessions)
	addIn("c.model", f.Models)

	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// SummarizeSkillCost aggregates the cost of the turns that ran under a skill,
// grouped per GroupBy. See Store.SummarizeSkillCost.
func (s *SQLite) SummarizeSkillCost(ctx context.Context, f ActivityFilter) (*SkillCostSummary, error) {
	if err := checkSkillFilter(f); err != nil {
		return nil, err
	}
	groupExprs, err := skillGroupExprs(f)
	if err != nil {
		return nil, err
	}
	where, args := buildSkillWhere(f)

	var sb strings.Builder
	sb.WriteString("SELECT ")
	for _, ge := range groupExprs {
		sb.WriteString(ge)
		sb.WriteString(", ")
	}
	sb.WriteString(skillCostSelectSQL)
	sb.WriteString(skillCostFromSQL)
	sb.WriteString(where)
	if len(groupExprs) > 0 {
		joined := strings.Join(groupExprs, ", ")
		sb.WriteString(" GROUP BY " + joined + " ORDER BY " + joined)
	}

	buckets, err := s.querySkillCostBuckets(ctx, sb.String(), args, f.GroupBy)
	if err != nil {
		return nil, err
	}

	sum := &SkillCostSummary{GroupBy: append([]string{}, f.GroupBy...), Buckets: buckets}

	// Grand total from the single pass, exactly as SummarizeActivity does it:
	// ungrouped, the one row IS the total; grouped, every field adds across
	// buckets except the distinct session count, which needs its own query.
	if len(f.GroupBy) == 0 {
		if len(buckets) == 1 {
			sum.Totals = buckets[0]
		}
		return sum, nil
	}
	for _, b := range buckets {
		sum.Totals.Turns += b.Turns
		sum.Totals.InputTokens += b.InputTokens
		sum.Totals.OutputTokens += b.OutputTokens
		sum.Totals.TotalTokens += b.TotalTokens
		sum.Totals.CostMicroUSD += b.CostMicroUSD
		sum.Totals.UnjoinedTurns += b.UnjoinedTurns
		sum.Totals.UnpricedTurns += b.UnpricedTurns
	}
	if len(buckets) > 0 {
		n, err := s.distinctSkillSessions(ctx, where, args)
		if err != nil {
			return nil, err
		}
		sum.Totals.Sessions = n
	}
	return sum, nil
}

// TopSkillCost ranks the grouped buckets by one metric and returns at most limit
// of them — the real "which skill is expensive" query. See Store.TopSkillCost.
func (s *SQLite) TopSkillCost(ctx context.Context, f ActivityFilter, by ActivityOrder, limit int) ([]SkillCostBucket, error) {
	if err := checkSkillFilter(f); err != nil {
		return nil, err
	}
	groupExprs, err := skillGroupExprs(f)
	if err != nil {
		return nil, err
	}
	if len(groupExprs) == 0 {
		return nil, fmt.Errorf("store: TopSkillCost needs at least one group dimension to rank")
	}
	order, err := skillCostOrderExpr(by)
	if err != nil {
		return nil, err
	}
	where, args := buildSkillWhere(f)

	joined := strings.Join(groupExprs, ", ")
	q := "SELECT " + joined + ", " + skillCostSelectSQL + skillCostFromSQL + where +
		" GROUP BY " + joined +
		// The group expressions break ties, so a ranking is deterministic
		// rather than at the mercy of scan order.
		" ORDER BY " + order + " DESC, " + joined
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return s.querySkillCostBuckets(ctx, q, args, f.GroupBy)
}

// querySkillCostBuckets runs a built skill-cost query and scans its rows. Both
// public queries share it so their projections cannot drift apart.
func (s *SQLite) querySkillCostBuckets(ctx context.Context, q string, args []any, groupBy []string) ([]SkillCostBucket, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: summarize skill cost: %w", err)
	}
	defer rows.Close()

	var out []SkillCostBucket
	for rows.Next() {
		keyVals := make([]string, len(groupBy))
		dest := make([]any, 0, len(groupBy)+8)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var b SkillCostBucket
		dest = append(dest, &b.Turns, &b.Sessions,
			&b.InputTokens, &b.OutputTokens, &b.TotalTokens,
			&b.CostMicroUSD, &b.UnjoinedTurns, &b.UnpricedTurns)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan skill cost row: %w", err)
		}
		if len(groupBy) > 0 {
			b.Keys = make(map[string]string, len(groupBy))
			b.OrderedKeys = append([]string{}, groupBy...)
			for i, dim := range groupBy {
				b.Keys[dim] = keyVals[i]
			}
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: skill cost rows: %w", err)
	}
	return out, nil
}

// distinctSkillSessions counts distinct non-empty session ids over the filtered
// context set.
func (s *SQLite) distinctSkillSessions(ctx context.Context, where string, args []any) (int64, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN c.session_id <> '' THEN c.session_id END)`+
		skillCostFromSQL+where, args...)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: distinct skill sessions: %w", err)
	}
	return n, nil
}

// insertSkillContextsTx appends skill-context rows inside an existing
// transaction, with the same idempotence the two ledgers get: ON
// CONFLICT(usage_dedup_key) DO NOTHING keeps a re-read silent while a CHECK
// violation (empty key, empty skill) still errors, which a blanket OR IGNORE
// would swallow. Per-row failures are skipped and summarised in skipErr so one
// poison row cannot abort a batch that is re-derived every cycle.
//
// The conflict target is the usage row itself, which is what makes a second
// sighting of the same turn a no-op rather than a second helping of its cost.
func insertSkillContextsTx(ctx context.Context, tx *sql.Tx, ctxs []model.SkillContext) (inserted int, skipErr, err error) {
	if len(ctxs) == 0 {
		return 0, nil, nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_skill_context (
			usage_dedup_key, tool, skill, session_id, project, model,
			event_time_unix, observed_time_unix, source_path
		) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(usage_dedup_key) DO NOTHING`)
	if err != nil {
		return 0, nil, fmt.Errorf("store: prepare skill context insert: %w", err)
	}
	defer stmt.Close()

	var (
		skipped   int
		firstSkip error
	)
	skip := func(rowErr error) {
		skipped++
		if firstSkip == nil {
			firstSkip = rowErr
		}
	}
	for _, c := range ctxs {
		if err := ctx.Err(); err != nil {
			return inserted, nil, err
		}
		if c.UsageDedupKey == "" {
			skip(fmt.Errorf("store: skill context with empty usage dedup key (tool=%s skill=%s)", c.Tool, c.Skill))
			continue
		}
		obs := c.ObservedTime
		if obs.IsZero() {
			obs = c.EventTime
		}
		res, execErr := stmt.ExecContext(ctx,
			c.UsageDedupKey, c.Tool, c.Skill, c.SessionID, c.Project, c.Model,
			c.EventTime.UTC().Unix(), obs.UTC().Unix(), c.SourcePath,
		)
		if execErr != nil {
			skip(fmt.Errorf("store: insert skill context %s: %w", c.UsageDedupKey, execErr))
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if skipped > 0 {
		return inserted, fmt.Errorf("store: skipped %d of %d skill context row(s); first: %w", skipped, len(ctxs), firstSkip), nil
	}
	return inserted, nil, nil
}
