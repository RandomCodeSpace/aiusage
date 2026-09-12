// Command perfcheck enforces aiusage's production performance contract over
// paired process, query, and Go benchmark samples produced by CI.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	MiB = 1024 * 1024
	KiB = 1024
)

type result struct {
	Status    string `json:"status"`
	Category  string `json:"category"`
	Metric    string `json:"metric"`
	Baseline  string `json:"baseline,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Limit     string `json:"limit,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type report struct {
	Schema  string   `json:"schema"`
	Passed  bool     `json:"passed"`
	Results []result `json:"results"`
}

type processReport struct {
	Metric processMetric `json:"metric"`
}

type processMetric struct {
	Name         string  `json:"name"`
	Command      string  `json:"command"`
	Purpose      string  `json:"purpose"`
	DurationsNS  []int64 `json:"durations_ns"`
	FirstByteNS  []int64 `json:"first_byte_ns"`
	MaxRSSKB     []int64 `json:"max_rss_kb"`
	OutputBytes  int64   `json:"output_bytes"`
	OutputSHA256 string  `json:"output_sha256"`
}

type queryReport struct {
	Matrix  string        `json:"matrix"`
	Metrics []queryMetric `json:"metrics"`
}

type queryMetric struct {
	Name        string  `json:"name"`
	DurationsNS []int64 `json:"durations_ns"`
	Digest      string  `json:"sha256"`
	ResultBytes int64   `json:"result_bytes"`
}

type side struct {
	process     map[string]*processMetric
	queryDigest map[string]string
	queryTimed  map[string][]float64
	bench       map[string]map[string][]float64
}

func main() {
	baselineDir := flag.String("baseline", "", "baseline report directory")
	candidateDir := flag.String("candidate", "", "candidate report directory")
	baselineBench := flag.String("baseline-bench", "", "baseline Go benchmark output")
	candidateBench := flag.String("candidate-bench", "", "candidate Go benchmark output")
	out := flag.String("out", "", "JSON result path")
	flag.Parse()
	if *baselineDir == "" || *candidateDir == "" || *baselineBench == "" || *candidateBench == "" || *out == "" {
		fatalf("usage: perfcheck --baseline DIR --candidate DIR --baseline-bench FILE --candidate-bench FILE --out FILE")
	}

	baseline, err := loadSide(*baselineDir, *baselineBench)
	if err != nil {
		fatalf("load baseline: %v", err)
	}
	candidate, err := loadSide(*candidateDir, *candidateBench)
	if err != nil {
		fatalf("load candidate: %v", err)
	}
	results := evaluate(baseline, candidate)
	passed := true
	for _, item := range results {
		if item.Status == "FAIL" {
			passed = false
		}
	}
	r := report{Schema: "production-performance-v1", Passed: passed, Results: results}
	if err := writeReport(*out, r); err != nil {
		fatalf("write report: %v", err)
	}
	printMarkdown(os.Stdout, r)
	if !passed {
		os.Exit(1)
	}
}

func loadSide(dir, benchPath string) (side, error) {
	s := side{
		process:     map[string]*processMetric{},
		queryDigest: map[string]string{},
		queryTimed:  map[string][]float64{},
	}
	if err := loadProcess(filepath.Join(dir, "process"), &s); err != nil {
		return side{}, err
	}
	if err := loadQueries(filepath.Join(dir, "query"), &s); err != nil {
		return side{}, err
	}
	f, err := os.Open(benchPath)
	if err != nil {
		return side{}, err
	}
	defer f.Close()
	s.bench, err = parseBenchmarks(f)
	if err != nil {
		return side{}, fmt.Errorf("parse %s: %w", benchPath, err)
	}
	return s, nil
}

