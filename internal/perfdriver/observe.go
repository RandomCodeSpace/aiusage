package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type processMetric struct {
	Name          string   `json:"name"`
	Command       string   `json:"command"`
	Purpose       string   `json:"purpose"`
	DurationsNS   []int64  `json:"durations_ns"`
	FirstByteNS   []int64  `json:"first_byte_ns"`
	MaxRSSKB      []int64  `json:"max_rss_kb"`
	OutputBytes   int64    `json:"output_bytes"`
	OutputSHA256  string   `json:"output_sha256"`
	SampleDigests []string `json:"sample_digests"`
}

type processReport struct {
	Profile string        `json:"profile"`
	Binary  string        `json:"binary"`
	DB      string        `json:"db,omitempty"`
	Root    string        `json:"root,omitempty"`
	Samples int           `json:"samples"`
	Warmups int           `json:"warmups"`
	Metric  processMetric `json:"metric"`
}

type observedRun struct {
	duration   time.Duration
	firstByte  time.Duration
	maxRSSKB   int64
	outputSize int64
	digest     string
}

func observeProcesses(binary, dbPath, rootPath, name, command, purpose string, samples, warmups int, out io.Writer) error {
	if binary == "" || name == "" || command == "" {
		return fmt.Errorf("observe requires --binary, --name, and --command")
	}
	if samples <= 0 || warmups < 0 {
		return fmt.Errorf("observe needs positive samples and non-negative warmups")
	}
	if purpose != "timed" && purpose != "absolute" && purpose != "correctness" {
		return fmt.Errorf("observe purpose must be timed, absolute, or correctness")
	}
	args, needsDB, needsRoot, err := observedCommand(command, dbPath, rootPath)
	if err != nil {
		return err
	}
	if needsDB && dbPath == "" {
		return fmt.Errorf("observe command %s requires --db", command)
	}
	if needsRoot && rootPath == "" {
		return fmt.Errorf("observe command %s requires --root", command)
	}
	root, err := os.MkdirTemp("", "aiusage-perf-observe-")
	if err != nil {
		return fmt.Errorf("create observer state: %w", err)
	}
	defer os.RemoveAll(root)
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte("{\"pricing\":{\"refresh\":false}}\n"), 0o600); err != nil {
		return fmt.Errorf("write observer config: %w", err)
	}
	if needsDB && !strings.HasPrefix(command, "source-farm-") {
		args = append([]string{"--db", dbPath, "--home", filepath.Join(root, "home"), "--config", configPath, "--no-daemon"}, args...)
	}
	env := observerEnvironment(root)
	for range warmups {
		if _, err := observeOne(binary, args, env); err != nil {
			return fmt.Errorf("warm %s: %w", name, err)
		}
	}

	metric := processMetric{
		Name:          name,
		Command:       command,
		Purpose:       purpose,
		DurationsNS:   make([]int64, 0, samples),
		FirstByteNS:   make([]int64, 0, samples),
		MaxRSSKB:      make([]int64, 0, samples),
		SampleDigests: make([]string, 0, samples),
	}
	for range samples {
		result, err := observeOne(binary, args, env)
		if err != nil {
			return fmt.Errorf("observe %s: %w", name, err)
		}
		if metric.OutputSHA256 != "" && metric.OutputSHA256 != result.digest {
			return fmt.Errorf("%s output changed between samples: %s then %s", name, metric.OutputSHA256, result.digest)
		}
		if metric.OutputBytes != 0 && metric.OutputBytes != result.outputSize {
			return fmt.Errorf("%s output size changed between samples: %d then %d", name, metric.OutputBytes, result.outputSize)
		}
		metric.OutputSHA256 = result.digest
		metric.OutputBytes = result.outputSize
		metric.DurationsNS = append(metric.DurationsNS, result.duration.Nanoseconds())
		metric.FirstByteNS = append(metric.FirstByteNS, result.firstByte.Nanoseconds())
		metric.MaxRSSKB = append(metric.MaxRSSKB, result.maxRSSKB)
		metric.SampleDigests = append(metric.SampleDigests, result.digest)
	}
	profile := longLedgerProfile
	if strings.HasPrefix(command, "source-farm-") {
		profile = sourceFarmProfile
	}
	report := processReport{
		Profile: profile,
		Binary:  binary,
		DB:      dbPath,
		Root:    rootPath,
		Samples: samples,
		Warmups: warmups,
		Metric:  metric,
	}
	return json.NewEncoder(out).Encode(report)
}

