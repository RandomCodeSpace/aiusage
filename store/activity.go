// Agent activity: the second append-only ledger. usage_events answers "what did
// this cost"; activity_events answers "what did the agent actually do" — which
// tool was called, which skill was invoked, which hook fired, how many times.
//
// The two are joined, never merged. An activity row stores no token counts at
// all: it names the usage_events row whose provider record contained the call
// (usage_dedup_key), and the read path below derives tokens and cost from the
// ledger by dividing that row's total between the calls that share it. See
// model.ActivityEvent for why that shape is the honest one, and
// activityDivisorSQL for why the divisor is counted in the table rather than
// read from the calls_in_turn each adapter stamped.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
)

// activityTableDDL is activity_events' CREATE TABLE as the v5 migration applies
// it. schema.sql carries the same table (it always describes the full latest
// schema) and TestActivityTableMatchesFreshSchema compares the two, so a
// migrated database and a fresh one can never carry different tables under one
// version stamp.
const activityTableDDL = `CREATE TABLE IF NOT EXISTS activity_events (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  dedup_key          TEXT    NOT NULL UNIQUE,
  tool               TEXT    NOT NULL,
  kind               TEXT    NOT NULL,
  name               TEXT    NOT NULL,
  session_id         TEXT    NOT NULL DEFAULT '',
  project            TEXT    NOT NULL DEFAULT '',
  model              TEXT    NOT NULL DEFAULT '',
  event_time_unix    INTEGER NOT NULL,
  observed_time_unix INTEGER NOT NULL,
  usage_dedup_key    TEXT    NOT NULL DEFAULT '',
  message_id         TEXT    NOT NULL DEFAULT '',
  request_id         TEXT    NOT NULL DEFAULT '',
  turn_seq           INTEGER NOT NULL DEFAULT 0,
  calls_in_turn      INTEGER NOT NULL DEFAULT 1,
  source_path        TEXT    NOT NULL DEFAULT '',
  CHECK (kind IN ('tool','skill','hook')),
  CHECK (name <> ''),
  CHECK (turn_seq >= 0 AND calls_in_turn >= 1 AND turn_seq < calls_in_turn)
)`

// activityIndexDDL and activityTriggerDDL complete the v5 step. The triggers are
// what make this table append-only on the same terms as usage_events; the
// indexes serve the four questions the activity surface asks (frequency over a
// window, per (tool, name), per session, and the cost join).
var (
	activityIndexDDL = []string{
		`CREATE INDEX IF NOT EXISTS idx_activity_event_time ON activity_events(event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_activity_tool_name  ON activity_events(tool, name)`,
		`CREATE INDEX IF NOT EXISTS idx_activity_name_time  ON activity_events(name, event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_activity_kind_time  ON activity_events(kind, event_time_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_activity_session    ON activity_events(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_activity_usage_key  ON activity_events(usage_dedup_key)`,
	}
	activityTriggerDDL = []string{
		`CREATE TRIGGER IF NOT EXISTS trg_activity_no_update
BEFORE UPDATE ON activity_events
BEGIN SELECT RAISE(ABORT, 'activity_events is append-only: UPDATE forbidden'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_activity_no_delete
BEFORE DELETE ON activity_events
BEGIN SELECT RAISE(ABORT, 'activity_events is append-only: DELETE forbidden'); END`,
	}
)

// activityV5Statements is the ordered v5 migration body: table, then indexes,
// then triggers.
func activityV5Statements() []string {
	out := make([]string, 0, 1+len(activityIndexDDL)+len(activityTriggerDDL))
	out = append(out, activityTableDDL)
	out = append(out, activityIndexDDL...)
	out = append(out, activityTriggerDDL...)
	return out
}

