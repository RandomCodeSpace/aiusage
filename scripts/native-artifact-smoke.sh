#!/usr/bin/env bash
set -euo pipefail

if (( $# != 5 )); then
	echo "usage: native-artifact-smoke.sh VERSION CANDIDATE_SHA GOOS GOARCH ASSET_DIRECTORY" >&2
	exit 2
fi

readonly version="$1"
readonly candidate_sha="$2"
readonly expected_os="$3"
readonly expected_arch="$4"
readonly asset_dir="$5"
readonly archive_version="${version#v}"
readonly archive_name="aiusage_${archive_version}_${expected_os}_${expected_arch}.tar.gz"
readonly archive="$asset_dir/$archive_name"
readonly checksums="$asset_dir/checksums.txt"
repo_root="$(git rev-parse --show-toplevel)"
readonly repo_root

if [[ "$archive_version" == "$version" || -z "$archive_version" ]]; then
	echo "version must begin with v" >&2
	exit 2
fi
if [[ ! "$candidate_sha" =~ ^[0-9a-f]{40}$ ]]; then
	echo "candidate SHA must be a full lowercase commit hash" >&2
	exit 2
fi
case "$expected_os/$expected_arch" in
	linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64) ;;
	*)
		echo "unsupported native target: $expected_os/$expected_arch" >&2
		exit 2
		;;
esac
if [[ ! -f "$archive" || ! -f "$checksums" ]]; then
	echo "native smoke requires $archive_name and checksums.txt" >&2
	exit 1
fi

native_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
native_arch="$(uname -m)"
case "$native_arch" in
	x86_64) native_arch=amd64 ;;
	aarch64 | arm64) native_arch=arm64 ;;
esac
if [[ "$native_os/$native_arch" != "$expected_os/$expected_arch" ]]; then
	echo "runner is $native_os/$native_arch, expected $expected_os/$expected_arch" >&2
	exit 1
fi

checksum_lines="$(awk -v name="$archive_name" '$2 == name || $2 == "*" name { print $1 }' "$checksums")"
checksum_count="$(printf '%s\n' "$checksum_lines" | awk 'NF { count++ } END { print count + 0 }')"
if [[ "$checksum_count" != 1 ]]; then
	echo "checksums.txt must name $archive_name exactly once" >&2
	exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
	actual_checksum="$(sha256sum "$archive" | awk '{print $1}')"
else
	actual_checksum="$(shasum -a 256 "$archive" | awk '{print $1}')"
fi
if [[ "$actual_checksum" != "$checksum_lines" ]]; then
	echo "checksum mismatch for $archive_name" >&2
	exit 1
fi

contents="$(tar -tzf "$archive" | sed 's#^\./##' | LC_ALL=C sort)"
if [[ "$contents" != $'LICENSE\naiusage' ]]; then
	echo "$archive_name must contain exactly LICENSE and aiusage" >&2
	printf '%s\n' "$contents" >&2
	exit 1
fi

smoke_root="$(mktemp -d "${RUNNER_TEMP:-/tmp}/aiusage-native-smoke.XXXXXX")"
cleanup() {
	rm -rf -- "$smoke_root"
}
trap cleanup EXIT

extract_dir="$smoke_root/extract"
home_dir="$smoke_root/home"
discovery_home="$smoke_root/discovery"
config_home="$smoke_root/config"
data_home="$smoke_root/data"
state_home="$smoke_root/state"
tmp_dir="$smoke_root/tmp"
mkdir -p "$extract_dir" "$home_dir" "$discovery_home" "$config_home" "$data_home" "$state_home" "$tmp_dir"
tar -xzf "$archive" -C "$extract_dir"
binary="$extract_dir/aiusage"

build_info="$(go version -m "$binary")"
grep -F $'build\tGOOS='"$expected_os" <<<"$build_info" >/dev/null
grep -F $'build\tGOARCH='"$expected_arch" <<<"$build_info" >/dev/null
grep -F $'build\tvcs.revision='"$candidate_sha" <<<"$build_info" >/dev/null
grep -F $'build\tvcs.modified=false' <<<"$build_info" >/dev/null

config_path="$config_home/config.json"
printf '%s\n' '{"pricing":{"refresh":false}}' >"$config_path"
db_path="$data_home/usage.db"
clean_env=(
	env -i
	"HOME=$home_dir"
	"PATH=$PATH"
	"TMPDIR=$tmp_dir"
	"XDG_CONFIG_HOME=$config_home"
	"XDG_DATA_HOME=$data_home"
	"XDG_STATE_HOME=$state_home"
)
global_args=(--db "$db_path" --home "$discovery_home" --config "$config_path" --no-daemon)

