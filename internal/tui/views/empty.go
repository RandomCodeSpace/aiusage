package views

import (
	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/internal/store"
)

// empty.go renders the three honest empty treatments. "Zero tokens", "no
// rows" and "query failed" are three different facts and must never share a
// rendering: rows that exist with all-zero token counts, a query that returned
// nothing, and a query that errored each get a distinct glyph + word. The
// glyph+word pair is the load-bearing channel (it survives monochrome); color
// is secondary.

// EmptyKind selects one of the three treatments.
type EmptyKind int

const (
	// EmptyZeroTokens: rows exist but every token count is zero.
	EmptyZeroTokens EmptyKind = iota
	// EmptyNoRows: the query succeeded and returned no rows.
	EmptyNoRows
	// EmptyQueryFailed: the query itself errored.
	EmptyQueryFailed
)

// EmptyState renders the one-line treatment for kind, truncated to w columns.
func EmptyState(c Ctx, kind EmptyKind, w int) string {
	switch kind {
	case EmptyZeroTokens:
		return c.Faint.Render(truncTo(c, "∅ zero tokens in range", w))
	case EmptyQueryFailed:
		return lipgloss.NewStyle().Foreground(c.WarnColor).Render(truncTo(c, "✕ query failed", w))
	default:
		return c.Faint.Render(truncTo(c, "◌ no rows in range", w))
	}
}

// truncTo truncates through the injected helper when present (headless view
// tests construct partial Ctx values).
func truncTo(c Ctx, s string, w int) string {
	if c.Truncate == nil {
		return s
	}
	return c.Truncate(s, w)
}

// zeroTotals reports whether every bucket's Total is zero — the "rows exist,
// zero tokens" fact, distinct from having no rows at all.
func zeroTotals(buckets []store.Bucket) bool {
	for _, b := range buckets {
		if b.Total != 0 {
			return false
		}
	}
	return true
}