func loadProcess(dir string, s *side) error {
	files, err := jsonFiles(dir)
	if err != nil {
		return fmt.Errorf("process reports: %w", err)
	}
	for _, path := range files {
		var r processReport
		if err := readJSON(path, &r); err != nil {
			return err
		}
		m := r.Metric
		if m.Name == "" || m.Command == "" || (m.Purpose != "timed" && m.Purpose != "absolute" && m.Purpose != "correctness") || len(m.DurationsNS) == 0 || len(m.DurationsNS) != len(m.FirstByteNS) || len(m.DurationsNS) != len(m.MaxRSSKB) {
			return fmt.Errorf("invalid process report %s", path)
		}
		agg := s.process[m.Name]
		if agg == nil {
			agg = &processMetric{Name: m.Name, Command: m.Command, Purpose: m.Purpose, OutputBytes: m.OutputBytes, OutputSHA256: m.OutputSHA256}
			s.process[m.Name] = agg
		}
		if agg.Command != m.Command || agg.Purpose != m.Purpose || agg.OutputBytes != m.OutputBytes || agg.OutputSHA256 != m.OutputSHA256 {
			return fmt.Errorf("process metric %s changed command or output between reports", m.Name)
		}
		agg.DurationsNS = append(agg.DurationsNS, m.DurationsNS...)
		agg.FirstByteNS = append(agg.FirstByteNS, m.FirstByteNS...)
		agg.MaxRSSKB = append(agg.MaxRSSKB, m.MaxRSSKB...)
	}
	if len(s.process) == 0 {
		return fmt.Errorf("no process reports in %s", dir)
	}
	return nil
}

func loadQueries(dir string, s *side) error {
	files, err := jsonFiles(dir)
	if err != nil {
		return fmt.Errorf("query reports: %w", err)
	}
	for _, path := range files {
		var r queryReport
		if err := readJSON(path, &r); err != nil {
			return err
		}
		if r.Matrix != "full" && r.Matrix != "timed" {
			return fmt.Errorf("invalid query matrix in %s", path)
		}
		for _, m := range r.Metrics {
			if m.Name == "" || m.Digest == "" || len(m.DurationsNS) == 0 {
				return fmt.Errorf("invalid query metric in %s", path)
			}
			if prior, ok := s.queryDigest[m.Name]; ok && prior != m.Digest {
				return fmt.Errorf("query %s changed output between reports", m.Name)
			}
			s.queryDigest[m.Name] = m.Digest
			if r.Matrix == "timed" {
				for _, value := range m.DurationsNS {
					s.queryTimed[m.Name] = append(s.queryTimed[m.Name], float64(value))
				}
			}
		}
	}
	if len(s.queryDigest) == 0 || len(s.queryTimed) == 0 {
		return fmt.Errorf("query reports in %s must contain full and timed matrices", dir)
	}
	return nil
}

func evaluate(baseline, candidate side) []result {
	var out []result
	out = append(out, compareProcess(baseline.process, candidate.process)...)
	out = append(out, compareQueries(baseline, candidate)...)
	out = append(out, compareBenchmarks(baseline.bench, candidate.bench)...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status == "FAIL"
		}
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Metric < out[j].Metric
	})
	return out
}

func compareProcess(baseline, candidate map[string]*processMetric) []result {
	var out []result
	for _, name := range unionKeys(baseline, candidate) {
		b, bok := baseline[name]
		c, cok := candidate[name]
		if !cok {
			out = append(out, failed("process", name, "", "", "same metric set", "metric missing from baseline or candidate"))
			continue
		}
		if c.Purpose != "correctness" && len(c.DurationsNS) < 10 {
			out = append(out, failed("process", name, "", strconv.Itoa(len(c.DurationsNS)), "at least 10 candidate samples", "insufficient p95 samples"))
		}
		if c.Purpose != "correctness" {
			out = append(out, processAbsolute(*c)...)
		}
		if !bok {
			if c.Purpose != "absolute" {
				out = append(out, failed("process", name, "", "", "paired baseline", "non-absolute metric has no baseline"))
			}
			continue
		}
		if b.Purpose != c.Purpose || b.Command != c.Command {
			out = append(out, failed("process", name, b.Purpose+"/"+b.Command, c.Purpose+"/"+c.Command, "same purpose and command", "process metric contract changed"))
			continue
		}
		compareOutput := c.Command != "version" && !strings.HasPrefix(c.Command, "source-farm-")
		if compareOutput && (b.OutputSHA256 != c.OutputSHA256 || b.OutputBytes != c.OutputBytes) {
			out = append(out, failed("equivalence", name, b.OutputSHA256, c.OutputSHA256, "exact bytes", "process output changed"))
		} else if compareOutput {
			out = append(out, passed("equivalence", name, "exact bytes"))
		}
		if c.Purpose == "timed" {
			out = append(out, timingRegression("process", name, int64sToFloat(b.DurationsNS), int64sToFloat(c.DurationsNS))...)
		}
	}
	if small, ok := candidate["export-json-100k"]; ok {
		if large, found := candidate["export-json-1m"]; found {
			increase := percentile(int64sToFloat(large.MaxRSSKB), .95) - percentile(int64sToFloat(small.MaxRSSKB), .95)
			limit := float64(32 * 1024)
			if increase > limit {
				out = append(out, failed("memory", "export-rss-growth", "", fmtKB(increase), "<= 32 MiB", "100k to 1m p95 RSS increase"))
			} else {
				out = append(out, passedValue("memory", "export-rss-growth", fmtKB(increase), "<= 32 MiB"))
			}
		}
	}
	return out
}

