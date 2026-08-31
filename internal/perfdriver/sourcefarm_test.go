package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceFarmContract(t *testing.T) {
	const (
		testSources = 32
		testRecords = 1_000
	)
	dir := t.TempDir()
	root := filepath.Join(dir, "farm")
	manifestPath := filepath.Join(dir, "manifest.json")
	fixtureRoot := filepath.Clean(filepath.Join("..", ".."))

	if err := generateSourceFarm(root, fixtureRoot, manifestPath, testSources, testRecords); err != nil {
		t.Fatalf("generate source farm: %v", err)
	}
	var manifest sourceFarmManifest
	readJSONFile(t, manifestPath, &manifest)
	if manifest.Profile != sourceFarmProfile || manifest.Adapters != len(sourceFarmTools) ||
		manifest.Sources != testSources || manifest.Records != testRecords {
		t.Fatalf("source-farm manifest = %+v", manifest)
	}
	if manifest.CanonicalFixtureRoot != "." || len(manifest.SourcesByTool) != len(sourceFarmTools) {
		t.Fatalf("source-farm provenance = %+v", manifest)
	}

	reportPath := filepath.Join(dir, "contract.json")
	if sourceFarmUnchangedCycles != 100 {
		t.Fatalf("source-farm production idle cycles = %d, want 100", sourceFarmUnchangedCycles)
	}
	if err := withNeutralSourceFarmEnvironment(func() error {
		return runSourceFarmContract(root, reportPath, testSources, testRecords, 1)
	}); err != nil {
		t.Fatalf("source-farm contract: %v", err)
	}
	var report sourceFarmContractReport
	readJSONFile(t, reportPath, &report)
	if report.Schema != "source-farm-contract-v1" || report.Profile != sourceFarmProfile {
		t.Fatalf("source-farm report identity = %+v", report)
	}
	if report.Initial.Adapters != len(sourceFarmTools) || report.Initial.Sources != testSources ||
		report.Initial.Records != testRecords {
		t.Fatalf("source-farm initial cycle = %+v", report.Initial)
	}
	if len(report.ChangedTools) != len(sourceFarmTools) || len(report.ChangedShapes) != 17 ||
		report.Changed.EventsInserted != 16 || report.Changed.ActivityInserted != 1 {
		t.Fatalf("source-farm changed cycle = %+v", report)
	}
	if report.UnchangedCycles != 1 {
		t.Fatalf("source-farm unchanged cycles = %d, want 1", report.UnchangedCycles)
	}

	for _, args := range [][]string{
		{"source-farm-discovery", "--root", root},
		{"source-farm-catchup", "--root", root},
	} {
		if err := run(args); err != nil {
			t.Fatalf("run %s: %v", args[0], err)
		}
	}
	warmDB := filepath.Join(dir, "warm.db")
	if err := run([]string{"source-farm-warm", "--root", root, "--db", warmDB}); err != nil {
		t.Fatalf("run source-farm-warm: %v", err)
	}
	if err := run([]string{"source-farm-unchanged", "--root", root, "--db", warmDB}); err != nil {
		t.Fatalf("run source-farm-unchanged: %v", err)
	}
	if err := run([]string{"source-farm-contract", "--root", root, "--out", filepath.Join(dir, "wrong-shape.json")}); err == nil {
		t.Fatal("production contract accepted the scaled test farm")
	}
	blockedOutput := filepath.Join(dir, "blocked-output")
	if err := os.WriteFile(blockedOutput, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := withNeutralSourceFarmEnvironment(func() error {
		return runSourceFarmContract(root, filepath.Join(blockedOutput, "contract.json"), testSources, testRecords, 1)
	}); err == nil {
		t.Fatal("source-farm contract wrote below a regular file")
	}
	if _, err := mutateSourceFarm(root); err != nil {
		t.Fatalf("mutate source farm: %v", err)
	}
	err := withNeutralSourceFarmEnvironment(func() error {
		_, err := runSourceFarmUnchanged(root, warmDB, 1)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "inserted") {
		t.Fatalf("changed farm passed unchanged cycle: %v", err)
	}
}

func TestSourceFarmValidation(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	manifest := filepath.Join(dir, "manifest.json")
	if err := generateSourceFarm("", "", "", 0, 0); err == nil {
		t.Fatal("generate-source-farm accepted missing arguments")
	}
	nonempty := filepath.Join(dir, "nonempty")
	if err := os.MkdirAll(nonempty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonempty, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generateSourceFarm(nonempty, dir, manifest, 1, 1); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("non-empty source farm error = %v", err)
	}
	if err := requireSourceFarmFixtures(missing); err == nil {
		t.Fatal("missing canonical fixtures succeeded")
	}
	tooSmall := filepath.Join(dir, "too-small")
	fixtureRoot := filepath.Clean(filepath.Join("..", ".."))
	if err := generateSourceFarm(tooSmall, fixtureRoot, filepath.Join(dir, "too-small.json"), 1, 1); err == nil {
		t.Fatal("source farm accepted an impossible Codex budget")
	}
	if _, err := discoverSourceCounts(t.Context(), missing); err == nil {
		t.Fatal("source discovery accepted an empty farm")
	}

	existingDB := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(existingDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSourceFarmCatchup(dir, existingDB); err == nil {
		t.Fatal("catch-up replaced an existing database")
	}
	blockedParent := filepath.Join(dir, "blocked-parent")
	if err := os.WriteFile(blockedParent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runSourceFarmCatchup(dir, filepath.Join(blockedParent, "usage.db")); err == nil {
		t.Fatal("catch-up opened a database below a regular file")
	}
	if _, err := warmSourceFarm(dir, ""); err == nil {
		t.Fatal("warm source farm accepted an empty database path")
	}
	if _, err := warmSourceFarm(dir, existingDB); err == nil {
		t.Fatal("warm source farm replaced an existing database")
	}
	if _, err := runSourceFarmUnchanged(dir, "", 0); err == nil {
		t.Fatal("unchanged source farm accepted invalid arguments")
	}
	if _, err := runSourceFarmUnchanged(dir, filepath.Join(blockedParent, "usage.db"), 1); err == nil {
		t.Fatal("unchanged source farm opened a database below a regular file")
	}
	if err := runSourceFarmContract("", "", 0, 0, 0); err == nil {
		t.Fatal("source-farm contract accepted invalid arguments")
	}
	if err := runSourceFarmContract(missing, filepath.Join(dir, "missing-contract.json"), 1, 1, 1); err == nil {
		t.Fatal("source-farm contract accepted a missing root")
	}
	if _, err := firstMatchingFile(dir, ".never"); err == nil {
		t.Fatal("missing matching file succeeded")
	}
	if _, err := firstMatchingFile(missing, ".never"); err == nil {
		t.Fatal("walk of missing root succeeded")
	}
	if _, err := countMarker(missing, "x"); err == nil {
		t.Fatal("missing marker file succeeded")
	}
	if err := appendFarmFile(missing, []byte("x")); err == nil {
		t.Fatal("append to missing file succeeded")
	}
	if err := copyFarmFile(missing, filepath.Join(dir, "copy")); err == nil {
		t.Fatal("copy of missing file succeeded")
	}
	if err := copyFarmTree(missing, filepath.Join(dir, "missing-copy")); err == nil {
		t.Fatal("copy of missing tree succeeded")
	}
	if err := writeFarmFile(filepath.Join(blockedParent, "file"), []byte("x")); err == nil {
		t.Fatal("write below a regular file succeeded")
	}
	if err := writeSourceFarmCrushIndex(blockedParent); err == nil {
		t.Fatal("Crush index below a regular file succeeded")
	}
	if err := buildCodexFarm(filepath.Join(blockedParent, "codex"), 1, 1); err == nil {
		t.Fatal("Codex farm below a regular file succeeded")
	}
	if err := buildNonCodexFarm(filepath.Join(dir, "incomplete"), missing); err == nil {
		t.Fatal("non-Codex farm accepted missing fixtures")
	}
	if _, err := usageEventsByTool(existingDB); err == nil {
		t.Fatal("usage query accepted a non-database file")
	}

	badCline := filepath.Join(dir, "bad-cline.json")
	if err := os.WriteFile(badCline, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendClineMessage(badCline); err == nil {
		t.Fatal("invalid Cline JSON succeeded")
	}
	if err := os.WriteFile(badCline, []byte(`{"messages":"wrong"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendClineMessage(badCline); err == nil {
		t.Fatal("Cline document without an array succeeded")
	}

	if err := buildInlineDatabase(filepath.Join(dir, "bad-inline.db"), []string{"not sql"}, nil); err == nil {
		t.Fatal("invalid inline schema succeeded")
	}
	if err := buildInlineDatabase(filepath.Join(dir, "bad-row.db"), []string{"CREATE TABLE ok (id INTEGER)"}, []sqlStatement{{query: "not sql"}}); err == nil {
		t.Fatal("invalid inline row succeeded")
	}
	if err := buildSQLDatabase(filepath.Join(dir, "bad-script.db"), false, missing); err == nil {
		t.Fatal("missing SQL script succeeded")
	}
	badScript := filepath.Join(dir, "bad.sql")
	if err := os.WriteFile(badScript, []byte("not sql"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := buildSQLDatabase(filepath.Join(dir, "bad-sql.db"), true, badScript); err == nil {
		t.Fatal("invalid SQL script succeeded")
	}
	validDB := filepath.Join(dir, "valid.db")
	if err := buildInlineDatabase(validDB, []string{"CREATE TABLE ok (id INTEGER)"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := execStatements(validDB, []sqlStatement{{query: "not sql"}}); err == nil {
		t.Fatal("invalid SQL statement succeeded")
	}

	symlinkRoot := filepath.Join(dir, "symlink-root")
	if err := os.MkdirAll(symlinkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(validDB, filepath.Join(symlinkRoot, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyFarmTree(symlinkRoot, filepath.Join(dir, "symlink-copy")); err == nil {
		t.Fatal("source-farm symlink succeeded")
	}

	for _, args := range [][]string{
		{"generate-source-farm"},
		{"source-farm-warm"},
		{"source-farm-unchanged"},
		{"source-farm-contract"},
		{"generate-source-farm", "--unknown"},
		{"source-farm-discovery", "--unknown"},
		{"source-farm-catchup", "--unknown"},
		{"source-farm-warm", "--unknown"},
		{"source-farm-unchanged", "--unknown"},
		{"source-farm-contract", "--unknown"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run %q unexpectedly succeeded", args)
		}
	}
}
