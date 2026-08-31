package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/store"
)

func TestGenerateAndQueryScaledLongLedger(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "usage.db")
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := generateLongLedger(dbPath, manifestPath, 200, 80, 40); err != nil {
		t.Fatalf("generate: %v", err)
	}
	var manifest longLedgerManifest
	readJSONFile(t, manifestPath, &manifest)
	if manifest.Profile != longLedgerProfile || manifest.SchemaVersion != store.SchemaVersion || manifest.UsageEvents != 200 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.UnpricedEvents != 2 || manifest.RawEvents != 20 || manifest.RawBytesPerEvent != 400 {
		t.Fatalf("fixture proportions = %+v", manifest)
	}
	if len(manifest.BoundaryInstants) != len(fixtureBoundaryTimes()) || manifest.DatabaseSHA256 == "" {
		t.Fatalf("fixture evidence incomplete: %+v", manifest)
	}

	for _, matrix := range []string{"full", "timed"} {
		var out bytes.Buffer
		if err := runQuerySuite(dbPath, matrix, 1, 1, &out); err != nil {
			t.Fatalf("query %s: %v", matrix, err)
		}
		var report querySuiteReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("decode %s: %v", matrix, err)
		}
		if report.Matrix != matrix || len(report.Metrics) == 0 {
			t.Fatalf("query report = %+v", report)
		}
		for _, metric := range report.Metrics {
			if metric.Digest == "" || len(metric.DurationsNS) != 1 || metric.ResultBytes == 0 {
				t.Fatalf("invalid metric: %+v", metric)
			}
		}
	}

	if err := generateLongLedger(dbPath, filepath.Join(dir, "again.json"), 1, 0, 0); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("existing fixture error = %v", err)
	}
}

func TestFixtureShapeAndValidation(t *testing.T) {
	if len(fixtureTools) != 15 || len(fixtureProviders) != 9 || len(fixtureTiers) != 3 {
		t.Fatalf("fixture cardinality drifted: tools=%d providers=%d tiers=%d", len(fixtureTools), len(fixtureProviders), len(fixtureTiers))
	}
	if raw := fixtureRawPayload(); len(raw) != 400 || !json.Valid([]byte(raw)) {
		t.Fatalf("raw fixture length/JSON = %d/%v", len(raw), json.Valid([]byte(raw)))
	}
	previous := time.Time{}
	for i := len(fixtureBoundaryTimes()); i < 10_000; i++ {
		at := fixtureEventTime(i, 10_000)
		if !previous.IsZero() && at.Before(previous) {
			t.Fatalf("fixture time moved backwards at %d: %s before %s", i, at, previous)
		}
		previous = at
	}
	if previous.After(longLedgerClock) {
		t.Fatalf("fixture extends past clock: %s", previous)
	}

	dir := t.TempDir()
	cases := []struct {
		name string
		err  error
	}{
		{"missing paths", generateLongLedger("", "", 1, 0, 0)},
		{"invalid counts", generateLongLedger(filepath.Join(dir, "a.db"), filepath.Join(dir, "a.json"), 0, 0, 0)},
		{"activity exceeds usage", generateLongLedger(filepath.Join(dir, "b.db"), filepath.Join(dir, "b.json"), 1, 2, 0)},
		{"query missing db", runQuerySuite("", "full", 1, 0, &bytes.Buffer{})},
		{"query invalid samples", runQuerySuite(filepath.Join(dir, "missing.db"), "full", 0, 0, &bytes.Buffer{})},
	}
	for _, tc := range cases {
		if tc.err == nil {
			t.Errorf("%s unexpectedly succeeded", tc.name)
		}
	}
}

