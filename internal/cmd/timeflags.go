package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// timeLayouts are accepted --since/--until formats, tried in order. Date-only
// values are interpreted at local midnight; full timestamps as given. A bare
// RFC3339 is accepted for machine callers.
var timeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseTimeFlag parses a --since/--until value. An empty string yields the zero
// time (open bound). Relative durations like "30m", "6h", "2d" are interpreted
// as "now minus that duration" so `--since 7d` works as a window start.
func parseTimeFlag(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, ok := parseSpan(s); ok {
		return timeNow().Add(-d), nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q: want RFC3339, YYYY-MM-DD[ HH:MM[:SS]], or a span like 30m/6h/2d", s)
}

// spanRe matches the duration spans accepted by `last` and relative time flags.
var spanRe = regexp.MustCompile(`^([0-9]+)(m|h|d)$`)

// parseSpan parses a duration of the form ^([0-9]+)(m|h|d)$. The bool reports
// whether the input matched the grammar at all (so callers can distinguish a
// span from an absolute timestamp).
func parseSpan(s string) (time.Duration, bool) {
	m := spanRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "m":
		return time.Duration(n) * time.Minute, true
	case "h":
		return time.Duration(n) * time.Hour, true
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}

// validDims is the set of grouping dimensions accepted by --by, matching
// store.Filter.GroupBy.
var validDims = map[string]bool{
	"hour": true, "day": true, "week": true, "month": true,
	"tool": true, "model": true, "project": true, "session": true,
}

// parseBy splits a comma-separated --by value into validated grouping
// dimensions. An empty value yields a nil slice (single grand-total bucket).
func parseBy(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	dims := make([]string, 0, len(parts))
	for _, p := range parts {
		d := strings.TrimSpace(p)
		if d == "" {
			continue
		}
		if !validDims[d] {
			return nil, fmt.Errorf("invalid --by dimension %q: want hour,day,week,month,tool,model,project,session", d)
		}
		dims = append(dims, d)
	}
	return dims, nil
}
