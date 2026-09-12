package report

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/RandomCodeSpace/aiusage/store"
)

func TestHumanCostProvenancePreservesMachineApproximation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		computed, unpriced int64
		groups             []store.UnpricedGroup
		pricer             Pricer
		want               string
		machine            bool
	}{
		{name: "vendor", want: "$1.23"},
		{name: "computed", computed: 2, want: "~$1.23"},
		{name: "mixed stamps", computed: 1, want: "~$1.23"},
		{name: "vendor floor", unpriced: 1, want: "≥$1.23", machine: true},
		{name: "computed floor", computed: 1, unpriced: 1, want: "≥~$1.23", machine: true},
		{name: "unknown", unpriced: 2, want: "-"},
		{name: "display full", unpriced: 2, groups: []store.UnpricedGroup{{Events: 2, Input: 2_000_000}}, pricer: fixedPricer{}, want: "~$2.00", machine: true},
		{name: "display floor", unpriced: 2, groups: []store.UnpricedGroup{{Events: 1, Input: 2_000_000}}, pricer: fixedPricer{}, want: "≥~$2.00", machine: true},
		{name: "vendor and display", unpriced: 1, groups: []store.UnpricedGroup{{Events: 1, Input: 1_000_000}}, pricer: fixedPricer{}, want: "~$2.23", machine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := store.Bucket{Events: 2, CostMicroUSD: 1_230_000, ComputedCostEvents: tc.computed, UnpricedEvents: tc.unpriced}
			if tc.unpriced == b.Events {
				b.CostMicroUSD = 0
			}
			sum := &store.Summary{Buckets: []store.Bucket{b}, Totals: b}
			costs := ResolveCosts(sum, tc.groups, tc.pricer)
			for _, c := range []Cost{costs.Buckets[0], costs.Totals} {
				if got := c.String(); got != tc.want {
					t.Errorf("cost = %q, want %q", got, tc.want)
				}
			}
			payload := summaryPayload(sum, costs)
			if payload.Buckets[0].CostApproximate != tc.machine || payload.Totals.CostApproximate != tc.machine {
				t.Fatalf("machine approximation changed: %+v", payload)
			}
			if plain := summaryPayload(sum, nil); plain.Totals.CostApproximate || plain.Buckets[0].CostApproximate {
				t.Fatal("export without resolved costs changed its machine approximation")
			}
		})
	}
}

func TestHumanReportWidthsUseTerminalCells(t *testing.T) {
	for _, key := range []string{"ascii", "界", "é", "e\u0301", "\x1b[31m界\x1b[0m"} {
		want := lipgloss.Width(key)
		widths := columnWidths([]string{"k"}, [][]string{{key}}, nil)
		if widths[0] != want {
			t.Errorf("%q width = %d, want %d", key, widths[0], want)
		}
		for _, padded := range []string{padLeft(key, 10), padRight(key, 10)} {
			if lipgloss.Width(padded) != 10 {
				t.Errorf("%q is %d cells", padded, lipgloss.Width(padded))
			}
		}
	}
	if got := padRight("ascii", 8); got != "ascii"+strings.Repeat(" ", 3) {
		t.Errorf("ASCII output changed: %q", got)
	}
}
