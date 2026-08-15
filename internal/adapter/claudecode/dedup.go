package claudecode

import "github.com/RandomCodeSpace/aiusage/internal/model"

// deduper implements ccusage-equivalent in-cycle deduplication for Claude Code
// transcript lines.
//
// Rules:
//   - Lines without a message id are never deduped (kept verbatim).
//   - Primary identity is (messageID, requestID).
//   - A sidechain replay shares the messageID but may carry a different
//     requestID; such a secondary collision is consolidated only when either
//     the resident or the incoming candidate isSidechain.
//   - On any collision the better candidate wins (keep-best ordering):
//     non-sidechain > higher token total > higher cost > has speed.
//
// USAGE COLLAPSES, ACTIVITY UNIONS. The two streams take opposite treatments of
// the same collision and the difference is not a subtlety, it is the whole
// correctness of the activity ledger. One API response is streamed across
// several transcript records sharing a message id; they repeat the response's
// token counts, so exactly one may become a usage row — keep-best. They do NOT
// repeat the response's tool_use blocks, they PARTITION them, so every record's
// blocks belong to the surviving turn. Taking only the winner's blocks (which
// is what this did until the union below) silently dropped 19,682 of 60,832
// local calls, and non-uniformly: Read lost 45% of its calls, Bash 29%, so
// rankings were distorted rather than merely low.
type deduper struct {
	primary   map[primaryKey]*candidate // (messageID, requestID) -> winner
	byMessage map[string]primaryKey     // messageID -> resident primary key
	// calls is the UNION of the tool calls seen under a primary key, in
	// first-seen order, deduplicated on toolCall.identity so a sidechain replay
	// of an already-seen record contributes nothing. Its length is the divisor
	// the read path attributes cost with, so it has to be the count across every
	// record of the message rather than any one record's.
	calls     map[primaryKey][]toolCall
	seenCalls map[primaryKey]map[string]struct{}
	noID      []candidate  // never-deduped (no message id)
	order     []primaryKey // stable emission order for deduped ones
	// hooks are activity rows that belong to no usage candidate: hook records
	// carry no message id and no usage block, so none of the dedup rules above
	// apply to them. Their own dedup keys (the record uuid) make a re-read
	// idempotent at the store instead.
	hooks []model.ActivityEvent
}

type primaryKey struct {
	messageID string
	requestID string
}

func newDeduper() *deduper {
	return &deduper{
		primary:   make(map[primaryKey]*candidate),
		byMessage: make(map[string]primaryKey),
		calls:     make(map[primaryKey][]toolCall),
		seenCalls: make(map[primaryKey]map[string]struct{}),
	}
}

// add ingests one candidate, applying the dedup rules. Whichever branch it
// takes, the candidate's tool calls join the union of the primary key its usage
// event ends up under — including the branches where the candidate itself loses
// and is discarded.
func (d *deduper) add(c candidate) {
	if c.messageID == "" {
		d.noID = append(d.noID, c)
		return
	}

	pk := primaryKey{messageID: c.messageID, requestID: c.requestID}

	// Primary collision: same message id AND request id. This is the streaming
	// case — locally the ONLY one: no message id anywhere in 1,300 transcripts
	// carries two request ids.
	if existing, ok := d.primary[pk]; ok {
		d.addCalls(pk, c.calls)
		if better(c, *existing) {
			cp := c
			d.primary[pk] = &cp
		}
		return
	}

	// Secondary (sidechain-replay) collision: same message id, different
	// request id. Consolidate only when a sidechain is involved on either side.
	if residentKey, ok := d.byMessage[c.messageID]; ok {
		resident := d.primary[residentKey]
		if resident != nil && (resident.isSidechain || c.isSidechain) {
			d.addCalls(residentKey, c.calls)
			if better(c, *resident) {
				// Replace the resident in place under its existing primary key
				// so emission order is preserved.
				cp := c
				d.primary[residentKey] = &cp
			}
			return
		}
	}

	// New distinct record.
	cp := c
	d.primary[pk] = &cp
	d.byMessage[c.messageID] = pk
	d.order = append(d.order, pk)
	d.addCalls(pk, c.calls)
}

