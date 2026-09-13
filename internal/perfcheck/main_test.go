package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluatePassingContract(t *testing.T) {
	baseline := passingSide()
	candidate := passingSide()
	candidate.process["version"].OutputSHA256 = "candidate-build-id"
	candidate.process["export-json-1m"] = &processMetric{
		Name: "export-json-1m", Command: "export-json", Purpose: "absolute",
		DurationsNS: repeatInt64(10_000_000_000), FirstByteNS: repeatInt64(100_000_000),
		MaxRSSKB: repeatInt64(70 * 1024), OutputBytes: 10, OutputSHA256: "large",
	}
	results := evaluate(baseline, candidate)
	if len(results) == 0 {
		t.Fatal("evaluation returned no evidence")
	}
	for _, item := range results {
		if item.Status == "FAIL" {
			t.Errorf("unexpected failure: %+v", item)
		}
	}
}

func TestEvaluateRejectsContractFailures(t *testing.T) {
	baseline := passingSide()
	candidate := passingSide()
	candidate.process["summary-all"].OutputSHA256 = "different"
	candidate.process["summary-all"].DurationsNS = repeatInt64(3_000_000_000)
	candidate.process["summary-all"].MaxRSSKB = repeatInt64(200 * 1024)
	delete(candidate.queryDigest, "summary/all/provider")
	candidate.bench["BenchmarkReload"]["B/op"] = repeatFloat(8 * KiB)
	candidate.bench["BenchmarkRangeCycleRevisit"]["queries/op"] = repeatFloat(1)

	results := evaluate(baseline, candidate)
	failures := 0
	joined := strings.Builder{}
	for _, item := range results {
		if item.Status == "FAIL" {
			failures++
			joined.WriteString(item.Category + "/" + item.Metric + "\n")
		}
	}
	if failures < 6 {
		t.Fatalf("got %d failures, want several independent gates:\n%s", failures, joined.String())
	}
	for _, want := range []string{"equivalence/summary-all", "absolute/summary-all", "regression/process summary-all", "equivalence/query summary/all/provider", "deterministic/BenchmarkRangeCycleRevisit"} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("failures missing %q:\n%s", want, joined.String())
		}
	}
}

func TestSourceFarmLimits(t *testing.T) {
	baseline := passingSide()
	candidate := passingSide()
	candidate.process["source-farm-discovery"].DurationsNS = repeatInt64(3_000_000_000)
	candidate.process["source-farm-catchup"].MaxRSSKB = repeatInt64(300 * 1024)
	candidate.bench["BenchmarkSourceFarmUnchanged"]["B/op"] = repeatFloat(17 * MiB)
	candidate.bench["BenchmarkSourceFarmUnchanged"]["sources/op"] = repeatFloat(2_499)
	candidate.bench["BenchmarkSourceFarmUnchanged"]["inserted/op"] = repeatFloat(1)

	var failures strings.Builder
	for _, item := range evaluate(baseline, candidate) {
		if item.Status == "FAIL" {
			failures.WriteString(item.Category + "/" + item.Metric + "\n")
		}
	}
	for _, want := range []string{
		"absolute/source-farm-discovery duration p95",
		"absolute/source-farm-catchup RSS",
		"absolute/BenchmarkSourceFarmUnchanged B/op",
		"deterministic/BenchmarkSourceFarmUnchanged sources/op",
		"deterministic/BenchmarkSourceFarmUnchanged inserted/op",
	} {
		if !strings.Contains(failures.String(), want) {
			t.Errorf("source-farm failures missing %q:\n%s", want, failures.String())
		}
	}
}