func processAbsolute(m processMetric) []result {
	durationLimit := time.Duration(0)
	rssLimitKB := int64(0)
	firstByteLimit := time.Duration(0)
	switch m.Command {
	case "version":
		durationLimit, rssLimitKB = 100*time.Millisecond, 32*1024
	case "source-farm-discovery":
		durationLimit, rssLimitKB = 2500*time.Millisecond, 64*1024
	case "source-farm-unchanged":
		durationLimit, rssLimitKB = time.Second, 128*1024
	case "source-farm-catchup":
		durationLimit, rssLimitKB = 5*time.Second, 256*1024
	case "summary-all", "summary-breakdown", "summary-provider":
		durationLimit, rssLimitKB = 2500*time.Millisecond, 128*1024
	case "export-json", "export-csv":
		durationLimit, firstByteLimit, rssLimitKB = 30*time.Second, 500*time.Millisecond, 192*1024
	case "export-json-raw", "export-csv-raw":
		durationLimit, firstByteLimit, rssLimitKB = 30*time.Second, 500*time.Millisecond, 256*1024
	}
	if durationLimit == 0 {
		return []result{failed("absolute", m.Name, "", "", "known process command", "no release limit is defined")}
	}
	var out []result
	duration := time.Duration(percentile(int64sToFloat(m.DurationsNS), .95))
	out = append(out, limitDuration("absolute", m.Name+" duration p95", duration, durationLimit))
	rss := int64(percentile(int64sToFloat(m.MaxRSSKB), .95))
	out = append(out, limitInt("absolute", m.Name+" RSS", rss, rssLimitKB, "KiB"))
	if firstByteLimit > 0 {
		firstByte := time.Duration(percentile(int64sToFloat(m.FirstByteNS), .95))
		out = append(out, limitDuration("absolute", m.Name+" first byte p95", firstByte, firstByteLimit))
	}
	return out
}

func compareQueries(baseline, candidate side) []result {
	var out []result
	for _, name := range unionStringKeys(baseline.queryDigest, candidate.queryDigest) {
		b, bok := baseline.queryDigest[name]
		c, cok := candidate.queryDigest[name]
		if !bok || !cok || b != c {
			out = append(out, failed("equivalence", "query "+name, b, c, "exact JSON digest", "query result changed"))
		} else {
			out = append(out, passed("equivalence", "query "+name, "exact JSON digest"))
		}
	}
	for _, name := range unionFloatKeys(baseline.queryTimed, candidate.queryTimed) {
		b, bok := baseline.queryTimed[name]
		c, cok := candidate.queryTimed[name]
		if !bok || !cok {
			out = append(out, failed("query", name, "", "", "same timed metric set", "timed query missing"))
			continue
		}
		p95 := time.Duration(percentile(c, .95))
		out = append(out, limitDuration("absolute", "query "+name+" p95", p95, 2500*time.Millisecond))
		out = append(out, timingRegression("query", name, b, c)...)
	}
	return out
}

