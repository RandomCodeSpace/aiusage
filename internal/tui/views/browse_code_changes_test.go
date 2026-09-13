package views

import (
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/RandomCodeSpace/aiusage/store"
)

func TestBrowseSessionCodeChangesAcrossWidths(t *testing.T) {
	for _, width := range []int{42, 60, 80, 120} {
		for _, tc := range []struct {
			name          string
			summary       store.CodeChangeSummary
			ready, failed bool
			want          string
		}{
			{name: "pending", want: "loading"},
			{name: "unknown", ready: true, want: "not reported"},
			{name: "unknown changes", summary: store.CodeChangeSummary{UnknownChanges: 2}, ready: true, want: "not reported"},
			{name: "zero", summary: store.CodeChangeSummary{KnownChanges: 1}, ready: true, want: "+0/-0"},
			{name: "partial", summary: store.CodeChangeSummary{KnownChanges: 1, UnknownChanges: 1, LinesAdded: 12, LinesRemoved: 3}, ready: true, want: "+12/-3"},
			{name: "failed", ready: true, failed: true, want: "unavailable"},
		} {
			t.Run(strconv.Itoa(width)+"/"+tc.name, func(t *testing.T) {
				c := byEntityTestCtx()
				c.Humanize = func(n int64) string { return strconv.FormatInt(n, 10) }
				b := NewBrowse(c)
				cell := lipgloss.NewStyle().PaddingRight(1)
				b.ApplyStyles(cell, cell, cell)
				rows := []store.Bucket{{Keys: map[string]string{"session": "session-one"}, Events: 2, Total: 7}, {Keys: map[string]string{"session": "session-two"}, Events: 1, Total: 3}}
				lay := ComputeLayout(width-2, 38)
				b.SetData(c, "session", rows, 10)
				b.SetLayout(lay)
				b.SetCodeChanges(tc.summary, tc.ready, tc.failed)
				out := ansiBrowseTest.ReplaceAllString(b.View(), "")
				for _, label := range []string{"SESSION", "events", "total", "Recorded session lines", "Lifetime", tc.want} {
					if !strings.Contains(out, label) {
						t.Fatalf("missing %q:\n%s", label, out)
					}
				}
				if tc.summary.KnownChanges > 0 && tc.summary.UnknownChanges > 0 && !strings.Contains(out, "partial") {
					t.Fatal("partial coverage hidden")
				}
				if lipgloss.Width(out) > lay.BodyW || lipgloss.Height(out) > lay.BodyH {
					t.Fatalf("view %dx%d exceeds body %dx%d:\n%s", lipgloss.Width(out), lipgloss.Height(out), lay.BodyW, lay.BodyH, out)
				}
				if len(b.table.Rows()) != len(rows) {
					t.Fatal("session footer lost table rows")
				}
				b.SetCursor(1)
				if b.codeChangesReady || b.codeChangesFailed || b.codeChanges != (store.CodeChangeSummary{}) {
					t.Fatal("cursor retained old session counts")
				}
				b.SetData(c, "project", []store.Bucket{{Keys: map[string]string{"project": "p"}}}, 0)
				if strings.Contains(ansiBrowseTest.ReplaceAllString(b.View(), ""), "Recorded session lines") {
					t.Fatal("session detail leaked to another dimension")
				}
			})
		}
	}
}
