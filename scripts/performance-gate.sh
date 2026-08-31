#!/usr/bin/env bash
set -euo pipefail

readonly first_baseline="9abd18cefe4f7cb8c556b9c17afa02abe156258b"
readonly repo_root="$(git rev-parse --show-toplevel)"
readonly candidate_sha="$(git rev-parse HEAD)"
readonly artifact_dir="${AIUSAGE_PERF_OUT:-$repo_root/performance}"
readonly scale="${AIUSAGE_PERF_SCALE:-1}"
readonly skip_exports="${AIUSAGE_PERF_SKIP_EXPORTS:-0}"
readonly sample_set="${AIUSAGE_PERF_SAMPLE_SET:-standalone}"
readonly perf_version_xflag="-X=github.com/RandomCodeSpace/aiusage/internal/buildinfo.Version=v0.0.0-performance"

if ! [[ "$scale" =~ ^[1-9][0-9]*$ ]]; then
	echo "AIUSAGE_PERF_SCALE must be a positive integer divisor" >&2
	exit 2
fi
if [[ "$skip_exports" != "0" && "$skip_exports" != "1" ]]; then
	echo "AIUSAGE_PERF_SKIP_EXPORTS must be 0 or 1" >&2
	exit 2
fi

case "$sample_set" in
standalone | primary)
	sample_start=1
	sample_end=10
	;;
retry)
	sample_start=11
	sample_end=20
	;;
*)
	echo "AIUSAGE_PERF_SAMPLE_SET must be standalone, primary, or retry" >&2
	exit 2
	;;
esac
warmup_sample="$sample_start"

baseline_sha="$first_baseline"
if git rev-parse -q --verify refs/tags/v0.5.0 >/dev/null; then
	baseline_tag="$(git tag --merged "$candidate_sha" --sort=-version:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ {print; exit}')"
	if [[ -z "$baseline_tag" ]]; then
		echo "v0.5.0 exists but no public release tag is reachable from the candidate" >&2
		exit 2
	fi
	baseline_sha="$(git rev-list -n 1 "$baseline_tag")"
fi

mkdir -p "$artifact_dir"
if find "$artifact_dir" -mindepth 1 -print -quit | grep -q .; then
	echo "performance artifact directory is not empty: $artifact_dir" >&2
	exit 2
fi

perf_root="$(mktemp -d /tmp/aiusage-performance.XXXXXX)"
baseline_tree="$perf_root/baseline"
candidate_tree="$perf_root/candidate"

cleanup() {
	git -C "$repo_root" worktree remove --force "$baseline_tree" >/dev/null 2>&1 || true
	git -C "$repo_root" worktree remove --force "$candidate_tree" >/dev/null 2>&1 || true
	rm -rf -- "$perf_root"
}
trap cleanup EXIT

git -C "$repo_root" worktree add --detach "$baseline_tree" "$baseline_sha"
git -C "$repo_root" worktree add --detach "$candidate_tree" "$candidate_sha"

