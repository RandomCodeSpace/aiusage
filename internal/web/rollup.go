package web

import (
	"context"
	"sync"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// Routing a question to the derived rollup or to the ledger.
//
// Two conditions have to hold before the rollup may answer. The first is about
// the QUESTION - rollupServiceable in query.go, a whitelist of the dimensions
// the table keeps. The second is about the TABLE, and it is the one a read-only
// server cannot fix: the v4 migration creates the rollup EMPTY and leaves the
// refill to the collector's next pass. On a machine with no daemon running,
// that pass never comes, and a server that trusted the rollup would show an
// empty dashboard over a full ledger for as long as the box stays daemonless.
//
// So the freshness verdict is checked here and a stale rollup sends every
// question to the ledger: slower, and right. The answer says which table it
// came from (the "source" field) rather than leaving the difference invisible.

// rollupCheckTTL is how long a freshness verdict is trusted before it is asked
// again. It bounds two costs against each other: the check is two aggregate
// queries, and a stale verdict must expire quickly enough that the fallback
// ends on its own once a daemon catches the rollup up.
const rollupCheckTTL = 5 * time.Second

// The source of a bucketed answer, as the wire names it.
const (
	sourceRollup = "rollup"
	sourceLedger = "ledger"
)

// rollupGate caches the store's freshness verdict. The verdict is keyed on the
// database's write time as well as the clock: a collection cycle (or a rebuild)
// touches the file, and there is no reason to keep serving from the ledger for
// the rest of a TTL after the rollup has caught up.
type rollupGate struct {
	mu      sync.Mutex
	checked bool
	stale   bool
	at      time.Time
	mtime   time.Time
}

// rollupUsable reports whether f may be answered from the rollup: the question
// must be one the rollup keeps the dimensions for, and the rollup must actually
// cover the ledger.
func (s *Server) rollupUsable(ctx context.Context, f store.Filter) bool {
	return rollupServiceable(f) && !s.rollupStale(ctx)
}

// rollupStale is the cached freshness verdict. An error is reported as STALE
// and cached like any other verdict: a wrong "fresh" serves zeros as if they
// were the answer, while a wrong "stale" only costs a ledger scan for one TTL.
// Caching the error also bounds a persistently failing store to one check and
// one log line per TTL instead of two aggregate queries per request. Holding
// the lock across the store round-trip is deliberate single-flight: the check
// runs at most once per TTL, so concurrent requests wait milliseconds once
// rather than racing to repeat it.
func (s *Server) rollupStale(ctx context.Context) bool {
	now := s.now()
	mtime := newestMTime(s.opt.DBPath)

	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	if s.gate.checked && s.gate.mtime.Equal(mtime) && now.Sub(s.gate.at) < rollupCheckTTL {
		return s.gate.stale
	}
	stale, err := s.reader.RollupStale(ctx)
	if err != nil {
		s.log.Printf("web: check rollup freshness: %v", err)
		stale = true
	}
	s.gate.checked, s.gate.stale, s.gate.at, s.gate.mtime = true, stale, now, mtime
	return stale
}
