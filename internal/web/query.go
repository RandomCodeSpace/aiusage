package web

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// maxFilterValues caps how many values one categorical filter may carry. The
// port is unauthenticated and every value becomes a bound parameter in an IN
// list, so the limit is there to keep a single request from building a statement
// with ten thousand placeholders. No real UI approaches it.
const maxFilterValues = 64

// groupDimensions are the group_by values this API accepts, matching
// store.Filter.GroupBy. Validating here rather than letting the store reject the
// dimension keeps an unknown value a 400 with a usable message instead of a 500
// carrying a store error.
var groupDimensions = map[string]bool{
	"hour": true, "day": true, "week": true, "month": true,
	"tool": true, "model": true, "provider": true, "project": true, "session": true,
}

// rollupDimensions are the subset the derived rollup keeps. Anything
// outside it (session, provider) or any session filter sends the query to the
// ledger, which is slower and exact rather than fast and wrong.
var rollupDimensions = map[string]bool{
	"hour": true, "day": true, "week": true, "month": true,
	"tool": true, "model": true, "project": true,
}

// parseFilter builds a store.Filter from the query string. The parameter names
// are the contract's: since/until as unix seconds (0 or absent = open bound),
// repeated tool/model/project/session, repeated group_by in the order given.
func parseFilter(q url.Values) (store.Filter, error) {
	since, err := parseUnix(q.Get("since"), "since")
	if err != nil {
		return store.Filter{}, err
	}
	until, err := parseUnix(q.Get("until"), "until")
	if err != nil {
		return store.Filter{}, err
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return store.Filter{}, fmt.Errorf("until must be after since")
	}

	f := store.Filter{Since: since, Until: until}
	for _, spec := range []struct {
		param string
		dest  *[]string
	}{
		{"tool", &f.Tools},
		{"model", &f.Models},
		{"project", &f.Projects},
		{"session", &f.Sessions},
	} {
		vals, err := parseList(q, spec.param)
		if err != nil {
			return store.Filter{}, err
		}
		*spec.dest = vals
	}

	dims, err := parseList(q, "group_by")
	if err != nil {
		return store.Filter{}, err
	}
	for _, d := range dims {
		if !groupDimensions[d] {
			return store.Filter{}, fmt.Errorf("invalid group_by %q", d)
		}
	}
	f.GroupBy = dims
	return f, nil
}

// parseUnix reads a unix-seconds bound. Empty and "0" both mean an OPEN bound,
// which is the honest reading of "no bound given" and lets the page ask for
// everything without inventing an epoch.
func parseUnix(s, name string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: want unix seconds", name)
	}
	if n < 0 {
		return time.Time{}, fmt.Errorf("invalid %s: negative", name)
	}
	if n == 0 {
		return time.Time{}, nil
	}
	return time.Unix(n, 0).UTC(), nil
}

// parseList collects a repeated parameter, dropping empty values (an empty
// filter value would otherwise select the rows whose dimension is unknown, which
// is a real query nobody meant to make by leaving a field blank).
func parseList(q url.Values, name string) ([]string, error) {
	raw := q[name]
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxFilterValues {
		return nil, fmt.Errorf("too many %s values: %d, max %d", name, len(raw), maxFilterValues)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// rollupServiceable reports whether the derived rollup can answer f
// EXACTLY. It is a whitelist on purpose: a dimension the rollup does not keep
// must send the query to the ledger, never be dropped or answered with a zero.
func rollupServiceable(f store.Filter) bool {
	if len(f.Sessions) > 0 {
		return false
	}
	for _, d := range f.GroupBy {
		if !rollupDimensions[d] {
			return false
		}
	}
	return true
}

// parseLimit reads the row limit for /api/events and clamps it to the hard cap.
// A caller asking for more gets the cap, not an error: the cap is a property of
// the server, and a client is allowed to not know it yet.
func parseLimit(s string) (int, error) {
	if s == "" {
		return EventsPageLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid limit: want a positive integer")
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid limit: want a positive integer")
	}
	if n > EventsPageLimit {
		return EventsPageLimit, nil
	}
	return n, nil
}

// parseCursor reads the keyset cursor: the last row id of the previous page.
// Empty means the first page. The value is opaque to the client, which passes
// back exactly what it was given.
func parseCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return n, nil
}
