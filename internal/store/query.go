package store

import (
	"fmt"
	"strings"
)

// buildWhere builds the WHERE clause (with a leading " WHERE " when non-empty)
// and the positional args for a Filter. Time bounds are compared against
// event_time_unix in UTC seconds; categorical filters use IN (...) lists.
func buildWhere(f Filter) (string, []any) {
	var conds []string
	var args []any

	if !f.Since.IsZero() {
		conds = append(conds, "event_time_unix >= ?")
		args = append(args, f.Since.UTC().Unix())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "event_time_unix < ?")
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
	addIn("tool", f.Tools)
	addIn("model", f.Models)
	addIn("project", f.Projects)
	addIn("session_id", f.Sessions)

	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// groupExpr maps a GroupBy dimension to its SQL select/group expression. Time
// dimensions are formatted in the local timezone (via 'localtime') so day/hour
// buckets match the wall clock, and use lexically-sortable layouts so callers
// can order buckets by the string value. Categorical dimensions select the raw
// column. The result is also used verbatim in GROUP BY / ORDER BY.
func groupExpr(dim string) (string, error) {
	switch dim {
	case "hour":
		return "strftime('%Y-%m-%d %H', event_time_unix, 'unixepoch', 'localtime')", nil
	case "day":
		return "strftime('%Y-%m-%d', event_time_unix, 'unixepoch', 'localtime')", nil
	case "week":
		// ISO-ish year-week; lexically sortable as YYYY-Www.
		return "strftime('%Y-W%W', event_time_unix, 'unixepoch', 'localtime')", nil
	case "month":
		return "strftime('%Y-%m', event_time_unix, 'unixepoch', 'localtime')", nil
	case "tool":
		return "tool", nil
	case "model":
		return "model", nil
	case "project":
		return "project", nil
	case "session":
		return "session_id", nil
	default:
		return "", fmt.Errorf("store: invalid group dimension %q", dim)
	}
}