# The harness is candidate-owned but intentionally uses only APIs present at
# the frozen baseline. Copying it into the old tree makes both sides generate
# the same semantic data in their own native schema.
mkdir -p "$baseline_tree/internal/perfdriver"
cp "$candidate_tree"/internal/perfdriver/*.go "$baseline_tree/internal/perfdriver/"
cp "$candidate_tree/internal/tui/production_bench_test.go" "$baseline_tree/internal/tui/production_bench_test.go"
cp "$candidate_tree/internal/tui/cyclebench_test.go" "$baseline_tree/internal/tui/cyclebench_test.go"

mkdir -p \
	"$artifact_dir/baseline/process" "$artifact_dir/baseline/query" \
	"$artifact_dir/candidate/process" "$artifact_dir/candidate/query" \
	"$artifact_dir/fixtures" "$artifact_dir/benchmarks"

usage_count=$((1000000 / scale))
activity_count=$((250000 / scale))
context_count=$((100000 / scale))
small_usage=$((100000 / scale))
small_activity=$((25000 / scale))
small_context=$((10000 / scale))
usage_count=$((usage_count > 1000 ? usage_count : 1000))
activity_count=$((activity_count > 250 ? activity_count : 250))
context_count=$((context_count > 100 ? context_count : 100))
small_usage=$((small_usage > 100 ? small_usage : 100))
small_activity=$((small_activity > 25 ? small_activity : 25))
small_context=$((small_context > 10 ? small_context : 10))

export CGO_ENABLED=0
export GOMAXPROCS=2
export TZ=UTC
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false -ldflags=$perf_version_xflag"
readonly go_bin="$(command -v go)"

(cd "$baseline_tree" && go build -o "$perf_root/baseline-aiusage" .)
(cd "$candidate_tree" && go build -o "$perf_root/candidate-aiusage" .)
(cd "$baseline_tree" && go build -o "$perf_root/baseline-driver" ./internal/perfdriver)
(cd "$candidate_tree" && go build -o "$perf_root/candidate-driver" ./internal/perfdriver)
(cd "$candidate_tree" && go build -o "$perf_root/perfcheck" ./internal/perfcheck)
(cd "$baseline_tree" && go test -c -o "$perf_root/baseline-tui.test" ./internal/tui)
(cd "$candidate_tree" && go test -c -o "$perf_root/candidate-tui.test" ./internal/tui)

(cd "$baseline_tree" && "$go_bin" test -c -o "$perf_root/baseline-perfdriver.test" ./internal/perfdriver)
(cd "$candidate_tree" && "$go_bin" test -c -o "$perf_root/candidate-perfdriver.test" ./internal/perfdriver)

"$perf_root/candidate-driver" generate-source-farm \
	--root "$perf_root/source-farm" --fixtures "$candidate_tree" \
	--manifest "$artifact_dir/fixtures/source-farm.json" \
	>"$artifact_dir/fixtures/source-farm.stdout.json"
"$perf_root/baseline-driver" source-farm-warm \
	--root "$perf_root/source-farm" --db "$perf_root/baseline-source-farm.db" \
	>"$artifact_dir/fixtures/baseline-source-farm-warm.json"
"$perf_root/candidate-driver" source-farm-warm \
	--root "$perf_root/source-farm" --db "$perf_root/candidate-source-farm.db" \
	>"$artifact_dir/fixtures/candidate-source-farm-warm.json"
"$perf_root/candidate-driver" source-farm-contract \
	--root "$perf_root/source-farm" --out "$artifact_dir/fixtures/source-farm-contract.json" \
	>"$artifact_dir/fixtures/source-farm-contract.stdout.json"

"$perf_root/baseline-driver" generate-long-ledger \
	--db "$perf_root/baseline-1m.db" --manifest "$artifact_dir/fixtures/baseline-1m.json" \
	--usage "$usage_count" --activity "$activity_count" --contexts "$context_count" \
	>"$artifact_dir/fixtures/baseline-1m.stdout.json"
"$perf_root/candidate-driver" generate-long-ledger \
	--db "$perf_root/candidate-1m.db" --manifest "$artifact_dir/fixtures/candidate-1m.json" \
	--usage "$usage_count" --activity "$activity_count" --contexts "$context_count" \
	>"$artifact_dir/fixtures/candidate-1m.stdout.json"
"$perf_root/baseline-driver" generate-long-ledger \
	--db "$perf_root/baseline-100k.db" --manifest "$artifact_dir/fixtures/baseline-100k.json" \
	--usage "$small_usage" --activity "$small_activity" --contexts "$small_context" \
	>"$artifact_dir/fixtures/baseline-100k.stdout.json"
"$perf_root/candidate-driver" generate-long-ledger \
	--db "$perf_root/candidate-100k.db" --manifest "$artifact_dir/fixtures/candidate-100k.json" \
	--usage "$small_usage" --activity "$small_activity" --contexts "$small_context" \
	>"$artifact_dir/fixtures/candidate-100k.stdout.json"

"$perf_root/baseline-driver" query-suite --db "$perf_root/baseline-1m.db" --matrix full --samples 1 \
	>"$artifact_dir/baseline/query/full.json"
"$perf_root/candidate-driver" query-suite --db "$perf_root/candidate-1m.db" --matrix full --samples 1 \
	>"$artifact_dir/candidate/query/full.json"

(cd "$candidate_tree" && AIUSAGE_PERF_DB="$perf_root/candidate-1m.db" \
	go test ./store -run '^TestLongLedgerAccelerationEquivalence$' -count=1) \
	| tee "$artifact_dir/fixtures/accelerated-equivalence.txt"

observe_sample() {
	local side="$1" driver="$2" binary="$3" db="$4" name="$5" command="$6" purpose="$7" sample="$8" warmups="$9"
	"$driver" observe --binary "$binary" --db "$db" --name "$name" --command "$command" \
		--purpose "$purpose" --samples 1 --warmups "$warmups" \
		>"$artifact_dir/$side/process/$name-$sample.json"
}

paired_process_metric() {
	local name="$1" command="$2" baseline_db="$3" candidate_db="$4" start="$5" end="$6"
	local sample warmups
	for sample in $(seq "$start" "$end"); do
		warmups=0
		if [[ "$sample" == "$warmup_sample" ]]; then warmups=1; fi
		if ((sample % 2 == 1)); then
			observe_sample baseline "$perf_root/baseline-driver" "$perf_root/baseline-aiusage" "$baseline_db" "$name" "$command" timed "$sample" "$warmups"
			observe_sample candidate "$perf_root/candidate-driver" "$perf_root/candidate-aiusage" "$candidate_db" "$name" "$command" timed "$sample" "$warmups"
		else
			observe_sample candidate "$perf_root/candidate-driver" "$perf_root/candidate-aiusage" "$candidate_db" "$name" "$command" timed "$sample" "$warmups"
			observe_sample baseline "$perf_root/baseline-driver" "$perf_root/baseline-aiusage" "$baseline_db" "$name" "$command" timed "$sample" "$warmups"
		fi
	done
}

observe_source_farm_sample() {
	local side="$1" driver="$2" db="$3" name="$4" command="$5" sample="$6" warmups="$7"
	local db_args=()
	if [[ -n "$db" ]]; then db_args=(--db "$db"); fi
	"$driver" observe --binary "$driver" --root "$perf_root/source-farm" "${db_args[@]}" \
		--name "$name" --command "$command" --purpose timed --samples 1 --warmups "$warmups" \
		>"$artifact_dir/$side/process/$name-$sample.json"
}

paired_source_farm_metric() {
	local name="$1" command="$2" baseline_db="$3" candidate_db="$4" start="$5" end="$6"
	local sample warmups
	for sample in $(seq "$start" "$end"); do
		warmups=0
		if [[ "$sample" == "$warmup_sample" ]]; then warmups=1; fi
		if ((sample % 2 == 1)); then
			observe_source_farm_sample baseline "$perf_root/baseline-driver" "$baseline_db" "$name" "$command" "$sample" "$warmups"
			observe_source_farm_sample candidate "$perf_root/candidate-driver" "$candidate_db" "$name" "$command" "$sample" "$warmups"
		else
			observe_source_farm_sample candidate "$perf_root/candidate-driver" "$candidate_db" "$name" "$command" "$sample" "$warmups"
			observe_source_farm_sample baseline "$perf_root/baseline-driver" "$baseline_db" "$name" "$command" "$sample" "$warmups"
		fi
	done
}

paired_query_samples() {
	local start="$1" end="$2" sample warmups
	for sample in $(seq "$start" "$end"); do
		warmups=0
		if [[ "$sample" == "$warmup_sample" ]]; then warmups=1; fi
		if ((sample % 2 == 1)); then
			"$perf_root/baseline-driver" query-suite --db "$perf_root/baseline-1m.db" --matrix timed --samples 1 --warmups "$warmups" >"$artifact_dir/baseline/query/timed-$sample.json"
			"$perf_root/candidate-driver" query-suite --db "$perf_root/candidate-1m.db" --matrix timed --samples 1 --warmups "$warmups" >"$artifact_dir/candidate/query/timed-$sample.json"
		else
			"$perf_root/candidate-driver" query-suite --db "$perf_root/candidate-1m.db" --matrix timed --samples 1 --warmups "$warmups" >"$artifact_dir/candidate/query/timed-$sample.json"
			"$perf_root/baseline-driver" query-suite --db "$perf_root/baseline-1m.db" --matrix timed --samples 1 --warmups "$warmups" >"$artifact_dir/baseline/query/timed-$sample.json"
		fi
	done
}

readonly micro_bench='^(BenchmarkReload|BenchmarkScrubStep|BenchmarkView|BenchmarkProductionRender120x40|BenchmarkProductionRender200x60)$'
readonly ui_bench='^BenchmarkProductionUIThread$'
readonly cold_bench='^(BenchmarkProductionColdLoad|BenchmarkRangeCycleBurst|BenchmarkRangeCyclePaced|BenchmarkRangeCycleRevisit)$'

bench_sample() {
	local side="$1" binary="$2" db="$3" sample="$4"
	AIUSAGE_PERF_DB="$db" "$binary" -test.run '^$' -test.bench "$micro_bench" -test.benchmem -test.benchtime 10x -test.count 1 \
		>>"$artifact_dir/benchmarks/$side.txt"
	# UI-thread handlers are tens of microseconds and a 10-iteration sample is
	# dominated by the one 50 KiB Model result allocation and GC scheduling.
	# More iterations stabilize the same workload without changing its limit.
	AIUSAGE_PERF_DB="$db" "$binary" -test.run '^$' -test.bench "$ui_bench" -test.benchmem -test.benchtime 1000x -test.count 1 \
		>>"$artifact_dir/benchmarks/$side.txt"
	AIUSAGE_PERF_DB="$db" "$binary" -test.run '^$' -test.bench "$cold_bench" -test.benchmem -test.benchtime 1x -test.count 1 \
		>>"$artifact_dir/benchmarks/$side.txt"
	echo "sample $sample complete" >&2
}

source_farm_bench_sample() {
	local side="$1" binary="$2" db="$3" sample="$4"
	AIUSAGE_SOURCE_FARM="$perf_root/source-farm" AIUSAGE_SOURCE_FARM_DB="$db" \
		"$binary" -test.run '^$' -test.bench '^BenchmarkSourceFarmUnchanged$' \
		-test.benchmem -test.benchtime 1x -test.count 1 \
		>>"$artifact_dir/benchmarks/$side.txt"
	echo "source-farm sample $sample complete" >&2
}

paired_bench_samples() {
	local start="$1" end="$2" sample
	for sample in $(seq "$start" "$end"); do
		if ((sample % 2 == 1)); then
			bench_sample baseline "$perf_root/baseline-tui.test" "$perf_root/baseline-1m.db" "$sample"
			source_farm_bench_sample baseline "$perf_root/baseline-perfdriver.test" "$perf_root/baseline-source-farm.db" "$sample"
			bench_sample candidate "$perf_root/candidate-tui.test" "$perf_root/candidate-1m.db" "$sample"
			source_farm_bench_sample candidate "$perf_root/candidate-perfdriver.test" "$perf_root/candidate-source-farm.db" "$sample"
		else
			bench_sample candidate "$perf_root/candidate-tui.test" "$perf_root/candidate-1m.db" "$sample"
			source_farm_bench_sample candidate "$perf_root/candidate-perfdriver.test" "$perf_root/candidate-source-farm.db" "$sample"
			bench_sample baseline "$perf_root/baseline-tui.test" "$perf_root/baseline-1m.db" "$sample"
			source_farm_bench_sample baseline "$perf_root/baseline-perfdriver.test" "$perf_root/baseline-source-farm.db" "$sample"
		fi
	done
}

run_timed_set() {
	local start="$1" end="$2"
	paired_query_samples "$start" "$end"
	paired_process_metric version version "" "" "$start" "$end"
	paired_process_metric summary-all summary-all "$perf_root/baseline-1m.db" "$perf_root/candidate-1m.db" "$start" "$end"
	paired_process_metric summary-breakdown summary-breakdown "$perf_root/baseline-1m.db" "$perf_root/candidate-1m.db" "$start" "$end"
	paired_process_metric summary-provider summary-provider "$perf_root/baseline-1m.db" "$perf_root/candidate-1m.db" "$start" "$end"
	paired_process_metric export-json-100k export-json "$perf_root/baseline-100k.db" "$perf_root/candidate-100k.db" "$start" "$end"
	paired_process_metric export-csv-100k export-csv "$perf_root/baseline-100k.db" "$perf_root/candidate-100k.db" "$start" "$end"
	paired_process_metric export-json-raw-100k export-json-raw "$perf_root/baseline-100k.db" "$perf_root/candidate-100k.db" "$start" "$end"
	paired_process_metric export-csv-raw-100k export-csv-raw "$perf_root/baseline-100k.db" "$perf_root/candidate-100k.db" "$start" "$end"
	paired_source_farm_metric source-farm-discovery source-farm-discovery "" "" "$start" "$end"
	paired_source_farm_metric source-farm-catchup source-farm-catchup "" "" "$start" "$end"
	paired_source_farm_metric source-farm-unchanged source-farm-unchanged "$perf_root/baseline-source-farm.db" "$perf_root/candidate-source-farm.db" "$start" "$end"
	paired_bench_samples "$start" "$end"
}

run_timed_set "$sample_start" "$sample_end"

if [[ "$skip_exports" == "0" ]]; then
	for command in export-json export-csv export-json-raw export-csv-raw; do
		name="${command}-1m"
		"$perf_root/candidate-driver" observe --binary "$perf_root/candidate-aiusage" \
			--db "$perf_root/candidate-1m.db" --name "$name" --command "$command" \
			--purpose absolute --samples 10 --warmups 1 \
			>"$artifact_dir/candidate/process/$name.json"
	done
fi

run_checker() {
	benchstat "$artifact_dir/benchmarks/baseline.txt" "$artifact_dir/benchmarks/candidate.txt" \
		>"$artifact_dir/benchmarks/benchstat.txt"
	"$perf_root/perfcheck" \
		--baseline "$artifact_dir/baseline" --candidate "$artifact_dir/candidate" \
		--baseline-bench "$artifact_dir/benchmarks/baseline.txt" \
		--candidate-bench "$artifact_dir/benchmarks/candidate.txt" \
		--out "$artifact_dir/result.json" | tee "$artifact_dir/report.md"
}

write_run_manifest() {
	local retry_used="$1"
	cat >"$artifact_dir/run.json" <<EOF
{"schema":"production-performance-run-v1","baseline":"$baseline_sha","candidate":"$candidate_sha","scale_divisor":$scale,"runner":"ubuntu-24.04","cgo_enabled":false,"gomaxprocs":2,"exports_included":$([[ "$skip_exports" == "0" ]] && echo true || echo false),"sample_set":"$sample_set","sample_start":$sample_start,"sample_end":$sample_end,"retry_used":$retry_used}
EOF
}

# CI runs primary and retry sets as separate 20-minute jobs. The finalizer first
# evaluates primary and consumes retry only for a relative timing-only failure.
# Keeping the standalone path makes the script useful outside Actions.
if [[ "$sample_set" != "standalone" ]]; then
	write_run_manifest false
	exit 0
fi

if ! run_checker; then
	if jq -e '[.results[] | select(.status == "FAIL")] as $f | ($f|length) > 0 and all($f[]; .category == "regression" and (.detail | startswith("median ratio")))' "$artifact_dir/result.json" >/dev/null; then
		echo "relative timing regression only; collecting the one permitted retry" >&2
		warmup_sample=11
		run_timed_set 11 20
		run_checker
		write_run_manifest true
	else
		exit 1
	fi
else
	write_run_manifest false
fi
