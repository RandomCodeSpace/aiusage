// Command adaptercompatcheck validates the repository-owned live-adapter
// evidence manifest. CI mode checks structure and registry coverage while
// preserving explicit pending evidence. Release mode additionally requires all
// 15 entries to be fresh and complete.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/adapter/all"
	"github.com/RandomCodeSpace/aiusage/model"
)

const (
	manifestSchema = "adapter-compatibility-v1"
	resultSchema   = "adapter-compatibility-result-v1"
)

type manifest struct {
	Schema  string  `json:"schema"`
	Entries []entry `json:"entries"`
}

type entry struct {
	Tool         string    `json:"tool"`
	Harness      string    `json:"harness"`
	Version      string    `json:"version"`
	CapturedAt   string    `json:"captured_at"`
	GOOS         string    `json:"goos"`
	GOARCH       string    `json:"goarch"`
	Status       string    `json:"status"`
	Recipe       []string  `json:"recipe"`
	Sanitization string    `json:"sanitization"`
	Surfaces     []surface `json:"surfaces"`
	Traps        []trap    `json:"traps"`
	Gaps         []string  `json:"gaps,omitempty"`
}

type surface struct {
	Name            string   `json:"name"`
	Origin          string   `json:"origin"`
	Fixture         string   `json:"fixture"`
	Package         string   `json:"package"`
	Oracle          string   `json:"oracle"`
	Discriminator   string   `json:"discriminator"`
	RequiredAnchors []string `json:"required_anchors"`
	Evidence        evidence `json:"evidence"`
}

type evidence struct {
	NonzeroUsage         bool `json:"nonzero_usage"`
	NonzeroVendorCost    bool `json:"nonzero_vendor_cost"`
	JoinedActivity       bool `json:"joined_activity"`
	UnattributedActivity bool `json:"unattributed_activity"`
	TurnContext          bool `json:"turn_context"`
	Reasoning            bool `json:"reasoning"`
}

type trap struct {
	Name          string `json:"name"`
	Package       string `json:"package,omitempty"`
	Test          string `json:"test,omitempty"`
	NotApplicable string `json:"not_applicable,omitempty"`
}

type pendingEntry struct {
	Tool string   `json:"tool"`
	Gaps []string `json:"gaps"`
}