func TestObserveProcesses(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stable")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'stable-output\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := observeProcesses(script, "", "", "version", "version", "timed", 2, 1, &out); err != nil {
		t.Fatalf("observe stable process: %v", err)
	}
	var report processReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Metric.Purpose != "timed" || len(report.Metric.DurationsNS) != 2 || report.Metric.OutputBytes != int64(len("stable-output\n")) {
		t.Fatalf("process report = %+v", report)
	}
	if report.Metric.OutputSHA256 == "" || report.Metric.SampleDigests[0] != report.Metric.SampleDigests[1] {
		t.Fatalf("process digests = %+v", report.Metric)
	}

	failing := filepath.Join(dir, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho failed >&2\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := observeProcesses(failing, "", "", "version", "version", "timed", 1, 0, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("failing process error = %v", err)
	}

	for _, tc := range []struct {
		name, binary, db, metric, command, purpose string
	}{
		{"missing", "", "", "", "", "timed"},
		{"purpose", script, "", "version", "version", "wrong"},
		{"unknown", script, "", "x", "wrong", "timed"},
		{"needs db", script, "", "summary", "summary-all", "timed"},
	} {
		if err := observeProcesses(tc.binary, tc.db, "", tc.metric, tc.command, tc.purpose, 1, 0, &bytes.Buffer{}); err == nil {
			t.Errorf("%s unexpectedly succeeded", tc.name)
		}
	}
}

func TestObservedCommandsAndWriters(t *testing.T) {
	for _, name := range []string{"version", "summary-all", "summary-breakdown", "summary-provider", "export-json", "export-csv", "export-json-raw", "export-csv-raw"} {
		args, needsDB, needsRoot, err := observedCommand(name, "/tmp/fixture.db", "")
		if err != nil || len(args) == 0 || (name == "version") == needsDB || needsRoot {
			t.Errorf("observedCommand(%s) = %v,%v,%v,%v", name, args, needsDB, needsRoot, err)
		}
	}
	for _, name := range []string{"source-farm-discovery", "source-farm-catchup", "source-farm-unchanged"} {
		args, _, needsRoot, err := observedCommand(name, "/tmp/farm.db", "/tmp/farm")
		if err != nil || len(args) == 0 || !needsRoot {
			t.Errorf("observedCommand(%s) = %v,%v,%v", name, args, needsRoot, err)
		}
	}
	if _, _, _, err := observedCommand("unknown", "", ""); err == nil {
		t.Fatal("unknown command succeeded")
	}

	w := newMeasurementWriter(time.Now())
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	first, size, digest := w.result(time.Second)
	if first <= 0 || size != 3 || digest == "" {
		t.Fatalf("measurement = %s,%d,%s", first, size, digest)
	}
	var dst bytes.Buffer
	limited := &limitedWriter{writer: &dst, remaining: 3}
	if n, err := limited.Write([]byte("abcdef")); err != nil || n != 6 || dst.String() != "abc" {
		t.Fatalf("limited write = %d,%v,%q", n, err, dst.String())
	}
}

func TestRunDispatch(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("empty run succeeded")
	}
	for _, name := range []string{"usage", "activity", "contexts"} {
		if err := run([]string{"fixture-size", name}); err != nil {
			t.Fatalf("fixture-size %s: %v", name, err)
		}
	}
	if err := run([]string{"fixture-size", "wrong"}); err == nil {
		t.Fatal("bad fixture size succeeded")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dispatch.db")
	manifestPath := filepath.Join(dir, "dispatch.json")
	if err := run([]string{
		"generate-long-ledger", "--db", dbPath, "--manifest", manifestPath,
		"--usage", "40", "--activity", "16", "--contexts", "8",
	}); err != nil {
		t.Fatalf("generate-long-ledger dispatch: %v", err)
	}
	if err := run([]string{"query-suite", "--db", dbPath, "--matrix", "timed", "--samples", "1"}); err != nil {
		t.Fatalf("query-suite dispatch: %v", err)
	}
	stable := filepath.Join(dir, "stable")
	if err := os.WriteFile(stable, []byte("#!/bin/sh\nprintf 'stable-output\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"observe", "--binary", stable, "--name", "version", "--command", "version", "--samples", "1",
	}); err != nil {
		t.Fatalf("observe dispatch: %v", err)
	}
	if err := run([]string{"unknown"}); err == nil {
		t.Fatal("unknown command succeeded")
	}
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