// ActivityFilter selects and groups agent activity for reporting. It mirrors
// Filter's vocabulary so a surface can carry one set of crumbs across both
// ledgers, minus the dimensions activity has no column for (provider, service
// tier) and plus the two it introduces (kind, name).
type ActivityFilter struct {
	Since time.Time // inclusive lower bound on event_time (zero = open)
	Until time.Time // exclusive upper bound on event_time (zero = open)

	Tools    []string // restrict to these agent CLIs (empty = all)
	Kinds    []string // restrict to these kinds: tool|skill|hook (empty = all)
	Names    []string // restrict to these tool/skill/hook names (empty = all)
	Projects []string // restrict to these projects (empty = all)
	Sessions []string // restrict to these sessions (empty = all)
	Models   []string // restrict to these models (empty = all)
	// Values restricts the TURN-CONTEXT queries to these context values (empty =
	// all) — agent types, skill names, MCP server or tool names, plugin names,
	// whichever dimension the query named. It is ignored by
	// SummarizeActivity/TopActivity/ListActivity, which read activity_events and
	// have no turn-context column: a turn context is a property of the turn,
	// recorded in usage_turn_context. See SummarizeTurnContext and the
	// turncontext.go package comment.
	Values []string
	// Skills is the pre-generalisation spelling of Values, kept for callers that
	// predate the other four dimensions. It restricts the SKILL dimension and is
	// REFUSED on any other, rather than quietly filtering agent names against a
	// list of skills and returning an empty result that reads as "that agent
	// cost nothing".
	Skills []string

	// GroupBy lists grouping dimensions, applied in order. Valid values:
	// "hour","day","week","month","tool","kind","name","project","session",
	// "model". Empty means a single grand-total bucket.
	GroupBy []string
}

// ActivityBucket is one grouped row of summarised activity.
//
// The Attributed* fields are this call's SHARE of the tokens its turn actually
// cost, taken from usage_events and divided between the calls that name it (see
// activityDivisorSQL). The division is integer, so a turn's shares sum to at
// most the turn's real total — the split can only ever UNDERSTATE, never
// inflate, which is the direction an attribution guess is allowed to be wrong
// in.
type ActivityBucket struct {
	// Keys maps each GroupBy dimension to its value for this bucket
	// (e.g. {"name":"Bash","tool":"claude-code"}). Ordered via OrderedKeys.
	Keys        map[string]string
	OrderedKeys []string

	// Calls counts invocations in the bucket. It is the frequency answer and is
	// never affected by attribution: a call with no joinable usage row is still
	// a call that happened.
	Calls int64
	// Sessions is the distinct non-empty session count within the group.
	// Distinct counts do not add across buckets.
	Sessions int64

	AttributedInput  int64
	AttributedOutput int64
	AttributedTotal  int64
	// AttributedCostMicroUSD sums the shares of costs STAMPED on the joined
	// usage rows. Calls whose usage row carries no stamped cost contribute
	// nothing and are counted in UnpricedCalls instead, so a bucket with
	// UnpricedCalls > 0 is an understatement until those are display-priced.
	AttributedCostMicroUSD int64

	// UnattributedCalls counts calls with NO joinable usage row: the source
	// records its calls and its token counts in unrelated records (codex), the
	// call carries no usage at all (hooks), or the partner row predates
	// activity collection. Their tokens are not missing, they are unknowable
	// from this table — reading a zero share as "free" would be the lie this
	// count exists to prevent.
	UnattributedCalls int64
	// UnpricedCalls counts calls that DID join a usage row which carries no
	// stamped cost (collected before v3, or a model the price ladder could not
	// price). Their tokens are attributed; their cost is not.
	UnpricedCalls int64
	// ComputedCostCalls counts calls whose joined usage row was priced from a
	// public rate card rather than by the harness (model.PriceProvenance). Zero
	// means every dollar in AttributedCostMicroUSD traces to a vendor's own
	// number. It counts CALLS, not usage rows: several calls sharing one turn
	// each carry that turn's provenance, which is the level the figure they
	// qualify is summed at.
	ComputedCostCalls int64
}

// ActivitySummary is the result of SummarizeActivity: grouped buckets plus a
// grand total.
type ActivitySummary struct {
	GroupBy []string
	Buckets []ActivityBucket
	Totals  ActivityBucket
}

// ActivityOrder names the metric TopActivity ranks by.
type ActivityOrder string

