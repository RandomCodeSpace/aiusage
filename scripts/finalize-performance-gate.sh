#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
readonly repo_root
candidate_sha="$(git rev-parse HEAD)"
readonly candidate_sha
readonly artifact_dir="${AIUSAGE_PERF_OUT:-$repo_root/performance}"
readonly primary_dir="${AIUSAGE_PERF_PRIMARY:-$repo_root/performance-primary}"
readonly retry_dir="${AIUSAGE_PERF_RETRY:-$repo_root/performance-retry}"
readonly export_dir="${AIUSAGE_PERF_EXPORTS:-$repo_root/performance-exports}"
readonly first_baseline="9abd18cefe4f7cb8c556b9c17afa02abe156258b"

baseline_sha="$first_baseline"
if git rev-parse -q --verify refs/tags/v0.5.0 >/dev/null; then
	baseline_tag="$(git tag --merged "$candidate_sha" --sort=-version:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ {print; exit}')"
	if [[ -z "$baseline_tag" ]]; then
		echo "v0.5.0 exists but no public release tag is reachable from the candidate" >&2
		exit 2
	fi
	baseline_sha="$(git rev-list -n 1 "$baseline_tag")"
fi

require_file() {
	if [[ ! -s "$1" ]]; then
		echo "missing performance evidence: $1" >&2
		exit 1
	fi
}

validate_shard() {
	local root="$1" set="$2" start="$3" end="$4"
	require_file "$root/run.json"
	require_file "$root/baseline/query/full.json"
	require_file "$root/candidate/query/full.json"
	require_file "$root/benchmarks/baseline.txt"
	require_file "$root/benchmarks/candidate.txt"
	if ! jq -e \
		--arg baseline "$baseline_sha" --arg candidate "$candidate_sha" --arg set "$set" \
		--argjson start "$start" --argjson end "$end" \
		'.schema == "production-performance-run-v1" and
		 .baseline == $baseline and .candidate == $candidate and
		 .scale_divisor == 1 and .exports_included == false and
		 .sample_set == $set and .sample_start == $start and .sample_end == $end' \
		"$root/run.json" >/dev/null; then
		echo "invalid $set performance shard manifest: $root/run.json" >&2
		exit 1
	fi
}

validate_shard "$primary_dir" primary 1 10
validate_shard "$retry_dir" retry 11 20

# Both jobs regenerate the same fixed-seed fixtures. Comparing their manifests
# prevents a retry from being combined with a different semantic workload.
for name in source-farm.json source-farm-contract.json; do
	require_file "$primary_dir/fixtures/$name"
	require_file "$retry_dir/fixtures/$name"
	if ! cmp -s "$primary_dir/fixtures/$name" "$retry_dir/fixtures/$name"; then
		echo "performance shard fixture mismatch: $name" >&2
		exit 1
	fi
done
for name in baseline-1m.json candidate-1m.json baseline-100k.json candidate-100k.json; do
	require_file "$primary_dir/fixtures/$name"
	require_file "$retry_dir/fixtures/$name"
	# schema_meta timestamps make the raw SQLite file hash nondeterministic even
	# when the fixed seed, clock, cardinalities, boundaries, and bytes agree.
	# Compare every semantic manifest field and exclude only that opaque hash.
	if ! diff -q \
		<(jq -S 'del(.database_sha256)' "$primary_dir/fixtures/$name") \
		<(jq -S 'del(.database_sha256)' "$retry_dir/fixtures/$name") >/dev/null; then
		echo "performance shard fixture mismatch: $name" >&2
		exit 1
	fi
done

for command in export-json export-csv export-json-raw export-csv-raw; do
	require_file "$export_dir/candidate/process/${command}-1m.json"
done

mkdir -p "$artifact_dir"
if find "$artifact_dir" -mindepth 1 -print -quit | grep -q .; then
	echo "combined performance artifact directory is not empty: $artifact_dir" >&2
	exit 2
fi
cp -a "$primary_dir/." "$artifact_dir/"
cp -a "$export_dir/candidate/process/." "$artifact_dir/candidate/process/"

perfcheck="$(mktemp /tmp/aiusage-perfcheck.XXXXXX)"
cleanup() {
	rm -f -- "$perfcheck"
}
trap cleanup EXIT
CGO_ENABLED=0 go build -o "$perfcheck" ./internal/perfcheck

evaluate() {
	local root="$1"
	benchstat "$root/benchmarks/baseline.txt" "$root/benchmarks/candidate.txt" \
		>"$root/benchmarks/benchstat.txt"
	"$perfcheck" \
		--baseline "$root/baseline" --candidate "$root/candidate" \
		--baseline-bench "$root/benchmarks/baseline.txt" \
		--candidate-bench "$root/benchmarks/candidate.txt" \
		--out "$root/result.json" | tee "$root/report.md"
}

write_run_manifest() {
	local retry_used="$1" samples="$2"
	local combined_set="primary"
	if [[ "$retry_used" == "true" ]]; then
		combined_set="primary+retry"
	fi
	cat >"$artifact_dir/run.json" <<EOF
{"schema":"production-performance-run-v1","baseline":"$baseline_sha","candidate":"$candidate_sha","scale_divisor":1,"runner":"ubuntu-24.04","cgo_enabled":false,"gomaxprocs":2,"exports_included":true,"sample_set":"$combined_set","paired_samples":$samples,"retry_used":$retry_used}
EOF
}

primary_status=0
if evaluate "$artifact_dir"; then
	primary_status=0
else
	primary_status=$?
fi
if [[ "$primary_status" == "0" ]]; then
	write_run_manifest false 10
	exit 0
fi

if ! jq -e '[.results[] | select(.status == "FAIL")] as $f |
	($f|length) > 0 and all($f[]; .category == "regression" and (.detail | startswith("median ratio")))' \
	"$artifact_dir/result.json" >/dev/null; then
	write_run_manifest false 10
	exit "$primary_status"
fi

echo "relative timing regression only; combining the one permitted retry set" >&2
cp "$artifact_dir/result.json" "$artifact_dir/primary-result.json"
cp "$artifact_dir/report.md" "$artifact_dir/primary-report.md"
cp "$artifact_dir/benchmarks/benchstat.txt" "$artifact_dir/benchmarks/primary-benchstat.txt"
cp -a "$retry_dir/baseline/process/." "$artifact_dir/baseline/process/"
cp -a "$retry_dir/candidate/process/." "$artifact_dir/candidate/process/"
cp "$retry_dir/baseline/query"/timed-*.json "$artifact_dir/baseline/query/"
cp "$retry_dir/candidate/query"/timed-*.json "$artifact_dir/candidate/query/"
cat "$retry_dir/benchmarks/baseline.txt" >>"$artifact_dir/benchmarks/baseline.txt"
cat "$retry_dir/benchmarks/candidate.txt" >>"$artifact_dir/benchmarks/candidate.txt"

combined_status=0
if evaluate "$artifact_dir"; then
	combined_status=0
else
	combined_status=$?
fi
write_run_manifest true 20
exit "$combined_status"