func compareBenchmarks(baseline, candidate map[string]map[string][]float64) []result {
	var out []result
	out = append(out, validateBenchmarks("baseline", baseline)...)
	out = append(out, validateBenchmarks("candidate", candidate)...)
	for _, name := range unionBenchKeys(baseline, candidate) {
		b, bok := baseline[name]
		c, cok := candidate[name]
		if !bok || !cok {
			out = append(out, failed("benchmark", name, "", "", "same benchmark set", "benchmark missing"))
			continue
		}
		out = append(out, benchmarkAbsolute(name, c)...)
		if bns, ok := b["ns/op"]; ok {
			if cns, found := c["ns/op"]; found {
				out = append(out, timingRegression("benchmark", name, bns, cns)...)
			}
		}
		for _, unit := range []string{"B/op", "allocs/op"} {
			bv, bok := b[unit]
			cv, cok := c[unit]
			if !bok || !cok {
				continue
			}
			base, cand := percentile(bv, .5), percentile(cv, .5)
			if cand > base*1.05 {
				out = append(out, failed("regression", name+" "+unit, fmtFloat(base), fmtFloat(cand), "<= baseline +5%", "allocation regression"))
			} else {
				out = append(out, passedValue("regression", name+" "+unit, fmtFloat(cand), "<= baseline +5%"))
			}
		}
	}
	return out
}

// Keep the release workload explicit: a benchmark omitted by both binaries must
// fail just as one omitted by only the candidate does.
func requiredBenchmarks() []string {
	names := []string{"BenchmarkReload", "BenchmarkScrubStep", "BenchmarkView",
		"BenchmarkRangeCycleBurst", "BenchmarkRangeCyclePaced", "BenchmarkRangeCycleRevisit",
		"BenchmarkSourceFarmUnchanged"}
	for _, prefix := range []string{"BenchmarkProductionRender120x40", "BenchmarkProductionRender200x60", "BenchmarkProductionColdLoad"} {
		for _, view := range []string{"Overview", "ByTool", "ByModel", "Sessions", "ActivityCalls",
			"ActivityAgent", "ActivitySkill", "ActivityMcpTool", "ActivityMcpServer", "ActivityPlugin"} {
			names = append(names, prefix+"/"+view)
		}
	}
	for _, input := range []string{"Key", "Mouse", "Resize", "Filter", "Scrub"} {
		names = append(names, "BenchmarkProductionUIThread/"+input)
	}
	return names
}

func validateBenchmarks(label string, benchmarks map[string]map[string][]float64) []result {
	var out []result
	for _, name := range requiredBenchmarks() {
		metrics, ok := benchmarks[name]
		if !ok {
			out = append(out, failed("benchmark", label+" "+name, "", "", "required workload", "benchmark missing"))
			continue
		}
		units := []string{"ns/op", "B/op", "allocs/op"}
		switch {
		case strings.HasPrefix(name, "BenchmarkRangeCycle"), strings.HasPrefix(name, "BenchmarkProductionColdLoad/"):
			units = append(units, "queries/op", "query-ms/op")
		case strings.HasPrefix(name, "BenchmarkProductionUIThread/"):
			units = append(units, "queries/op")
		case name == "BenchmarkSourceFarmUnchanged":
			units = append(units, "sources/op", "inserted/op")
		}
		n := len(metrics["ns/op"])
		for _, unit := range units {
			count := len(metrics[unit])
			if (count != 10 && count != 20) || count != n {
				out = append(out, failed("benchmark", label+" "+name+" "+unit, "", strconv.Itoa(count), "complete paired set of 10 or 20 samples", "required metric missing or sample count invalid"))
			}
		}
	}
	return out
}