const (
	// ActivityByCalls ranks by invocation count — "what do I call most".
	ActivityByCalls ActivityOrder = "calls"
	// ActivityByCost ranks by attributed cost — "which skill is expensive".
	ActivityByCost ActivityOrder = "cost"
	// ActivityByTokens ranks by attributed total tokens, the answer to the same
	// question on a ledger whose rows were never priced.
	ActivityByTokens ActivityOrder = "tokens"
)

// orderExpr maps a rank metric to its SQL ordering expression. The expressions
// are the same aggregates the select list computes.
func (o ActivityOrder) orderExpr() (string, error) {
	switch o {
	case ActivityByCalls, "":
		return "COUNT(*)", nil
	case ActivityByCost:
		return activityCostSQL, nil
	case ActivityByTokens:
		return activityTokensSQL, nil
	default:
		return "", fmt.Errorf("store: invalid activity order %q", o)
	}
}

// activityDivisorSQL is the cost split's denominator: how many activity rows
// actually share this call's usage row, COUNTED IN THE LEDGER rather than read
// from the calls_in_turn stamp the adapter wrote.
//
// The stamp is what an adapter believed at insert time, and an append-only
// table cannot correct a belief that later turns out to be low. Claude Code
// streams one response across several transcript records, and until the union
// in the claudecode deduper each of those records contributed a row stamped
// calls_in_turn=1 for a turn that had three; the missing rows can be appended
// afterwards, but the rows already stored keep their 1 forever. Reading the
// stamp would then divide one turn's tokens by 1, 3 and 3 and attribute 167% of
// it — an OVERSTATEMENT, the one direction this table promises never to go.
//
// Counting the rows removes the possibility instead of documenting it. The
// divisor is always exactly the number of rows summing over that usage row, so
// integer division makes the shares sum to AT MOST the turn's real total by
// construction, whatever any adapter stamped, in whatever order the rows
// landed, however many passes it took. It is the same rule the rollup lives
// under: derived on read, never authoritative on disk. calls_in_turn stays in
// the table as what the source reported, which is worth keeping and is not the
// same claim.
//
// The ” case never joins a usage row (LEFT JOIN gives NULL and the sums skip
// it), so it short-circuits to 1 rather than counting every unattributed row in
// the table. idx_activity_usage_key serves the lookup.
const activityDivisorSQL = `(CASE WHEN a.usage_dedup_key = '' THEN 1 ELSE
	(SELECT COUNT(*) FROM activity_events s WHERE s.usage_dedup_key = a.usage_dedup_key) END)`

// The attribution expressions, in one place so SummarizeActivity and
// TopActivity can never disagree about what "attributed" means. Integer
// division floors (every operand is non-negative), so a turn's shares sum to
// at most its real total.
const (
	activityCostSQL   = `COALESCE(SUM(u.cost_micro_usd / ` + activityDivisorSQL + `),0)`
	activityTokensSQL = `COALESCE(SUM(u.total_tokens / ` + activityDivisorSQL + `),0)`
)

// activitySelectSQL is the metric half of both queries' select list, in the
// order scanActivityBucket reads it.
//
// It is a var rather than a const only because its last column is assembled
// from model's vendor price-source vocabulary at init (computedCostCountSQL);
// nothing writes to it after that.
var activitySelectSQL = `COUNT(*),
	COUNT(DISTINCT CASE WHEN a.session_id <> '' THEN a.session_id END),
	COALESCE(SUM(u.input_tokens / ` + activityDivisorSQL + `),0),
	COALESCE(SUM(u.output_tokens / ` + activityDivisorSQL + `),0),
	` + activityTokensSQL + `,
	` + activityCostSQL + `,
	COALESCE(SUM(CASE WHEN u.dedup_key IS NULL THEN 1 ELSE 0 END),0),
	COALESCE(SUM(CASE WHEN u.dedup_key IS NOT NULL AND u.cost_micro_usd IS NULL THEN 1 ELSE 0 END),0),
	` + computedCostCountSQL("u.cost_micro_usd", "u.price_source")