func TestLoadSideAndReportIO(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"process", "query"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	process := processReport{Metric: *passingSide().process["summary-all"]}
	writeTestJSON(t, filepath.Join(dir, "process", "summary.json"), process)
	full := queryReport{Matrix: "full", Metrics: []queryMetric{{Name: "q", DurationsNS: []int64{1}, Digest: "digest", ResultBytes: 1}}}
	timed := queryReport{Matrix: "timed", Metrics: []queryMetric{{Name: "q", DurationsNS: repeatInt64(1), Digest: "digest", ResultBytes: 1}}}
	writeTestJSON(t, filepath.Join(dir, "query", "full.json"), full)
	writeTestJSON(t, filepath.Join(dir, "query", "timed.json"), timed)
	benchPath := filepath.Join(dir, "bench.txt")
	bench := "BenchmarkReload-2 100 10000 ns/op 3000 B/op 22 allocs/op\n"
	if err := os.WriteFile(benchPath, []byte(bench), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSide(dir, benchPath)
	if err != nil {
		t.Fatalf("load side: %v", err)
	}
	if len(loaded.process) != 1 || len(loaded.queryTimed["q"]) != 10 || loaded.bench["BenchmarkReload"]["B/op"][0] != 3000 {
		t.Fatalf("loaded side = %+v", loaded)
	}

	r := report{Schema: "production-performance-v1", Passed: true, Results: []result{passed("test", "metric", "limit")}}
	reportPath := filepath.Join(dir, "out", "result.json")
	if err := writeReport(reportPath, r); err != nil {
		t.Fatal(err)
	}
	var decoded report
	raw, err := os.ReadFile(reportPath)
	if err != nil || json.Unmarshal(raw, &decoded) != nil || !decoded.Passed {
		t.Fatalf("written report = %s err=%v", raw, err)
	}
	var markdown bytes.Buffer
	printMarkdown(&markdown, r)
	if !strings.Contains(markdown.String(), "| PASS | test | metric |") {
		t.Fatalf("markdown = %q", markdown.String())
	}
}

func TestStatisticsAndBenchmarkParser(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		"goos: linux",
		"BenchmarkThing/Sub-2 10 123.5 ns/op 456 B/op 7 allocs/op",
		"BenchmarkThing/Sub-2 10 125.5 ns/op 458 B/op 8 allocs/op",
	}, "\n"))
	parsed, err := parseBenchmarks(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed["BenchmarkThing/Sub"]["ns/op"]; len(got) != 2 || got[1] != 125.5 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if trimCPU("BenchmarkNoCPU") != "BenchmarkNoCPU" || trimCPU("BenchmarkX-16") != "BenchmarkX" {
		t.Fatal("CPU suffix trimming failed")
	}
	if got := percentile([]float64{5, 1, 4, 2, 3}, .5); got != 3 {
		t.Fatalf("median = %v", got)
	}
	base := repeatFloat(100)
	fast := repeatFloat(105)
	slow := repeatFloat(130)
	if p := pairedRegressionP(base, fast, .10); p < .05 {
		t.Fatalf("fast candidate p=%v", p)
	}
	if p := pairedRegressionP(base, slow, .10); p >= .05 {
		t.Fatalf("slow candidate p=%v", p)
	}
	if got := timingRegression("test", "fast", base, fast); got[0].Status != "PASS" {
		t.Fatalf("fast timing = %+v", got)
	}
	if got := timingRegression("test", "short", base[:2], fast[:2]); got[0].Status != "FAIL" {
		t.Fatalf("short timing = %+v", got)
	}
	if _, err := parseBenchmarks(strings.NewReader("no benchmarks\n")); err == nil {
		t.Fatal("empty benchmark input succeeded")
	}
}

func TestBenchmarkContractRejectsMissingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(side, side)
	}{
		{"both omit view", func(b, c side) {
			delete(b.bench, "BenchmarkProductionColdLoad/ByTool")
			delete(c.bench, "BenchmarkProductionColdLoad/ByTool")
		}},
		{"both omit workload", func(b, c side) {
			delete(b.bench, "BenchmarkReload")
			delete(c.bench, "BenchmarkReload")
		}},
		{"allocation unit", func(b, c side) { delete(c.bench["BenchmarkView"], "B/op") }},
		{"query unit", func(b, c side) { delete(c.bench["BenchmarkRangeCycleBurst"], "queries/op") }},
		{"source unit", func(b, c side) { delete(c.bench["BenchmarkSourceFarmUnchanged"], "sources/op") }},
		{"partial unit", func(b, c side) { c.bench["BenchmarkView"]["allocs/op"] = []float64{1} }},
		{"inconsistent unit", func(b, c side) {
			c.bench["BenchmarkView"]["B/op"] = append(repeatFloat(1), repeatFloat(1)...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, c := passingSide(), passingSide()
			tc.alter(b, c)
			for _, item := range compareBenchmarks(b.bench, c.bench) {
				if item.Category == "benchmark" && item.Status == "FAIL" {
					return
				}
			}
			t.Fatal("incomplete benchmark evidence passed")
		})
	}
}