func benchmarkAbsolute(name string, metrics map[string][]float64) []result {
	var durationLimit, bytesLimit, allocLimit float64
	switch {
	case name == "BenchmarkReload":
		durationLimit, bytesLimit, allocLimit = 25_000, 4*KiB, 24
	case name == "BenchmarkSourceFarmUnchanged":
		durationLimit, bytesLimit = 1_000_000_000, 16*MiB
	case name == "BenchmarkScrubStep":
		durationLimit, bytesLimit, allocLimit = 1_000, KiB, 4
	case name == "BenchmarkView" || strings.Contains(name, "ProductionRender120x40"):
		durationLimit, bytesLimit, allocLimit = 25_000_000, 4*MiB, 35_000
	case strings.Contains(name, "ProductionRender200x60"):
		durationLimit, bytesLimit, allocLimit = 33_000_000, 8*MiB, 70_000
	case strings.Contains(name, "ProductionColdLoad"):
		durationLimit = 2_500_000_000
	case strings.Contains(name, "ProductionUIThread"):
		durationLimit = 10_000_000
	case name == "BenchmarkRangeCycleBurst":
		durationLimit = 500_000_000
	case name == "BenchmarkRangeCyclePaced":
		durationLimit = 1_250_000_000
	}
	var out []result
	if durationLimit > 0 {
		if values := metrics["ns/op"]; len(values) > 0 {
			out = append(out, limitFloat("absolute", name+" ns/op p95", percentile(values, .95), durationLimit, "ns/op"))
			if strings.Contains(name, "ProductionRender120x40") {
				out = append(out, limitFloat("absolute", name+" ns/op median", percentile(values, .5), 16_000_000, "ns/op"))
			}
		}
	}
	if bytesLimit > 0 {
		if values := metrics["B/op"]; len(values) > 0 {
			out = append(out, limitFloat("absolute", name+" B/op", percentile(values, .95), bytesLimit, "B/op"))
		}
	}
	if allocLimit > 0 {
		if values := metrics["allocs/op"]; len(values) > 0 {
			out = append(out, limitFloat("absolute", name+" allocs/op", percentile(values, .95), allocLimit, "allocs/op"))
		}
	}
	if strings.HasPrefix(name, "BenchmarkRangeCycle") {
		queryLimit := float64(0)
		queryTimeLimit := float64(500)
		switch name {
		case "BenchmarkRangeCycleBurst":
			queryLimit = 5
		case "BenchmarkRangeCyclePaced":
			queryLimit = 16
		case "BenchmarkRangeCycleRevisit":
			queryLimit, queryTimeLimit = 0, 0
		default:
			return out
		}
		if values := metrics["queries/op"]; len(values) > 0 {
			out = append(out, limitFloat("deterministic", name+" queries/op", percentile(values, .95), queryLimit, "queries/op"))
		}
		if values := metrics["query-ms/op"]; len(values) > 0 {
			out = append(out, limitFloat("absolute", name+" query-ms/op", percentile(values, .95), queryTimeLimit, "query-ms/op"))
		}
	}
	if strings.Contains(name, "ProductionColdLoad") {
		if values := metrics["query-ms/op"]; len(values) > 0 {
			out = append(out, limitFloat("absolute", name+" query-ms/op", percentile(values, .95), 5_000, "query-ms/op"))
		}
	}
	if strings.Contains(name, "ProductionUIThread") {
		if values := metrics["queries/op"]; len(values) > 0 {
			out = append(out, limitFloat("deterministic", name+" queries/op", percentile(values, .95), 0, "queries/op"))
		}
	}
	if name == "BenchmarkSourceFarmUnchanged" {
		if values := metrics["sources/op"]; len(values) > 0 {
			value := percentile(values, .95)
			if value != 2500 {
				out = append(out, failed("deterministic", name+" sources/op", "", fmtFloat(value)+" sources/op", "= 2500 sources/op", "source-farm cardinality"))
			} else {
				out = append(out, passedValue("deterministic", name+" sources/op", fmtFloat(value)+" sources/op", "= 2500 sources/op"))
			}
		}
		if values := metrics["inserted/op"]; len(values) > 0 {
			out = append(out, limitFloat("deterministic", name+" inserted/op", percentile(values, .95), 0, "inserted/op"))
		}
	}
	return out
}

func timingRegression(category, name string, baseline, candidate []float64) []result {
	if len(baseline) != len(candidate) || (len(baseline) != 10 && len(baseline) != 20) {
		return []result{failed("regression", category+" "+name, strconv.Itoa(len(baseline)), strconv.Itoa(len(candidate)), "paired samples = 10 or 20", "sample count mismatch or unsupported sample count")}
	}
	bmed, cmed := percentile(baseline, .5), percentile(candidate, .5)
	p := pairedRegressionP(baseline, candidate, .10)
	detail := fmt.Sprintf("median ratio %.3fx, p=%.4f", cmed/bmed, p)
	if cmed >= bmed*1.10 && p < .05 {
		return []result{failed("regression", category+" "+name, fmtFloat(bmed), fmtFloat(cmed), "<10% slowdown or p>=0.05", detail)}
	}
	return []result{passedDetail("regression", category+" "+name, "<10% slowdown or p>=0.05", detail)}
}

