package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
)

// Repository evidence is validated at its checked-in September capture snapshot.
const repositoryEvidenceNow = "2026-09-12T18:00:00Z"

func TestMainWritesMachineReadableCIResult(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "result.json")
	outRel, err := filepath.Rel(root, out)
	if err != nil {
		t.Fatal(err)
	}

	oldArgs, oldFlags := os.Args, flag.CommandLine
	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlags
	}()
	flag.CommandLine = flag.NewFlagSet("adaptercompatcheck", flag.ContinueOnError)
	os.Args = []string{
		"adaptercompatcheck",
		"--root", root,
		"--manifest", "adapter/compatibility.json",
		"--mode", "ci",
		"--now", repositoryEvidenceNow,
		"--out", outRel,
	}

	main()

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var got result
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.Schema != resultSchema || got.Mode != "ci" || got.Registered != 15 || len(got.Errors) != 0 {
		t.Fatalf("result = %+v", got)
	}
}

func TestRepositoryManifestCoversRegistryWithoutPretendingPendingEvidenceIsReady(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	m, raw, err := readManifest(filepath.Join(root, "adapter", "compatibility.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	got := validate(root, m, raw, "ci", mustTime(t, repositoryEvidenceNow))
	if len(got.Errors) != 0 {
		t.Fatalf("CI validation errors: %v", got.Errors)
	}
	if got.Registered != 15 || len(m.Entries) != got.Registered {
		t.Fatalf("registered/manifest entries = %d/%d, want 15/15", got.Registered, len(m.Entries))
	}
	if got.Ready != 14 || len(got.Pending) != 1 || got.Complete {
		t.Fatalf("ready/pending/complete = %d/%d/%v, want 14/1/false",
			got.Ready, len(got.Pending), got.Complete)
	}

	crush := pendingFor(t, got, "crush")
	if !contains(crush.Gaps, "no nonzero live vendor-cost evidence") {
		t.Fatalf("zero-cost Crush fixture was accepted as priced evidence: %v", crush.Gaps)
	}
}

func TestReleaseModeFailsClosedOnEveryPendingGap(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	m, raw, err := readManifest(filepath.Join(root, "adapter", "compatibility.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	got := validate(root, m, raw, "release", mustTime(t, repositoryEvidenceNow))
	if len(got.Errors) == 0 || got.Complete {
		t.Fatalf("release accepted pending compatibility evidence: complete=%v errors=%v",
			got.Complete, got.Errors)
	}
	if !containsSubstring(got.Errors, "release blocker:") {
		t.Fatalf("release errors do not identify the evidence blocker: %v", got.Errors)
	}
}

func TestManifestMustCoverEachRegisteredToolExactlyOnce(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	m, raw, err := readManifest(filepath.Join(root, "adapter", "compatibility.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	m.Entries = append(m.Entries[:len(m.Entries)-1], m.Entries[0])
	got := validate(root, m, raw, "ci", mustTime(t, repositoryEvidenceNow))
	if !containsSubstring(got.Errors, "duplicate tool entry") {
		t.Fatalf("duplicate entry was accepted: %v", got.Errors)
	}
	if !containsSubstring(got.Errors, "has no manifest entry") {
		t.Fatalf("missing registered tool was accepted: %v", got.Errors)
	}
}

func TestReadManifestRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{"schema":"adapter-compatibility-v1","entries":[]} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readManifest(path); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("readManifest trailing value error = %v", err)
	}
}

func TestReadManifestRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{"schema":"adapter-compatibility-v1","entries":[],"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readManifest(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("readManifest unknown-field error = %v", err)
	}
}

func TestReadyEntryPassesReleaseValidation(t *testing.T) {
	root := fixtureRoot(t)
	e := readyEntry()
	errs, gaps := validateEntry(root, e, clineCapability(), "release", mustTime(t, e.CapturedAt))
	if len(errs) != 0 || len(gaps) != 0 {
		t.Fatalf("ready entry rejected: errors=%v gaps=%v", errs, gaps)
	}
}

func TestEntryValidationRejectsIncompleteOrContradictoryEvidence(t *testing.T) {
	root := fixtureRoot(t)
	now := mustTime(t, "2026-08-31T07:00:00Z")
	tests := []struct {
		name   string
		mutate func(*entry, *model.ToolCapability)
		want   string
	}{
		{"invalid status", func(e *entry, _ *model.ToolCapability) { e.Status = "claimed" }, "status = \"claimed\""},
		{"empty harness", func(e *entry, _ *model.ToolCapability) { e.Harness = " " }, "harness is empty"},
		{"unrecorded goos", func(e *entry, _ *model.ToolCapability) { e.GOOS = "unknown" }, "goos was not recorded"},
		{"wrong trap count", func(e *entry, _ *model.ToolCapability) { e.Traps = e.Traps[:2] }, "trap declarations = 2"},
		{"ambiguous trap", func(e *entry, _ *model.ToolCapability) { e.Traps[0].NotApplicable = "also set" }, "must name exactly one"},
		{"missing named trap", func(e *entry, _ *model.ToolCapability) { e.Traps[0].Name = "other" }, "missing trap declaration \"split-identity\""},
		{"pending without gaps", func(e *entry, _ *model.ToolCapability) { e.Status = "pending" }, "pending entry has no explicit gaps"},
		{"ready with declared gaps", func(e *entry, _ *model.ToolCapability) { e.Gaps = []string{"not done"} }, "ready entry still declares gaps"},
		{"missing version", func(e *entry, _ *model.ToolCapability) { e.Version = "" }, "exact harness version"},
		{"missing capture time", func(e *entry, _ *model.ToolCapability) { e.CapturedAt = "" }, "UTC capture time is missing"},
		{"missing recipe", func(e *entry, _ *model.ToolCapability) { e.Recipe = nil }, "bounded capture recipe is missing"},
		{"missing sanitization", func(e *entry, _ *model.ToolCapability) { e.Sanitization = "" }, "sanitization method is missing"},
		{"no surfaces", func(e *entry, _ *model.ToolCapability) { e.Surfaces = nil }, "no evidence surface is recorded"},
		{"surface has no anchors", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].RequiredAnchors = nil }, "has no discriminator or required anchors"},
		{"unsupported origin", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].Origin = "guessed" }, "origin = \"guessed\""},
		{"fixture outside testdata", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].Fixture = "pkg/live.jsonl" }, "is not a repository testdata path"},
		{"missing fixture", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].Fixture = "pkg/testdata/missing.jsonl" }, "no such file"},
		{"missing oracle", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].Oracle = "" }, "has no oracle test"},
		{"unknown oracle", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].Oracle = "TestAbsent" }, "does not exist"},
		{"constructed usage", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].Origin = "constructed" }, "no nonzero live usage evidence"},
		{"missing exact join", func(e *entry, _ *model.ToolCapability) { e.Surfaces[0].Evidence.JoinedActivity = false }, "exact-join capability lacks"},
		{"missing unattributed call", func(e *entry, c *model.ToolCapability) {
			c.Activity = model.ActivityUnattributed
			e.Surfaces[0].Evidence.JoinedActivity = false
		}, "unattributed activity capability lacks"},
		{"missing reasoning proof", func(e *entry, c *model.ToolCapability) {
			c.Reasoning = model.ReasoningReportSubset
		}, "reasoning declaration lacks"},
		{"crush without live cost", func(e *entry, c *model.ToolCapability) {
			e.Tool = model.ToolCrush
			c.Tool = model.ToolCrush
			c.Activity = model.ActivityNone
		}, "no nonzero live vendor-cost evidence"},
		{"future capture", func(e *entry, _ *model.ToolCapability) { e.CapturedAt = "2026-09-01T07:00:00Z" }, "capture time is in the future"},
		{"non-UTC capture", func(e *entry, _ *model.ToolCapability) { e.CapturedAt = "2026-08-31T09:00:00+02:00" }, "must use UTC Z form"},
		{"malformed capture", func(e *entry, _ *model.ToolCapability) { e.CapturedAt = "2026-08-31" }, "is not RFC3339 UTC"},
		{"stale release capture", func(e *entry, _ *model.ToolCapability) { e.CapturedAt = "2026-07-01T07:00:00Z" }, "capture is older than 30 days"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, cap := readyEntry(), clineCapability()
			tc.mutate(&e, &cap)
			errs, gaps := validateEntry(root, e, cap, "release", now)
			all := append(append([]string{}, errs...), gaps...)
			if !containsSubstring(all, tc.want) {
				t.Fatalf("validation output %v does not contain %q", all, tc.want)
			}
		})
	}
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg", "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "testdata", "live.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "oracle_test.go"), []byte("package pkg\nfunc TestOracle() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func readyEntry() entry {
	return entry{
		Tool:         model.ToolCline,
		Harness:      "Cline CLI",
		Version:      "3.0.55",
		CapturedAt:   "2026-08-31T06:00:00Z",
		GOOS:         "linux",
		GOARCH:       "amd64",
		Status:       "ready",
		Recipe:       []string{"run one bounded turn"},
		Sanitization: "replace content and retain counters",
		Surfaces: []surface{{
			Name:            "messages",
			Origin:          "live",
			Fixture:         "pkg/testdata/live.jsonl",
			Package:         "./pkg",
			Oracle:          "TestOracle",
			Discriminator:   "assistant message",
			RequiredAnchors: []string{"metrics"},
			Evidence: evidence{
				NonzeroUsage:   true,
				JoinedActivity: true,
			},
		}},
		Traps: []trap{
			{Name: "split-identity", Package: "./pkg", Test: "TestOracle"},
			{Name: "cumulative-vs-event", Package: "./pkg", Test: "TestOracle"},
			{Name: "assigned-not-accumulated", Package: "./pkg", Test: "TestOracle"},
		},
	}
}

func clineCapability() model.ToolCapability {
	return model.ToolCapability{
		Tool:      model.ToolCline,
		Cost:      model.CostComputed,
		Activity:  model.ActivityExact,
		Reasoning: model.ReasoningReportNone,
		Tier:      model.TierLive,
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func pendingFor(t *testing.T, got result, tool string) pendingEntry {
	t.Helper()
	for _, p := range got.Pending {
		if p.Tool == tool {
			return p
		}
	}
	t.Fatalf("no pending entry for %q", tool)
	return pendingEntry{}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
