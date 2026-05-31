package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// HumanizeTokens renders a token count compactly: 912.3K, 2.0M, 1.4B. Values
// below 1000 are shown verbatim. One decimal place is used for K/M/B/T to keep
// columns narrow while preserving a sense of magnitude.
func HumanizeTokens(n int64) string {
	if n < 0 {
		return "-" + HumanizeTokens(-n)
	}
	switch {
	case n < 1_000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return trimDecimal(float64(n)/1_000) + "K"
	case n < 1_000_000_000:
		return trimDecimal(float64(n)/1_000_000) + "M"
	case n < 1_000_000_000_000:
		return trimDecimal(float64(n)/1_000_000_000) + "B"
	default:
		return trimDecimal(float64(n)/1_000_000_000_000) + "T"
	}
}

// trimDecimal formats with one decimal place but drops a trailing ".0" only for
// exact values >= 100 so narrow magnitudes (2.0M) keep their decimal for visual
// rhythm while large round numbers (250M) stay compact.
func trimDecimal(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	if v >= 100 && strings.HasSuffix(s, ".0") {
		return s[:len(s)-2]
	}
	return s
}

// CommaGroup renders an integer with thousands separators (1,234,567).
func CommaGroup(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// PadLeft right-aligns s within width columns using spaces. If s is wider than
// width it is returned unchanged.
func PadLeft(s string, width int) string {
	w := utf8.RuneCountInString(s)
	if w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}

// PadRight left-aligns s within width columns, truncating with an ellipsis if it
// overflows.
func PadRight(s string, width int) string {
	w := utf8.RuneCountInString(s)
	if w == width {
		return s
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return Truncate(s, width)
}

// Truncate shortens s to at most width display columns, appending an ellipsis
// when it was cut. width < 1 yields the empty string.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:width-1]) + "…"
}

// Percent renders value/total as a compact percentage string (e.g. "34%").
func Percent(value, total int64) string {
	if total <= 0 {
		return "0%"
	}
	p := float64(value) * 100 / float64(total)
	// Don't let a small-but-nonzero share round to "0%" (the misleading
	// "fresh 0%" case when cache dwarfs fresh), nor a near-total share to a
	// flat "100%". Both are honest only at the exact bounds.
	if p > 0 && p < 1 {
		return "<1%"
	}
	if p > 99 && p < 100 {
		return ">99%"
	}
	return strconv.FormatFloat(p, 'f', 0, 64) + "%"
}

// Delta renders the change from prev to cur as a directional chip:
//
//	▲ 312.0K  (rose — caller styles in Warn)
//	▼ 88.0K   (fell — caller styles in Good)
//	· —       (no prior period)
//
// It returns the bare glyph+number string; the caller applies color so the
// semantic (up=warn, down=good) stays at the call site where it has context.
// dir is +1 (rose), -1 (fell) or 0 (flat/no-prior) so the caller can pick a
// color without re-deriving the sign.
func Delta(cur, prev int64) (text string, dir int) {
	if prev == 0 {
		return "· —", 0
	}
	d := cur - prev
	switch {
	case d > 0:
		return "▲ " + HumanizeTokens(d), 1
	case d < 0:
		return "▼ " + HumanizeTokens(-d), -1
	default:
		return "= 0", 0
	}
}
