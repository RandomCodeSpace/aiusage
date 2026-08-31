package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

type queryMetric struct {
	Name        string  `json:"name"`
	DurationsNS []int64 `json:"durations_ns"`
	Digest      string  `json:"sha256"`
	ResultBytes int     `json:"result_bytes"`
}

type querySuiteReport struct {
	Profile       string        `json:"profile"`
	Matrix        string        `json:"matrix"`
	SchemaVersion int           `json:"schema_version"`
	Samples       int           `json:"samples"`
	Warmups       int           `json:"warmups"`
	Metrics       []queryMetric `json:"metrics"`
}

type measuredQuery struct {
	name string
	run  func(context.Context) (any, error)
}

func runQuerySuite(dbPath, matrix string, samples, warmups int, out io.Writer) error {
	if dbPath == "" {
		return fmt.Errorf("query-suite requires --db")
	}
	if samples <= 0 || warmups < 0 {
		return fmt.Errorf("query-suite needs positive samples and non-negative warmups")
	}
	reader, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open query fixture: %w", err)
	}
	defer reader.Close()

	var queries []measuredQuery
	switch matrix {
	case "full":
		queries = fullQueryMatrix(reader)
	case "timed":
		queries = timedQueryMatrix(reader)
	default:
		return fmt.Errorf("unknown query matrix %q", matrix)
	}

	report := querySuiteReport{
		Profile:       longLedgerProfile,
		Matrix:        matrix,
		SchemaVersion: store.SchemaVersion,
		Samples:       samples,
		Warmups:       warmups,
		Metrics:       make([]queryMetric, 0, len(queries)),
	}
	ctx := context.Background()
	for _, query := range queries {
		for range warmups {
			if _, err := query.run(ctx); err != nil {
				return fmt.Errorf("warm %s: %w", query.name, err)
			}
		}
		metric := queryMetric{Name: query.name, DurationsNS: make([]int64, 0, samples)}
		for range samples {
			start := time.Now()
			value, err := query.run(ctx)
			duration := time.Since(start)
			if err != nil {
				return fmt.Errorf("run %s: %w", query.name, err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("encode %s result: %w", query.name, err)
			}
			digest := sha256.Sum256(encoded)
			hexDigest := hex.EncodeToString(digest[:])
			if metric.Digest != "" && metric.Digest != hexDigest {
				return fmt.Errorf("query %s changed output between samples: %s then %s", query.name, metric.Digest, hexDigest)
			}
			metric.Digest = hexDigest
			metric.ResultBytes = len(encoded)
			metric.DurationsNS = append(metric.DurationsNS, duration.Nanoseconds())
		}
		report.Metrics = append(report.Metrics, metric)
	}
	return json.NewEncoder(out).Encode(report)
}