// pairedRegressionP is an exact one-sided paired randomization test. The null
// permits a ten-percent slowdown; each pair tests candidate-(1.10*baseline).
func pairedRegressionP(baseline, candidate []float64, allowance float64) float64 {
	n := min(len(baseline), len(candidate))
	if n == 0 || n > 20 {
		return 1
	}
	diffs := make([]float64, n)
	observed := float64(0)
	for i := range n {
		diffs[i] = candidate[i] - baseline[i]*(1+allowance)
		observed += diffs[i]
	}
	total := 1 << n
	extreme := 0
	for mask := 0; mask < total; mask++ {
		sum := float64(0)
		for i, value := range diffs {
			if mask&(1<<i) == 0 {
				sum += math.Abs(value)
			} else {
				sum -= math.Abs(value)
			}
		}
		if sum+1e-12 >= observed {
			extreme++
		}
	}
	return float64(extreme) / float64(total)
}

func parseBenchmarks(r io.Reader) (map[string]map[string][]float64, error) {
	out := map[string]map[string][]float64{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		name := trimCPU(fields[0])
		metrics := out[name]
		if metrics == nil {
			metrics = map[string][]float64{}
			out[name] = metrics
		}
		for i := 2; i+1 < len(fields); i += 2 {
			value, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", scanner.Text(), err)
			}
			metrics[fields[i+1]] = append(metrics[fields[i+1]], value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no benchmark samples")
	}
	return out, nil
}

func trimCPU(name string) string {
	cut := strings.LastIndexByte(name, '-')
	if cut < 0 {
		return name
	}
	if _, err := strconv.Atoi(name[cut+1:]); err == nil {
		return name[:cut]
	}
	return name
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	index := int(math.Ceil(p*float64(len(copyValues)))) - 1
	index = max(0, min(index, len(copyValues)-1))
	return copyValues[index]
}

func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func readJSON(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func writeReport(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	encErr := json.NewEncoder(f).Encode(value)
	closeErr := f.Close()
	if encErr != nil {
		return encErr
	}
	return closeErr
}

func printMarkdown(w io.Writer, r report) {
	fmt.Fprintln(w, "| Status | Category | Metric | Candidate | Limit | Detail |")
	fmt.Fprintln(w, "|---|---|---|---:|---:|---|")
	for _, item := range r.Results {
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s |\n",
			item.Status, item.Category, item.Metric, item.Candidate, item.Limit, item.Detail)
	}
}

func passed(category, metric, limit string) result {
	return result{Status: "PASS", Category: category, Metric: metric, Limit: limit}
}

func passedValue(category, metric, candidate, limit string) result {
	return result{Status: "PASS", Category: category, Metric: metric, Candidate: candidate, Limit: limit}
}

func passedDetail(category, metric, limit, detail string) result {
	return result{Status: "PASS", Category: category, Metric: metric, Limit: limit, Detail: detail}
}

func failed(category, metric, baseline, candidate, limit, detail string) result {
	return result{Status: "FAIL", Category: category, Metric: metric, Baseline: baseline, Candidate: candidate, Limit: limit, Detail: detail}
}

func limitDuration(category, metric string, value, limit time.Duration) result {
	if value > limit {
		return failed(category, metric, "", value.String(), "<= "+limit.String(), "absolute release limit")
	}
	return passedValue(category, metric, value.String(), "<= "+limit.String())
}

func limitInt(category, metric string, value, limit int64, unit string) result {
	return limitFloat(category, metric, float64(value), float64(limit), unit)
}

func limitFloat(category, metric string, value, limit float64, unit string) result {
	if value > limit {
		return failed(category, metric, "", fmtFloat(value)+" "+unit, "<= "+fmtFloat(limit)+" "+unit, "release limit")
	}
	return passedValue(category, metric, fmtFloat(value)+" "+unit, "<= "+fmtFloat(limit)+" "+unit)
}

func int64sToFloat(values []int64) []float64 {
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = float64(value)
	}
	return out
}

func fmtFloat(value float64) string { return strconv.FormatFloat(value, 'f', 2, 64) }
func fmtKB(value float64) string    { return fmt.Sprintf("%.2f MiB", value/1024) }

func unionKeys(a, b map[string]*processMetric) []string {
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	return sortedKeys(keys)
}

func unionStringKeys(a, b map[string]string) []string {
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	return sortedKeys(keys)
}

func unionFloatKeys(a, b map[string][]float64) []string {
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	return sortedKeys(keys)
}

func unionBenchKeys(a, b map[string]map[string][]float64) []string {
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	return sortedKeys(keys)
}

func sortedKeys(keys map[string]bool) []string {
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
