#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
readonly repo_root
candidate_sha="$(git rev-parse HEAD)"
readonly candidate_sha
readonly artifact_dir="${AIUSAGE_PERF_OUT:-$repo_root/performance}"
readonly command="${AIUSAGE_PERF_EXPORT_COMMAND:-}"
readonly scale="${AIUSAGE_PERF_SCALE:-1}"

case "$command" in
	export-json | export-csv | export-json-raw | export-csv-raw) ;;
	*)
		echo "AIUSAGE_PERF_EXPORT_COMMAND must name one export format" >&2
		exit 2
		;;
esac
if ! [[ "$scale" =~ ^[1-9][0-9]*$ ]]; then
	echo "AIUSAGE_PERF_SCALE must be a positive integer divisor" >&2
	exit 2
fi

mkdir -p "$artifact_dir"
if find "$artifact_dir" -mindepth 1 -print -quit | grep -q .; then
	echo "performance artifact directory is not empty: $artifact_dir" >&2
	exit 2
fi
mkdir -p "$artifact_dir/candidate/process" "$artifact_dir/fixtures"

perf_root="$(mktemp -d /tmp/aiusage-performance-export.XXXXXX)"
cleanup() {
	rm -rf -- "$perf_root"
}
trap cleanup EXIT

usage_count=$((1000000 / scale))
activity_count=$((250000 / scale))
context_count=$((100000 / scale))
usage_count=$((usage_count > 1000 ? usage_count : 1000))
activity_count=$((activity_count > 250 ? activity_count : 250))
context_count=$((context_count > 100 ? context_count : 100))

export CGO_ENABLED=0
export GOMAXPROCS=2
export TZ=UTC
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false"

(cd "$repo_root" && go build -o "$perf_root/aiusage" .)
(cd "$repo_root" && go build -o "$perf_root/perfdriver" ./internal/perfdriver)

"$perf_root/perfdriver" generate-long-ledger \
	--db "$perf_root/candidate-1m.db" \
	--manifest "$artifact_dir/fixtures/candidate-1m-$command.json" \
	--usage "$usage_count" --activity "$activity_count" --contexts "$context_count" \
	>"$artifact_dir/fixtures/candidate-1m-$command.stdout.json"

name="${command}-1m"
# The release limit is 30 seconds per sample. The outer bound allows ten
# samples plus one warm-up to report their measurements, but prevents a single
# regression from consuming the job's fixed 20-minute budget without evidence.
timeout --signal=TERM 7m "$perf_root/perfdriver" observe \
	--binary "$perf_root/aiusage" --db "$perf_root/candidate-1m.db" \
	--name "$name" --command "$command" --purpose absolute \
	--samples 10 --warmups 1 \
	>"$artifact_dir/candidate/process/$name.json"

jq -e --arg name "$name" '
  .samples == 10 and .warmups == 1 and
  .metric.name == $name and .metric.purpose == "absolute" and
  (.metric.durations_ns | length) == 10 and
  (.metric.first_byte_ns | length) == 10 and
  (.metric.max_rss_kb | length) == 10
' "$artifact_dir/candidate/process/$name.json" >/dev/null

cat >"$artifact_dir/export-$command-run.json" <<EOF
{"schema":"production-performance-export-v1","candidate":"$candidate_sha","command":"$command","scale_divisor":$scale,"runner":"ubuntu-24.04","cgo_enabled":false,"gomaxprocs":2}
EOF
