package views

import (
	"strings"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// The Timeline view draws a full-width time-series chart across the active
// range with a scrub crosshair and a docked amber readout strip. The root model
// owns the scrub cursor (which bucket is highlighted) and whether it is pinned;
// the view is stateless — Render takes TimelineData and returns a frame.

// TimelineData is the input for one Timeline frame.
type TimelineData struct {
	Buckets  []store.Bucket // ascending by time
	Dim      string         // "day" / "hour" / "week" / "month"
	Cursor   int            // highlighted bucket index (scrub crosshair)
	Pinned   bool           // whether the scrub is pinned (vs tracking live edge)
	RangeLbl string
	TopTool  string // dominant tool at the cursor (for the readout strip)
	Focused  bool   // whether the chart pane wears the focus ring
}

// dimNoun maps a grouping dimension to a human noun for the title.
func dimNoun(dim string) string {
	switch dim {
	case "hour":
		return "hourly"
	case "week":
		return "weekly"
	case "month":
		return "monthly"
	default:
		return "daily"
	}
}

func bucketLabel(b store.Bucket, dim string) string {
	if v, ok := b.Keys[dim]; ok {
		if dim == "hour" {
			if idx := strings.LastIndexByte(v, ' '); idx >= 0 && idx+1 < len(v) {
				return v[idx+1:] + ":00"
			}
		}
		return v
	}
	return "—"
}