reported_version="$("${clean_env[@]}" "$binary" version)"
if [[ "$reported_version" != "$version" ]]; then
	echo "binary reports $reported_version; expected $version" >&2
	exit 1
fi
"${clean_env[@]}" "$binary" --help >"$smoke_root/help.txt"
grep -F "Usage:" "$smoke_root/help.txt" >/dev/null

once_output="$("${clean_env[@]}" "$binary" "${global_args[@]}" once)"
if [[ "$once_output" != "adapters=15 sources=0 seen=0 inserted=0 activity=0 snapshots=0 errors=0" ]]; then
	echo "empty once result differs: $once_output" >&2
	exit 1
fi

"${clean_env[@]}" "$binary" "${global_args[@]}" doctor >"$smoke_root/doctor.txt"
grep -F "build:    $version" "$smoke_root/doctor.txt" >/dev/null
grep -F "path:           $db_path" "$smoke_root/doctor.txt" >/dev/null
grep -F "events:         0" "$smoke_root/doctor.txt" >/dev/null
grep -F "none: no collector is running" "$smoke_root/doctor.txt" >/dev/null
schema_pair="$(sed -n 's/^schema version: \([0-9][0-9]*\) (binary: \([0-9][0-9]*\))$/\1 \2/p' "$smoke_root/doctor.txt")"
schema_recorded="${schema_pair%% *}"
schema_binary="${schema_pair##* }"
if [[ -z "$schema_pair" || "$schema_recorded" != "$schema_binary" ]]; then
	echo "doctor did not report a current schema" >&2
	exit 1
fi
adapter_count=0
for adapter in claude-code codex copilot opencode hermes agy cline crush dsh goose kimi-code pi openclaw qwen-code reasonix; do
	if ! grep -E "^${adapter}[[:space:]]+configured, no data source$" "$smoke_root/doctor.txt" >/dev/null; then
		echo "doctor did not report the empty $adapter adapter" >&2
		exit 1
	fi
	adapter_count=$((adapter_count + 1))
done
if [[ "$adapter_count" != 15 ]]; then
	echo "doctor adapter count is $adapter_count; expected 15" >&2
	exit 1
fi

for report in summary today; do
	"${clean_env[@]}" "$binary" "${global_args[@]}" "$report" --json >"$smoke_root/$report.json"
	jq -e '
	  (.Buckets | length) == 0 and
	  .Totals.Events == 0 and .Totals.Sessions == 0 and
	  .Totals.Input == 0 and .Totals.Output == 0 and
	  .Totals.CacheCreation == 0 and .Totals.CacheRead == 0 and
	  .Totals.Reasoning == 0 and .Totals.Total == 0 and
	  .Totals.CostMicroUSD == 0 and .Totals.UnpricedEvents == 0 and
	  .Totals.ComputedCostEvents == 0 and .Totals.DisplayCostMicroUSD == 0
	' "$smoke_root/$report.json" >/dev/null
done

"${clean_env[@]}" "$binary" "${global_args[@]}" export --format json >"$smoke_root/events.json"
jq -e 'type == "array" and length == 0' "$smoke_root/events.json" >/dev/null
"${clean_env[@]}" "$binary" "${global_args[@]}" export --format csv >"$smoke_root/events.csv"
expected_header='tool,model,session,project,event_time,observed_time,input,output,cache_creation,cache_read,reasoning,total,request_id,message_id,source_path,kind,provider,service_tier,cost_micro_usd,cost_usd,price_source'
if [[ "$(tr -d '\r\n' <"$smoke_root/events.csv")" != "$expected_header" ]]; then
	echo "empty CSV header differs from the machine contract" >&2
	exit 1
fi

if [[ -e "$state_home/aiusage/aiusage.pid" ]]; then
	echo "native smoke left a detached daemon pidfile" >&2
	exit 1
fi
if [[ -e "$config_home/systemd/user/aiusage-collect.service" ]]; then
	echo "native smoke installed a systemd unit" >&2
	exit 1
fi
if [[ -e "$home_dir/Library/LaunchAgents/io.github.randomcodespace.aiusage.collect.plist" ]]; then
	echo "native smoke installed a LaunchAgent" >&2
	exit 1
fi
if pgrep -f "$binary run" >/dev/null; then
	echo "native smoke left a collector process" >&2
	exit 1
fi
if [[ -n "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]]; then
	echo "native smoke changed the checked-out candidate" >&2
	git -C "$repo_root" status --short --untracked-files=all >&2
	exit 1
fi

echo "native artifact smoke passed for $expected_os/$expected_arch at $candidate_sha"