type result struct {
	Schema         string         `json:"schema"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	CheckedAt      string         `json:"checked_at"`
	Mode           string         `json:"mode"`
	Registered     int            `json:"registered"`
	Ready          int            `json:"ready"`
	Complete       bool           `json:"complete"`
	Pending        []pendingEntry `json:"pending"`
	Errors         []string       `json:"errors"`
}

func main() {
	manifestPath := flag.String("manifest", "adapter/compatibility.json", "manifest path relative to repository root")
	repoRoot := flag.String("root", ".", "repository root")
	mode := flag.String("mode", "ci", "validation mode: ci or release")
	outPath := flag.String("out", "", "optional result JSON path")
	nowText := flag.String("now", "", "validation clock in RFC3339 (tests only)")
	flag.Parse()

	now := time.Now().UTC()
	if *nowText != "" {
		var err error
		now, err = time.Parse(time.RFC3339, *nowText)
		if err != nil {
			fatalf("parse --now: %v", err)
		}
	}
	if *mode != "ci" && *mode != "release" {
		fatalf("unknown mode %q", *mode)
	}

	m, raw, err := readManifest(filepath.Join(*repoRoot, *manifestPath))
	if err != nil {
		fatalf("read manifest: %v", err)
	}
	r := validate(*repoRoot, m, raw, *mode, now)
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fatalf("encode result: %v", err)
	}
	encoded = append(encoded, '\n')
	if *outPath == "" {
		_, _ = os.Stdout.Write(encoded)
	} else if err := os.WriteFile(filepath.Join(*repoRoot, *outPath), encoded, 0o644); err != nil {
		fatalf("write result: %v", err)
	}
	fmt.Fprintf(os.Stderr, "adapter compatibility: %d/%d ready, complete=%v\n", r.Ready, r.Registered, r.Complete)
	if len(r.Errors) > 0 {
		for _, msg := range r.Errors {
			fmt.Fprintln(os.Stderr, "-", msg)
		}
		os.Exit(1)
	}
}

func readManifest(path string) (manifest, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return manifest{}, nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return manifest{}, nil, err
		}
		return manifest{}, nil, errors.New("manifest contains trailing JSON values")
	}
	return m, raw, nil
}

func validate(root string, m manifest, raw []byte, mode string, now time.Time) result {
	sum := sha256.Sum256(raw)
	r := result{
		Schema:         resultSchema,
		ManifestSHA256: hex.EncodeToString(sum[:]),
		CheckedAt:      now.UTC().Format(time.RFC3339),
		Mode:           mode,
		Errors:         []string{},
		Pending:        []pendingEntry{},
	}
	if m.Schema != manifestSchema {
		r.Errors = append(r.Errors, fmt.Sprintf("schema = %q, want %q", m.Schema, manifestSchema))
	}

	reg := all.Default()
	caps := reg.Capabilities()
	r.Registered = len(reg.All())
	want := make(map[string]bool, r.Registered)
	for _, a := range reg.All() {
		want[a.ID()] = true
		cap := a.Capabilities()
		if cap.Tool != a.ID() || cap.Tier != model.TierLive || cap.Cost == "" || cap.Activity == "" || cap.Reasoning == "" {
			r.Errors = append(r.Errors, fmt.Sprintf("%s has an incomplete or non-live capability declaration: %+v", a.ID(), cap))
		}
	}
	if len(m.Entries) != len(want) {
		r.Errors = append(r.Errors, fmt.Sprintf("manifest entries = %d, registry entries = %d", len(m.Entries), len(want)))
	}

	seen := map[string]bool{}
	for _, e := range m.Entries {
		if seen[e.Tool] {
			r.Errors = append(r.Errors, fmt.Sprintf("duplicate tool entry %q", e.Tool))
			continue
		}
		seen[e.Tool] = true
		if !want[e.Tool] {
			r.Errors = append(r.Errors, fmt.Sprintf("manifest tool %q is not registered", e.Tool))
			continue
		}
		entryErrors, gaps := validateEntry(root, e, caps[e.Tool], mode, now)
		for _, msg := range entryErrors {
			r.Errors = append(r.Errors, e.Tool+": "+msg)
		}
		if e.Status == "ready" && len(gaps) == 0 {
			r.Ready++
		} else {
			if len(gaps) == 0 {
				gaps = []string{"entry is not ready"}
			}
			r.Pending = append(r.Pending, pendingEntry{Tool: e.Tool, Gaps: gaps})
		}
	}
	for id := range want {
		if !seen[id] {
			r.Errors = append(r.Errors, fmt.Sprintf("registered tool %q has no manifest entry", id))
		}
	}
	sort.Slice(r.Pending, func(i, j int) bool { return r.Pending[i].Tool < r.Pending[j].Tool })
	r.Complete = len(r.Errors) == 0 && len(r.Pending) == 0 && r.Ready == r.Registered
	return r
}

func validateEntry(root string, e entry, cap model.ToolCapability, mode string, now time.Time) ([]string, []string) {
	var structural, gaps []string
	if e.Status != "ready" && e.Status != "pending" {
		structural = append(structural, fmt.Sprintf("status = %q, want ready or pending", e.Status))
	}
	if strings.TrimSpace(e.Harness) == "" {
		structural = append(structural, "harness is empty")
	}
	for label, value := range map[string]string{"goos": e.GOOS, "goarch": e.GOARCH} {
		if evidenceValueMissing(value) {
			gaps = append(gaps, label+" was not recorded at capture time")
		}
	}
	if len(e.Traps) != 3 {
		structural = append(structural, fmt.Sprintf("trap declarations = %d, want 3", len(e.Traps)))
	}
	trapNames := map[string]bool{}
	for _, tr := range e.Traps {
		trapNames[tr.Name] = true
		if (tr.Test == "") == (tr.NotApplicable == "") {
			structural = append(structural, fmt.Sprintf("trap %q must name exactly one test or source-backed not-applicable reason", tr.Name))
		}
		if tr.Test != "" {
			structural = append(structural, validateTest(root, tr.Package, tr.Test)...)
		}
	}
	for _, name := range []string{"split-identity", "cumulative-vs-event", "assigned-not-accumulated"} {
		if !trapNames[name] {
			structural = append(structural, fmt.Sprintf("missing trap declaration %q", name))
		}
	}

	if e.Status == "pending" {
		if len(e.Gaps) == 0 {
			structural = append(structural, "pending entry has no explicit gaps")
		}
		gaps = append(gaps, e.Gaps...)
	} else if len(e.Gaps) != 0 {
		structural = append(structural, "ready entry still declares gaps")
	}
	if e.Version == "" {
		gaps = append(gaps, "exact harness version or revision is missing")
	}
	if e.CapturedAt == "" {
		gaps = append(gaps, "UTC capture time is missing")
	}
	if len(e.Recipe) == 0 {
		gaps = append(gaps, "bounded capture recipe is missing")
	}
	if e.Sanitization == "" {
		gaps = append(gaps, "sanitization method is missing")
	}
	if len(e.Surfaces) == 0 {
		gaps = append(gaps, "no evidence surface is recorded")
	}

	var live evidence
	var reasoning bool
	for _, s := range e.Surfaces {
		if s.Name == "" || s.Discriminator == "" || len(s.RequiredAnchors) == 0 {
			structural = append(structural, fmt.Sprintf("surface %q has no discriminator or required anchors", s.Name))
		}
		if s.Origin != "live" && s.Origin != "writer" && s.Origin != "constructed" {
			structural = append(structural, fmt.Sprintf("surface %q origin = %q", s.Name, s.Origin))
		}
		if s.Fixture != "" {
			structural = append(structural, validateFixture(root, s.Fixture)...)
		} else {
			gaps = append(gaps, fmt.Sprintf("surface %q has no repository fixture", s.Name))
		}
		if s.Oracle != "" {
			structural = append(structural, validateTest(root, s.Package, s.Oracle)...)
		} else {
			gaps = append(gaps, fmt.Sprintf("surface %q has no oracle test", s.Name))
		}
		if s.Origin != "live" {
			gaps = append(gaps, fmt.Sprintf("surface %q is %s, not live-origin", s.Name, s.Origin))
		}
		if s.Origin == "live" {
			live.NonzeroUsage = live.NonzeroUsage || s.Evidence.NonzeroUsage
			live.NonzeroVendorCost = live.NonzeroVendorCost || s.Evidence.NonzeroVendorCost
			live.JoinedActivity = live.JoinedActivity || s.Evidence.JoinedActivity
			live.UnattributedActivity = live.UnattributedActivity || s.Evidence.UnattributedActivity
			live.TurnContext = live.TurnContext || s.Evidence.TurnContext
		}
		if s.Origin == "live" || s.Origin == "writer" {
			reasoning = reasoning || s.Evidence.Reasoning
		}
	}
	if e.Tool == model.ToolCrush {
		if !live.NonzeroVendorCost {
			gaps = append(gaps, "no nonzero live vendor-cost evidence")
		}
	} else if !live.NonzeroUsage {
		gaps = append(gaps, "no nonzero live usage evidence")
	}
	if cap.Activity == model.ActivityExact && !live.JoinedActivity {
		gaps = append(gaps, "exact-join capability lacks joined activity evidence")
	}
	if cap.Activity == model.ActivityUnattributed && !live.UnattributedActivity {
		gaps = append(gaps, "unattributed activity capability lacks evidence")
	}
	if cap.Reasoning != model.ReasoningReportNone && !reasoning {
		gaps = append(gaps, "reasoning declaration lacks nonzero live or versioned-writer evidence")
	}

	if e.CapturedAt != "" {
		captured, err := time.Parse(time.RFC3339, e.CapturedAt)
		if err != nil {
			structural = append(structural, "captured_at is not RFC3339 UTC")
		} else {
			if captured.Location() != time.UTC || !strings.HasSuffix(e.CapturedAt, "Z") {
				structural = append(structural, "captured_at must use UTC Z form")
			}
			if captured.After(now) {
				structural = append(structural, "capture time is in the future")
			}
			if mode == "release" && now.Sub(captured) > 30*24*time.Hour {
				gaps = append(gaps, "capture is older than 30 days")
			}
		}
	}
	gaps = unique(gaps)
	if mode == "release" {
		for _, gap := range gaps {
			structural = append(structural, "release blocker: "+gap)
		}
	}
	return unique(structural), gaps
}

func evidenceValueMissing(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "unknown", "unrecorded", "pending":
		return true
	default:
		return false
	}
}

func validateFixture(root, name string) []string {
	clean := filepath.Clean(name)
	if filepath.IsAbs(name) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		!strings.Contains(filepath.ToSlash(clean), "/testdata/") {
		return []string{fmt.Sprintf("fixture %q is not a repository testdata path", name)}
	}
	if _, err := os.Stat(filepath.Join(root, clean)); err != nil {
		return []string{fmt.Sprintf("fixture %q: %v", name, err)}
	}
	return nil
}

func validateTest(root, pkg, name string) []string {
	clean := filepath.Clean(strings.TrimPrefix(pkg, "./"))
	if pkg == "" || name == "" || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return []string{fmt.Sprintf("invalid oracle reference %q.%q", pkg, name)}
	}
	want := []byte("func " + name + "(")
	found := false
	err := filepath.WalkDir(filepath.Join(root, clean), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != filepath.Join(root, clean) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found = found || bytes.Contains(body, want)
		return nil
	})
	if err != nil {
		return []string{fmt.Sprintf("oracle package %q: %v", pkg, err)}
	}
	if !found {
		return []string{fmt.Sprintf("oracle %s.%s does not exist", pkg, name)}
	}
	return nil
}

func unique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