// activityFromSQL is the joined source. The LEFT JOIN is load-bearing: rows
// whose usage_dedup_key is ” (codex calls, hooks) match nothing — ” is never
// a usage_events dedup key, since insertEventsTx rejects an empty one — and a
// LEFT JOIN keeps the call while contributing no tokens, where an inner join
// would silently drop it from the frequency answer too.
const activityFromSQL = ` FROM activity_events a
	LEFT JOIN usage_events u ON u.dedup_key = a.usage_dedup_key`

// buildActivityWhere builds the WHERE clause (with a leading " WHERE " when
// non-empty) and the positional args for an ActivityFilter. Columns are
// qualified with the activity alias because every query joins the ledger.
func buildActivityWhere(f ActivityFilter) (string, []any) {
	var conds []string
	var args []any

	if !f.Since.IsZero() {
		conds = append(conds, "a.event_time_unix >= ?")
		args = append(args, f.Since.UTC().Unix())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "a.event_time_unix < ?")
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
	addIn("a.tool", f.Tools)
	addIn("a.kind", f.Kinds)
	addIn("a.name", f.Names)
	addIn("a.project", f.Projects)
	addIn("a.session_id", f.Sessions)
	addIn("a.model", f.Models)

	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// activityGroupExpr maps a GroupBy dimension to its SQL select/group
// expression. Time dimensions fold to local wall clock with the SAME layouts
// groupExpr uses on usage_events, so an activity bucket key and a usage bucket
// key mean the same thing and the two ledgers can be put side by side. As
// there, 'localtime' follows the SYSTEM timezone rather than Go's time.Local.
func activityGroupExpr(dim string) (string, error) {
	switch dim {
	case "hour":
		return "strftime('%Y-%m-%d %H', a.event_time_unix, 'unixepoch', 'localtime')", nil
	case "day":
		return "strftime('%Y-%m-%d', a.event_time_unix, 'unixepoch', 'localtime')", nil
	case "week":
		return "strftime('%Y-W%W', a.event_time_unix, 'unixepoch', 'localtime')", nil
	case "month":
		return "strftime('%Y-%m', a.event_time_unix, 'unixepoch', 'localtime')", nil
	case "tool":
		return "a.tool", nil
	case "kind":
		return "a.kind", nil
	case "name":
		return "a.name", nil
	case "project":
		return "a.project", nil
	case "session":
		return "a.session_id", nil
	case "model":
		return "a.model", nil
	default:
		return "", fmt.Errorf("store: invalid activity group dimension %q", dim)
	}
}

// activityGroupExprs resolves every dimension of a filter, refusing the whole
// query on the first unknown one.
func activityGroupExprs(f ActivityFilter) ([]string, error) {
	out := make([]string, 0, len(f.GroupBy))
	for _, dim := range f.GroupBy {
		expr, err := activityGroupExpr(dim)
		if err != nil {
			return nil, err
		}
		out = append(out, expr)
	}
	return out, nil
}

// SummarizeActivity aggregates agent activity (tool calls, skill invocations,
// hook firings) matching ActivityFilter, grouped per its GroupBy, with tokens
// and cost ATTRIBUTED from the ledger: each call takes its joined usage row's
// counts divided by the number of calls that shared that turn. The division is
// integer and every operand non-negative, so the attributed total over any
// window is at most the same window's usage_events total — the split can
// understate, never inflate.
func (s *Reader) SummarizeActivity(ctx context.Context, f ActivityFilter) (*ActivitySummary, error) {
	groupExprs, err := activityGroupExprs(f)
	if err != nil {
		return nil, err
	}
	where, args := buildActivityWhere(f)

	var sb strings.Builder
	sb.WriteString("SELECT ")
	for _, ge := range groupExprs {
		sb.WriteString(ge)
		sb.WriteString(", ")
	}
	sb.WriteString(activitySelectSQL)
	sb.WriteString(activityFromSQL)
	sb.WriteString(where)
	if len(groupExprs) > 0 {
		joined := strings.Join(groupExprs, ", ")
		sb.WriteString(" GROUP BY " + joined + " ORDER BY " + joined)
	}

	buckets, err := s.queryActivityBuckets(ctx, sb.String(), args, f.GroupBy)
	if err != nil {
		return nil, err
	}

	sum := &ActivitySummary{GroupBy: append([]string{}, f.GroupBy...), Buckets: buckets}

	// Grand total from the single pass, exactly as Summarize does it: ungrouped,
	// the one row IS the total; grouped, every field adds across buckets except
	// the distinct session count, which needs its own narrow query.
	if len(f.GroupBy) == 0 {
		if len(buckets) == 1 {
			sum.Totals = buckets[0]
		}
		return sum, nil
	}
	for _, b := range buckets {
		sum.Totals.Calls += b.Calls
		sum.Totals.AttributedInput += b.AttributedInput
		sum.Totals.AttributedOutput += b.AttributedOutput
		sum.Totals.AttributedTotal += b.AttributedTotal
		sum.Totals.AttributedCostMicroUSD += b.AttributedCostMicroUSD
		sum.Totals.UnattributedCalls += b.UnattributedCalls
		sum.Totals.UnpricedCalls += b.UnpricedCalls
		sum.Totals.ComputedCostCalls += b.ComputedCostCalls
	}
	if len(buckets) > 0 {
		n, err := s.distinctActivitySessions(ctx, where, args)
		if err != nil {
			return nil, err
		}
		sum.Totals.Sessions = n
	}
	return sum, nil
}

// TopActivity ranks SummarizeActivity's grouped buckets by one metric and
// returns at most limit of them (0 = uncapped) — the "which skill is expensive"
// query, ordered and capped in SQL so a caller never materialises the whole
// vocabulary to show ten rows. It needs at least one GroupBy dimension: ranking
// a single grand-total bucket is not a ranking.
func (s *Reader) TopActivity(ctx context.Context, f ActivityFilter, by ActivityOrder, limit int) ([]ActivityBucket, error) {
	groupExprs, err := activityGroupExprs(f)
	if err != nil {
		return nil, err
	}
	if len(groupExprs) == 0 {
		return nil, fmt.Errorf("store: TopActivity needs at least one group dimension to rank")
	}
	order, err := by.orderExpr()
	if err != nil {
		return nil, err
	}
	where, args := buildActivityWhere(f)

	joined := strings.Join(groupExprs, ", ")
	q := "SELECT " + joined + ", " + activitySelectSQL + activityFromSQL + where +
		" GROUP BY " + joined +
		// The group expressions break ties, so a ranking is deterministic
		// rather than at the mercy of scan order.
		" ORDER BY " + order + " DESC, " + joined
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return s.queryActivityBuckets(ctx, q, args, f.GroupBy)
}

// queryActivityBuckets runs a built activity query and scans its rows. Both
// public queries share it so their projections cannot drift apart.
func (s *Reader) queryActivityBuckets(ctx context.Context, q string, args []any, groupBy []string) ([]ActivityBucket, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: summarize activity: %w", err)
	}
	defer rows.Close()

	var out []ActivityBucket
	for rows.Next() {
		keyVals := make([]string, len(groupBy))
		dest := make([]any, 0, len(groupBy)+8)
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var b ActivityBucket
		dest = append(dest, &b.Calls, &b.Sessions,
			&b.AttributedInput, &b.AttributedOutput, &b.AttributedTotal,
			&b.AttributedCostMicroUSD, &b.UnattributedCalls, &b.UnpricedCalls,
			&b.ComputedCostCalls)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scan activity row: %w", err)
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
		return nil, fmt.Errorf("store: activity rows: %w", err)
	}
	return out, nil
}

