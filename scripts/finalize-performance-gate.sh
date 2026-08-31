#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
readonly repo_root
candidate_sha="$(git rev-parse HEAD)"
readonly candidate_sha
readonly artifact_dir="${AIUSAGE_PERF_OUT:-$repo_root/performance}"
readonly first_baseline="9abd18cefe4f7cb8c556b9c17afa02abe156258b"

for command in export-json export-csv export-json-raw export-csv-raw; do
	path="$artifact_dir/candidate/process/${command}-1m.json"
	if [[ ! -s "$path" ]]; then
		echo "missing export performance evidence: $path" >&2
		exit 1
	fi
done
for path in \
	"$artifact_dir/baseline/query/full.json" \
	"$artifact_dir/candidate/query/full.json" \
	"$artifact_dir/benchmarks/baseline.txt" \
	"$artifact_dir/benchmarks/candidate.txt"; do
	if [[ ! -s "$path" ]]; then
		echo "missing paired performance evidence: $path" >&2
		exit 1
	fi
done

baseline_sha="$first_baseline"
if git rev-parse -q --verify refs/tags/v0.5.0 >/dev/null; then
	baseline_tag="$(git tag --merged "$candidate_sha" --sort=-version:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ {print; exit}')"
	if [[ -z "$baseline_tag" ]]; then
		echo "v0.5.0 exists but no public release tag is reachable from the candidate" >&2
		exit 2
	fi
	baseline_sha="$(git rev-list -n 1 "$baseline_tag")"
fi

perfcheck="$(mktemp /tmp/aiusage-perfcheck.XXXXXX)"
cleanup() {
	rm -f -- "$perfcheck"
}
trap cleanup EXIT

CGO_ENABLED=0 go build -o "$perfcheck" ./internal/perfcheck
benchstat "$artifact_dir/benchmarks/baseline.txt" "$artifact_dir/benchmarks/candidate.txt" \
	>"$artifact_dir/benchmarks/benchstat.txt"
"$perfcheck" \
	--baseline "$artifact_dir/baseline" --candidate "$artifact_dir/candidate" \
	--baseline-bench "$artifact_dir/benchmarks/baseline.txt" \
	--candidate-bench "$artifact_dir/benchmarks/candidate.txt" \
	--out "$artifact_dir/result.json" | tee "$artifact_dir/report.md"

cat >"$artifact_dir/run.json" <<EOF
{"schema":"production-performance-run-v1","baseline":"$baseline_sha","candidate":"$candidate_sha","scale_divisor":1,"runner":"ubuntu-24.04","cgo_enabled":false,"gomaxprocs":2,"exports_included":true}
EOF