func observedCommand(name, dbPath, rootPath string) ([]string, bool, bool, error) {
	switch name {
	case "version":
		return []string{"version"}, false, false, nil
	case "summary-all":
		return []string{"summary", "--json"}, true, false, nil
	case "summary-breakdown":
		return []string{"summary", "--by", "day,tool,model", "--json"}, true, false, nil
	case "summary-provider":
		return []string{"summary", "--by", "provider", "--json"}, true, false, nil
	case "export-json":
		return []string{"export", "--format", "json"}, true, false, nil
	case "export-csv":
		return []string{"export", "--format", "csv"}, true, false, nil
	case "export-json-raw":
		return []string{"export", "--format", "json", "--include-raw"}, true, false, nil
	case "export-csv-raw":
		return []string{"export", "--format", "csv", "--include-raw"}, true, false, nil
	case "source-farm-discovery":
		return []string{"source-farm-discovery", "--root", rootPath}, false, true, nil
	case "source-farm-catchup":
		return []string{"source-farm-catchup", "--root", rootPath}, false, true, nil
	case "source-farm-unchanged":
		return []string{"source-farm-unchanged", "--root", rootPath, "--db", dbPath, "--cycles", "1"}, true, true, nil
	default:
		return nil, false, false, fmt.Errorf("unknown observed command %q for database %q", name, dbPath)
	}
}

func observeOne(binary string, args, env []string) (observedRun, error) {
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{writer: &stderr, remaining: 32 << 10}
	start := time.Now()
	stdout := newMeasurementWriter(start)
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		return observedRun{}, fmt.Errorf("start %s: %w", binary, err)
	}
	err := cmd.Wait()
	duration := time.Since(start)
	if err != nil {
		return observedRun{}, fmt.Errorf("%s %s: %w: %s", binary, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	firstByte, outputSize, digest := stdout.result(duration)
	rss, err := processRSSKB(cmd.ProcessState)
	if err != nil {
		return observedRun{}, err
	}
	return observedRun{
		duration:   duration,
		firstByte:  firstByte,
		maxRSSKB:   rss,
		outputSize: outputSize,
		digest:     digest,
	}, nil
}

type measurementWriter struct {
	mu        sync.Mutex
	started   time.Time
	firstByte time.Duration
	bytes     int64
	hash      hash.Hash
}

func newMeasurementWriter(start time.Time) *measurementWriter {
	return &measurementWriter{started: start, hash: sha256.New()}
}

func (w *measurementWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > 0 && w.firstByte == 0 {
		w.firstByte = time.Since(w.started)
	}
	n, err := w.hash.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *measurementWriter) result(duration time.Duration) (time.Duration, int64, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	firstByte := w.firstByte
	if firstByte == 0 {
		firstByte = duration
	}
	return firstByte, w.bytes, hex.EncodeToString(w.hash.Sum(nil))
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) > 0 {
		if _, err := w.writer.Write(p); err != nil {
			return 0, err
		}
		w.remaining -= len(p)
	}
	return original, nil
}

func processRSSKB(state *os.ProcessState) (int64, error) {
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0, fmt.Errorf("process RSS is unavailable on %T", state.SysUsage())
	}
	return usage.Maxrss, nil
}

func observerEnvironment(root string) []string {
	overrides := map[string]string{
		"HOME":                            filepath.Join(root, "home"),
		"CODEX_HOME":                      filepath.Join(root, "codex"),
		"COPILOT_OTEL_FILE_EXPORTER_PATH": filepath.Join(root, "copilot.jsonl"),
		"XDG_CONFIG_HOME":                 filepath.Join(root, "config"),
		"XDG_DATA_HOME":                   filepath.Join(root, "data"),
		"XDG_STATE_HOME":                  filepath.Join(root, "state"),
		"TZ":                              "UTC",
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}