func TestTimingRejectsUnsupportedSampleCounts(t *testing.T) {
	for _, n := range []int{0, 9, 11, 19, 21} {
		values := make([]float64, n)
		for i := range values {
			values[i] = 100
		}
		if got := timingRegression("test", "count", values, values); got[0].Status != "FAIL" {
			t.Errorf("%d samples passed: %+v", n, got)
		}
	}
	values := append(repeatFloat(100), repeatFloat(100)...)
	if got := timingRegression("test", "twenty", values, values); got[0].Status != "PASS" {
		t.Fatalf("supported twenty-sample retry failed: %+v", got)
	}
	if got := percentile([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, .95); got != 10 {
		t.Fatalf("ten-sample nearest-rank p95 = %v, want maximum", got)
	}
}

func passingSide() side {
	process := map[string]*processMetric{
		"version": {
			Name: "version", Command: "version", Purpose: "timed",
			DurationsNS: repeatInt64(50_000_000), FirstByteNS: repeatInt64(40_000_000), MaxRSSKB: repeatInt64(16 * 1024),
			OutputBytes: 2, OutputSHA256: "version",
		},
		"summary-all": {
			Name: "summary-all", Command: "summary-all", Purpose: "timed",
			DurationsNS: repeatInt64(1_000_000_000), FirstByteNS: repeatInt64(900_000_000), MaxRSSKB: repeatInt64(64 * 1024),
			OutputBytes: 10, OutputSHA256: "summary",
		},
		"export-json-100k": {
			Name: "export-json-100k", Command: "export-json", Purpose: "timed",
			DurationsNS: repeatInt64(2_000_000_000), FirstByteNS: repeatInt64(100_000_000), MaxRSSKB: repeatInt64(50 * 1024),
			OutputBytes: 10, OutputSHA256: "export",
		},
		"source-farm-discovery": {
			Name: "source-farm-discovery", Command: "source-farm-discovery", Purpose: "timed",
			DurationsNS: repeatInt64(200_000_000), FirstByteNS: repeatInt64(190_000_000), MaxRSSKB: repeatInt64(32 * 1024),
			OutputBytes: 10, OutputSHA256: "farm-discovery",
		},
		"source-farm-catchup": {
			Name: "source-farm-catchup", Command: "source-farm-catchup", Purpose: "timed",
			DurationsNS: repeatInt64(1_000_000_000), FirstByteNS: repeatInt64(900_000_000), MaxRSSKB: repeatInt64(128 * 1024),
			OutputBytes: 10, OutputSHA256: "farm-catchup",
		},
		"source-farm-unchanged": {
			Name: "source-farm-unchanged", Command: "source-farm-unchanged", Purpose: "timed",
			DurationsNS: repeatInt64(100_000_000), FirstByteNS: repeatInt64(90_000_000), MaxRSSKB: repeatInt64(64 * 1024),
			OutputBytes: 10, OutputSHA256: "farm-unchanged",
		},
	}
	bench := map[string]map[string][]float64{
		"BenchmarkReload":                   benchmarkMetrics(10_000, 3*KiB, 22),
		"BenchmarkScrubStep":                benchmarkMetrics(500, 500, 2),
		"BenchmarkView":                     benchmarkMetrics(10_000_000, 3*MiB, 30_000),
		"BenchmarkProductionRender120x40/X": benchmarkMetrics(10_000_000, 3*MiB, 30_000),
		"BenchmarkProductionRender200x60/X": benchmarkMetrics(20_000_000, 7*MiB, 60_000),
		"BenchmarkProductionColdLoad/X":     withExtra(benchmarkMetrics(1_000_000_000, 1, 1), "query-ms/op", 1000),
		"BenchmarkProductionUIThread/Key":   withExtra(benchmarkMetrics(1_000_000, 1, 1), "queries/op", 0),
		"BenchmarkRangeCycleBurst": withExtras(benchmarkMetrics(100_000_000, 1, 1), map[string]float64{
			"queries/op": 5, "query-ms/op": 400,
		}),
		"BenchmarkRangeCyclePaced": withExtras(benchmarkMetrics(1_000_000_000, 1, 1), map[string]float64{
			"queries/op": 16, "query-ms/op": 400,
		}),
		"BenchmarkRangeCycleRevisit": withExtras(benchmarkMetrics(100_000, 1, 1), map[string]float64{
			"queries/op": 0, "query-ms/op": 0,
		}),
		"BenchmarkSourceFarmUnchanged": withExtras(benchmarkMetrics(100_000_000, 8*MiB, 10_000), map[string]float64{
			"sources/op": 2500, "inserted/op": 0,
		}),
	}
	for _, name := range requiredBenchmarks() {
		if bench[name] != nil {
			continue
		}
		switch {
		case strings.HasPrefix(name, "BenchmarkProductionRender120x40/"):
			bench[name] = benchmarkMetrics(10_000_000, 3*MiB, 30_000)
		case strings.HasPrefix(name, "BenchmarkProductionRender200x60/"):
			bench[name] = benchmarkMetrics(20_000_000, 7*MiB, 60_000)
		case strings.HasPrefix(name, "BenchmarkProductionColdLoad/"):
			bench[name] = withExtras(benchmarkMetrics(1_000_000_000, 1, 1), map[string]float64{"queries/op": 4, "query-ms/op": 1000})
		case strings.HasPrefix(name, "BenchmarkProductionUIThread/"):
			bench[name] = withExtra(benchmarkMetrics(1_000_000, 1, 1), "queries/op", 0)
		}
	}
	return side{
		process:     process,
		queryDigest: map[string]string{"summary/all/provider": "query"},
		queryTimed:  map[string][]float64{"summary/all/provider": repeatFloat(1_000_000)},
		bench:       bench,
	}
}

func benchmarkMetrics(ns, bytes, allocs float64) map[string][]float64 {
	return map[string][]float64{"ns/op": repeatFloat(ns), "B/op": repeatFloat(bytes), "allocs/op": repeatFloat(allocs)}
}

func withExtra(metrics map[string][]float64, unit string, value float64) map[string][]float64 {
	metrics[unit] = repeatFloat(value)
	return metrics
}

func withExtras(metrics map[string][]float64, values map[string]float64) map[string][]float64 {
	for unit, value := range values {
		metrics[unit] = repeatFloat(value)
	}
	return metrics
}

func repeatInt64(value int64) []int64 {
	out := make([]int64, 10)
	for i := range out {
		out[i] = value
	}
	return out
}

func repeatFloat(value float64) []float64 {
	out := make([]float64, 10)
	for i := range out {
		out[i] = value
	}
	return out
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