// distinctActivitySessions counts distinct non-empty session ids over the
// filtered activity set.
func (s *Reader) distinctActivitySessions(ctx context.Context, where string, args []any) (int64, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN a.session_id <> '' THEN a.session_id END)`+
		activityFromSQL+where, args...)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: distinct activity sessions: %w", err)
	}
	return n, nil
}

// insertActivityTx appends activity rows inside an existing transaction, with
// the same idempotence usage_events gets: ON CONFLICT(dedup_key) DO NOTHING
// keeps a re-read silent while a CHECK violation (unknown kind, empty name,
// turn_seq past calls_in_turn) still errors, which a blanket OR IGNORE would
// swallow. Per-row failures are skipped and summarised in skipErr so one poison
// row cannot abort a batch that is re-derived every cycle; err is reserved for
// failures of the batch itself.
func insertActivityTx(ctx context.Context, tx *sql.Tx, acts []model.ActivityEvent) (inserted int, skipErr, err error) {
	if len(acts) == 0 {
		return 0, nil, nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO activity_events (
			dedup_key, tool, kind, name, session_id, project, model,
			event_time_unix, observed_time_unix,
			usage_dedup_key, message_id, request_id,
			turn_seq, calls_in_turn, source_path
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(dedup_key) DO NOTHING`)
	if err != nil {
		return 0, nil, fmt.Errorf("store: prepare activity insert: %w", err)
	}
	defer stmt.Close()

	skips := rowSkips{table: tableActivityEvents}
	for _, a := range acts {
		if err := ctx.Err(); err != nil {
			return inserted, nil, err
		}
		if a.DedupKey == "" {
			skips.add(a.DedupKey, fmt.Errorf("store: activity with empty dedup key (tool=%s name=%s)", a.Tool, a.Name))
			continue
		}
		calls := a.CallsInTurn
		if calls < 1 {
			calls = 1
		}
		res, execErr := stmt.ExecContext(ctx,
			a.DedupKey, a.Tool, string(a.Kind), a.Name, a.SessionID, a.Project, a.Model,
			a.EventTime.UTC().Unix(), activityObservedUnix(a),
			a.UsageDedupKey, a.MessageID, a.RequestID,
			a.TurnSeq, calls, a.SourcePath,
		)
		if execErr != nil {
			skips.add(a.DedupKey, fmt.Errorf("store: insert activity %s: %w", a.DedupKey, execErr))
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, skips.err(len(acts)), nil
}

// activityObservedUnix returns the observed timestamp in UTC seconds, falling
// back to the event time when ObservedTime is unset.
func activityObservedUnix(a model.ActivityEvent) int64 {
	if a.ObservedTime.IsZero() {
		return a.EventTime.UTC().Unix()
	}
	return a.ObservedTime.UTC().Unix()
}

// ListActivity returns activity rows matching the filter, ordered by event time
// then row id. It exists for tests and for a future export; the reporting
// surfaces group instead of listing.
func (s *Reader) ListActivity(ctx context.Context, f ActivityFilter) ([]model.ActivityEvent, error) {
	where, args := buildActivityWhere(f)
	// The alias is kept so buildActivityWhere's qualified columns resolve; no
	// join is needed to list.
	q := `SELECT a.id, a.dedup_key, a.tool, a.kind, a.name, a.session_id, a.project, a.model,
		a.event_time_unix, a.observed_time_unix, a.usage_dedup_key, a.message_id, a.request_id,
		a.turn_seq, a.calls_in_turn, a.source_path
		FROM activity_events a` + where + ` ORDER BY a.event_time_unix, a.id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list activity: %w", err)
	}
	defer rows.Close()

	var out []model.ActivityEvent
	for rows.Next() {
		var (
			a                  model.ActivityEvent
			kind               string
			eventUnix, obsUnix int64
		)
		if err := rows.Scan(&a.ID, &a.DedupKey, &a.Tool, &kind, &a.Name, &a.SessionID, &a.Project, &a.Model,
			&eventUnix, &obsUnix, &a.UsageDedupKey, &a.MessageID, &a.RequestID,
			&a.TurnSeq, &a.CallsInTurn, &a.SourcePath); err != nil {
			return nil, fmt.Errorf("store: scan activity: %w", err)
		}
		a.Kind = model.ActivityKind(kind)
		a.EventTime = time.Unix(eventUnix, 0).UTC()
		a.ObservedTime = time.Unix(obsUnix, 0).UTC()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: activity list rows: %w", err)
	}
	return out, nil
}
