package tui

import (
	"testing"
)

// Percent must not collapse a small-but-nonzero share to "0%" (the misleading
// "fresh 0%" case) nor a near-total share to a flat "100%". Tiny → "<1%",
// near-full → ">99%", exact zero/full stay "0%"/"100%".
func TestPercentSmallShare(t *testing.T) {
	cases := []struct {
		value, total int64
		want         string
	}{
		{0, 100, "0%"},       // exact zero
		{1, 1000, "<1%"},     // 0.1% — must not read as 0%
		{62, 9684, "<1%"},    // ~0.6% (the real fresh/total ratio)
		{50, 100, "50%"},     // mid
		{9622, 9684, ">99%"}, // ~99.4% — must not read as flat 100%
		{100, 100, "100%"},   // exact full
		{0, 0, "0%"},         // empty
	}
	for _, c := range cases {
		if got := Percent(c.value, c.total); got != c.want {
			t.Errorf("Percent(%d,%d) = %q, want %q", c.value, c.total, got, c.want)
		}
	}
}