func fullQueryMatrix(reader *store.Reader) []measuredQuery {
	today := longLedgerClock.AddDate(0, 0, -1)
	sevenDays := longLedgerClock.AddDate(0, 0, -7)
	thirtyDays := longLedgerClock.AddDate(0, 0, -30)
	queries := []measuredQuery{
		summaryQuery(reader, "summary/today", store.Filter{Since: today}),
		summaryQuery(reader, "summary/7d", store.Filter{Since: sevenDays}),
		summaryQuery(reader, "summary/30d", store.Filter{Since: thirtyDays}),
		summaryQuery(reader, "summary/all", store.Filter{}),
		summaryQuery(reader, "summary/all/day-tool-model", store.Filter{GroupBy: []string{"day", "tool", "model"}}),
		summaryQuery(reader, "summary/all/provider", store.Filter{GroupBy: []string{"provider"}}),
	}
	for _, dim := range []string{"hour", "day", "week", "month", "tool", "model", "provider", "project", "session"} {
		queries = append(queries, summaryQuery(reader, "summary/30d/by-"+dim,
			store.Filter{Since: thirtyDays, GroupBy: []string{dim}}))
	}
	queries = append(queries,
		summaryQuery(reader, "summary/30d/day-tool-model", store.Filter{Since: thirtyDays, GroupBy: []string{"day", "tool", "model"}}),
		summaryQuery(reader, "summary/filter-tool", store.Filter{Tools: []string{model.ToolCodex}, GroupBy: []string{"day"}}),
		summaryQuery(reader, "summary/filter-model", store.Filter{Models: []string{"model-07"}, GroupBy: []string{"month"}}),
		summaryQuery(reader, "summary/filter-project", store.Filter{Projects: []string{"/fixture/project-042"}, GroupBy: []string{"tool"}}),
		summaryQuery(reader, "summary/filter-session", store.Filter{Sessions: []string{"session-0042"}, GroupBy: []string{"provider"}}),
		unpricedQuery(reader, "unpriced/30d/day-tool-model", store.Filter{Since: thirtyDays, GroupBy: []string{"day", "tool", "model"}}),
		unpricedQuery(reader, "unpriced/all/provider", store.Filter{GroupBy: []string{"provider"}}),
		activitySummaryQuery(reader, "activity/30d/by-name", store.ActivityFilter{Since: thirtyDays, GroupBy: []string{"name"}}),
		activityTopQuery(reader, "activity/30d/top-calls", store.ActivityFilter{Since: thirtyDays, GroupBy: []string{"name", "kind", "tool"}}, store.ActivityByCalls),
		activityTopQuery(reader, "activity/30d/top-tokens", store.ActivityFilter{Since: thirtyDays, GroupBy: []string{"name", "kind", "tool"}}, store.ActivityByTokens),
		activityTopQuery(reader, "activity/30d/top-cost", store.ActivityFilter{Since: thirtyDays, GroupBy: []string{"name", "kind", "tool"}}, store.ActivityByCost),
	)
	for _, dim := range model.TurnDimensions() {
		queries = append(queries, turnContextTopQuery(reader,
			"turn-context/30d/"+string(dim), dim,
			store.ActivityFilter{Since: thirtyDays, GroupBy: []string{"value"}}))
	}
	return queries
}

func timedQueryMatrix(reader *store.Reader) []measuredQuery {
	thirtyDays := longLedgerClock.AddDate(0, 0, -30)
	queries := []measuredQuery{
		summaryQuery(reader, "summary/all", store.Filter{}),
		summaryQuery(reader, "summary/all/day-tool-model", store.Filter{GroupBy: []string{"day", "tool", "model"}}),
		summaryQuery(reader, "summary/all/provider", store.Filter{GroupBy: []string{"provider"}}),
		activityTopQuery(reader, "activity/30d/top-cost", store.ActivityFilter{Since: thirtyDays, GroupBy: []string{"name", "kind", "tool"}}, store.ActivityByCost),
	}
	for _, dim := range model.TurnDimensions() {
		queries = append(queries, turnContextTopQuery(reader,
			"turn-context/30d/"+string(dim), dim,
			store.ActivityFilter{Since: thirtyDays, GroupBy: []string{"value"}}))
	}
	return queries
}

func summaryQuery(reader *store.Reader, name string, filter store.Filter) measuredQuery {
	return measuredQuery{name: name, run: func(ctx context.Context) (any, error) {
		return reader.Summarize(ctx, filter)
	}}
}

func unpricedQuery(reader *store.Reader, name string, filter store.Filter) measuredQuery {
	return measuredQuery{name: name, run: func(ctx context.Context) (any, error) {
		return reader.UnpricedGroups(ctx, filter)
	}}
}

func activitySummaryQuery(reader *store.Reader, name string, filter store.ActivityFilter) measuredQuery {
	return measuredQuery{name: name, run: func(ctx context.Context) (any, error) {
		return reader.SummarizeActivity(ctx, filter)
	}}
}

func activityTopQuery(reader *store.Reader, name string, filter store.ActivityFilter, order store.ActivityOrder) measuredQuery {
	return measuredQuery{name: name, run: func(ctx context.Context) (any, error) {
		return reader.TopActivity(ctx, filter, order, 20)
	}}
}

func turnContextTopQuery(reader *store.Reader, name string, dimension model.TurnDimension, filter store.ActivityFilter) measuredQuery {
	return measuredQuery{name: name, run: func(ctx context.Context) (any, error) {
		return reader.TopTurnContext(ctx, dimension, filter, store.ActivityByCost, 20)
	}}
}
