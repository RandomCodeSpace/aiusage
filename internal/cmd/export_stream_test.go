package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

// TestStreamEventExportCrossesPageBoundary exercises the fixed 4096-row
// boundary with every event at the same timestamp. The second query must resume
// by id, yielding one valid JSON array and one CSV header with no lost,
// duplicated or reordered row.
func TestStreamEventExportCrossesPageBoundary(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	events := make([]model.UsageEvent, exportPageSize+1)
	for i := range events {
		key := "paged-" + strconv.Itoa(i)
		events[i] = model.UsageEvent{
			Tool:         model.ToolCodex,
			Model:        "gpt-5",
			SessionID:    "same-second",
			EventTime:    at,
			ObservedTime: at.Add(time.Minute),
			TotalTokens:  1,
			SourcePath:   key,
			DedupKey:     key,
			Kind:         model.KindUsage,
		}
	}
	if n, err := st.InsertEvents(context.Background(), events); err != nil || n != len(events) {
		t.Fatalf("inserted=%d err=%v want %d,nil", n, err, len(events))
	}

	var jsonOut bytes.Buffer
	if err := streamEventExport(context.Background(), st.Reader, store.Filter{}, "json", false, &jsonOut); err != nil {
		t.Fatalf("stream JSON: %v", err)
	}
	var got []model.UsageEvent
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("parse streamed JSON: %v", err)
	}
	assertPagedEventOrder(t, got, len(events))

	var csvOut bytes.Buffer
	if err := streamEventExport(context.Background(), st.Reader, store.Filter{}, "csv", false, &csvOut); err != nil {
		t.Fatalf("stream CSV: %v", err)
	}
	records, err := csv.NewReader(&csvOut).ReadAll()
	if err != nil {
		t.Fatalf("parse streamed CSV: %v", err)
	}
	if len(records) != len(events)+1 {
		t.Fatalf("CSV records=%d want %d (one header plus every event)", len(records), len(events)+1)
	}
	for i := 1; i < len(records); i++ {
		wantPath := "paged-" + strconv.Itoa(i-1)
		if records[i][14] != wantPath {
			t.Fatalf("CSV row %d source_path=%q want %q", i, records[i][14], wantPath)
		}
	}
}

func assertPagedEventOrder(t *testing.T, events []model.UsageEvent, want int) {
	t.Helper()
	if len(events) != want {
		t.Fatalf("streamed events=%d want %d", len(events), want)
	}
	for i, event := range events {
		wantKey := "paged-" + strconv.Itoa(i)
		if event.DedupKey != wantKey {
			t.Fatalf("streamed event %d key=%q want %q", i, event.DedupKey, wantKey)
		}
	}
}
