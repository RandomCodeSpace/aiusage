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
type deduper struct {
	primary   map[primaryKey]*candidate // (messageID, requestID) -> winner
	byMessage map[string]primaryKey     // messageID -> resident primary key
	noID      []candidate               // never-deduped (no message id)
	order     []primaryKey              // stable emission order for deduped ones
}

type primaryKey struct {
	messageID string
	requestID string
}

func newDeduper() *deduper {
	return &deduper{
		primary:   make(map[primaryKey]*candidate),
		byMessage: make(map[string]primaryKey),
	}
}

// add ingests one candidate, applying the dedup rules.
func (d *deduper) add(c candidate) {
	if c.messageID == "" {
		d.noID = append(d.noID, c)
		return
	}

	pk := primaryKey{messageID: c.messageID, requestID: c.requestID}

	// Primary collision: same message id AND request id.
	if existing, ok := d.primary[pk]; ok {
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