// addCalls appends calls not already present under pk, preserving first-seen
// order. A sidechain replay repeats a record's tool_use blocks verbatim, so the
// identity check is what keeps a replayed call from inflating the divisor.
func (d *deduper) addCalls(pk primaryKey, calls []toolCall) {
	if len(calls) == 0 {
		return
	}
	seen, ok := d.seenCalls[pk]
	if !ok {
		seen = make(map[string]struct{}, len(calls))
		d.seenCalls[pk] = seen
	}
	for _, c := range calls {
		id := c.identity()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		d.calls[pk] = append(d.calls[pk], c)
	}
}

// better reports whether candidate a should win over b under the keep-best
// ordering: non-sidechain beats sidechain, then higher token total, then higher
// cost, then presence of a speed field.
func better(a, b candidate) bool {
	if a.isSidechain != b.isSidechain {
		return !a.isSidechain // non-sidechain wins
	}
	if a.total != b.total {
		return a.total > b.total
	}
	if a.cost != b.cost {
		return a.cost > b.cost
	}
	if a.hasSpeed != b.hasSpeed {
		return a.hasSpeed
	}
	return false
}

// addHooks records activity rows that stand outside the usage dedup rules.
func (d *deduper) addHooks(a []model.ActivityEvent) {
	d.hooks = append(d.hooks, a...)
}

// events returns the surviving usage events: deduped records in first-seen
// order followed by never-deduped (no-id) records in file order.
func (d *deduper) events() []model.UsageEvent {
	out := make([]model.UsageEvent, 0, len(d.order)+len(d.noID))
	for _, pk := range d.order {
		if c := d.primary[pk]; c != nil {
			out = append(out, c.event)
		}
	}
	for _, c := range d.noID {
		out = append(out, c.event)
	}
	return out
}

// activity returns the activity rows of every surviving turn, in the order
// events() emits those turns, plus the hook rows.
//
// The rows come from the union of the turn's records but are ATTRIBUTED to the
// winning candidate's usage event — its dedup key, its timestamp, its session
// and project. That pairing is the point: the union makes the divisor equal the
// number of rows the key will have, and the winner is the only record of the
// message that becomes a usage row, so every call names a key the ledger
// actually holds.
func (d *deduper) activity() []model.ActivityEvent {
	out := make([]model.ActivityEvent, 0, len(d.hooks))
	for _, pk := range d.order {
		if c := d.primary[pk]; c != nil {
			out = append(out, mintActivity(d.calls[pk], c.event, c.requestID)...)
		}
	}
	// A record with no message id is never deduped, so it is its own turn and
	// its own record's calls are the whole union.
	for i := range d.noID {
		c := &d.noID[i]
		out = append(out, mintActivity(c.calls, c.event, c.requestID)...)
	}
	return append(out, d.hooks...)
}

// skillContexts returns one row per surviving usage event that was produced
// inside a skill, taken from the same winners events() emits and in the same
// order.
//
// Taking the winner's copy is what keeps the context aligned with the event it
// describes: the row is keyed by the usage event's dedup key, and a loser's key
// names a usage row that events() never emitted, so the context would join
// nothing and the winning turn would show no skill at all. Hooks contribute
// none — they carry no usage object to attribute.
func (d *deduper) skillContexts() []model.SkillContext {
	var out []model.SkillContext
	add := func(c *candidate) {
		if c == nil || c.skill == "" {
			return
		}
		out = append(out, model.SkillContext{
			UsageDedupKey: c.event.DedupKey,
			Tool:          model.ToolClaudeCode,
			Skill:         c.skill,
			SessionID:     c.event.SessionID,
			Project:       c.event.Project,
			Model:         c.event.Model,
			EventTime:     c.event.EventTime,
			SourcePath:    c.event.SourcePath,
		})
	}
	for _, pk := range d.order {
		add(d.primary[pk])
	}
	for i := range d.noID {
		add(&d.noID[i])
	}
	return out
}
