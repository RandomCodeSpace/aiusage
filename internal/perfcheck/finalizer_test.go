package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinalizerComparesSemanticFixtureFields(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("finalizer requires bash")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("finalizer requires jq")
	}
	script, err := filepath.Abs("../../scripts/finalize-performance-gate.sh")
	if err != nil {
		t.Fatal(err)
	}
	const candidate = "1111111111111111111111111111111111111111"
	for _, tc := range []struct {
		name, field string
		value       any
		accepted    bool
	}{
		{"identical", "", nil, true},
		{"physical size", "database_bytes", 16384, true},
		{"physical hash", "database_sha256", "other-pages", true},
		{"usage", "usage_events", 101, false},
		{"schema", "schema_version", 8, false},
		{"rollup", "rollup_rows", 21, false},
		{"seed", "seed", 1235, false},
		{"raw bytes", "raw_bytes_per_event", 513, false},
		{"additional semantic field", "extra_contract", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, body string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(root, "bin")
			// Exercise the actual finalizer and jq comparison. Build/evaluation
			// are stubbed because this regression concerns shard admission only.
			write(filepath.Join(bin, "git"), `#!/usr/bin/env bash
case "$*" in
  "rev-parse --show-toplevel") printf '%s\n' "$AIUSAGE_TEST_ROOT" ;;
  "rev-parse HEAD") printf '%s\n' "$AIUSAGE_TEST_CANDIDATE" ;;
  "rev-parse -q --verify refs/tags/v0.5.0") exit 1 ;;
  *) exit 99 ;;
esac
`, 0o755)
			write(filepath.Join(bin, "go"), `#!/usr/bin/env bash
set -eu
[[ "$1" == build && "$2" == -o && "$4" == ./internal/perfcheck ]]
printf '#!/usr/bin/env bash\nexit 0\n' >"$3"
chmod +x "$3"
`, 0o755)
			write(filepath.Join(bin, "benchstat"), "#!/usr/bin/env bash\nexit 0\n", 0o755)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AIUSAGE_TEST_ROOT", root)
			t.Setenv("AIUSAGE_TEST_CANDIDATE", candidate)
			for _, side := range []string{"primary", "retry"} {
				shard := filepath.Join(root, side)
				for _, file := range []string{"baseline/query/full.json", "candidate/query/full.json", "benchmarks/baseline.txt", "benchmarks/candidate.txt", "fixtures/source-farm.json", "fixtures/source-farm-contract.json"} {
					write(filepath.Join(shard, file), "{}\n", 0o600)
				}
				start, end := 1, 10
				if side == "retry" {
					start, end = 11, 20
				}
				writeTestJSON(t, filepath.Join(shard, "run.json"), map[string]any{
					"schema": "production-performance-run-v1", "baseline": "9abd18cefe4f7cb8c556b9c17afa02abe156258b",
					"candidate": candidate, "scale_divisor": 1, "exports_included": false,
					"sample_set": side, "sample_start": start, "sample_end": end,
				})
				for _, name := range []string{"baseline-1m.json", "candidate-1m.json", "baseline-100k.json", "candidate-100k.json"} {
					manifest := map[string]any{
						"seed": 1234, "schema_version": 9, "usage_events": 100,
						"rollup_rows": 20, "raw_bytes_per_event": 512,
						"database_bytes": 12288, "database_sha256": "original-pages",
					}
					if side == "retry" && name == "candidate-1m.json" && tc.field != "" {
						manifest[tc.field] = tc.value
					}
					writeTestJSON(t, filepath.Join(shard, "fixtures", name), manifest)
				}
			}
			exports := filepath.Join(root, "exports")
			for _, command := range []string{"export-json", "export-csv", "export-json-raw", "export-csv-raw"} {
				write(filepath.Join(exports, "candidate", "process", command+"-1m.json"), "{}\n", 0o600)
			}
			output := filepath.Join(root, "combined")
			t.Setenv("AIUSAGE_PERF_OUT", output)
			t.Setenv("AIUSAGE_PERF_PRIMARY", filepath.Join(root, "primary"))
			t.Setenv("AIUSAGE_PERF_RETRY", filepath.Join(root, "retry"))
			t.Setenv("AIUSAGE_PERF_EXPORTS", exports)
			command := exec.CommandContext(t.Context(), bash, script)
			command.Dir = root
			body, err := command.CombinedOutput()
			if tc.accepted {
				if err != nil {
					t.Fatalf("physical metadata blocked matching semantics: %v\n%s", err, body)
				}
				for _, path := range []string{filepath.Join(output, "fixtures", "candidate-1m.json"), filepath.Join(root, "retry", "fixtures", "candidate-1m.json")} {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var manifest map[string]any
					if err := json.Unmarshal(data, &manifest); err != nil || manifest["database_bytes"] == nil || manifest["database_sha256"] == nil {
						t.Fatalf("physical metadata was removed from artifact: %s, %v", data, err)
					}
				}
			} else if err == nil || !strings.Contains(string(body), "performance shard fixture mismatch: candidate-1m.json") {
				t.Fatalf("semantic mismatch accepted or wrong refusal: %v\n%s", err, body)
			}
		})
	}
}
