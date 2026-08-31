#!/usr/bin/env bash
set -euo pipefail

if (( $# != 3 )); then
	echo "usage: macos-service-lifecycle.sh VERSION CANDIDATE_SHA ASSET_DIRECTORY" >&2
	exit 2
fi

readonly version="$1"
readonly candidate_sha="$2"
readonly asset_dir="$3"
readonly archive_version="${version#v}"
readonly archive="$asset_dir/aiusage_${archive_version}_darwin_arm64.tar.gz"
readonly label="io.github.randomcodespace.aiusage.collect"
launch_domain="gui/$(id -u)"
readonly launch_domain
readonly launch_target="$launch_domain/$label"
readonly plist_dir="$HOME/Library/LaunchAgents"
readonly plist_path="$plist_dir/$label.plist"
readonly default_config_dir="$HOME/.config/aiusage"
readonly default_config="$default_config_dir/config.json"

if [[ "$(uname -s)/$(uname -m)" != "Darwin/arm64" ]]; then
	echo "macOS lifecycle gate requires a native darwin/arm64 runner" >&2
	exit 1
fi
if [[ ! -f "$archive" ]]; then
	echo "missing lifecycle archive: $archive" >&2
	exit 1
fi
if [[ -e "$plist_path" ]]; then
	echo "runner already has the aiusage LaunchAgent: $plist_path" >&2
	exit 1
fi

lifecycle_root="$(mktemp -d "${RUNNER_TEMP:-/tmp}/aiusage-macos-lifecycle.XXXXXX")"
binary=""
pid_path="$lifecycle_root/state/aiusage.pid"
version_path="$lifecycle_root/state/daemon.version"
config_existed=0
config_dir_existed=0
plist_dir_existed=0
failure_path=""

clean_run() {
	env -i \
		"HOME=$HOME" \
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
		"TMPDIR=${RUNNER_TEMP:-/tmp}" \
		"$@"
}

run_with_failure() {
	env -i \
		"HOME=$HOME" \
		"PATH=$failure_path:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
		"TMPDIR=${RUNNER_TEMP:-/tmp}" \
		"$@"
}

cleanup() {
	set +e
	/bin/launchctl bootout "$launch_target" >/dev/null 2>&1
	if [[ -f "$pid_path" ]]; then
		cleanup_pid="$(cat "$pid_path" 2>/dev/null)"
		if [[ "$cleanup_pid" =~ ^[1-9][0-9]*$ ]]; then
			kill -TERM "$cleanup_pid" >/dev/null 2>&1
		fi
	fi
	rm -f -- "$plist_path"
	if [[ "$config_existed" -eq 1 ]]; then
		cp -p "$lifecycle_root/original-config.json" "$default_config"
	else
		rm -f -- "$default_config"
	fi
	if [[ "$config_dir_existed" -eq 0 ]]; then
		rmdir "$default_config_dir" >/dev/null 2>&1
	fi
	if [[ "$plist_dir_existed" -eq 0 ]]; then
		rmdir "$plist_dir" >/dev/null 2>&1
	fi
	rm -rf -- "$lifecycle_root"
}
trap cleanup EXIT

if [[ -d "$default_config_dir" ]]; then
	config_dir_existed=1
fi
if [[ -f "$default_config" ]]; then
	config_existed=1
	cp -p "$default_config" "$lifecycle_root/original-config.json"
fi
if [[ -d "$plist_dir" ]]; then
	plist_dir_existed=1
fi
mkdir -p "$default_config_dir" "$plist_dir" "$lifecycle_root/extract" \
	"$lifecycle_root/data" "$lifecycle_root/state" "$lifecycle_root/discovery"
tar -xzf "$archive" -C "$lifecycle_root/extract"
binary="$lifecycle_root/extract/aiusage"
chmod 0755 "$binary"

build_info="$(go version -m "$binary")"
grep -F $'build\tGOOS=darwin' <<<"$build_info" >/dev/null
grep -F $'build\tGOARCH=arm64' <<<"$build_info" >/dev/null
grep -F $'build\tvcs.revision='"$candidate_sha" <<<"$build_info" >/dev/null
grep -F $'build\tvcs.modified=false' <<<"$build_info" >/dev/null

db_path="$lifecycle_root/data/usage.db"
log_path="$lifecycle_root/state/aiusage.log"
discovery_dir="$lifecycle_root/discovery"
write_config() {
	configured_log="$1"
	printf '{"db_path":"%s","pid_path":"%s","log_path":"%s","home":"%s","pricing":{"refresh":false}}\n' \
		"$db_path" "$pid_path" "$configured_log" "$discovery_dir" >"$default_config"
	chmod 0600 "$default_config"
}
write_config "$log_path"
touch "$lifecycle_root/data/preserve-me"

once_output="$(clean_run "$binary" once)"
if [[ "$once_output" != "adapters=15 sources=0 seen=0 inserted=0 activity=0 snapshots=0 errors=0" ]]; then
	echo "lifecycle seed collection differs: $once_output" >&2
	exit 1
fi

clean_run "$binary" setup >"$lifecycle_root/setup.txt"
grep -F "supervised by launchd for this GUI login" "$lifecycle_root/setup.txt" >/dev/null
if [[ "$(find "$plist_dir" -maxdepth 1 -type f -name 'io.github.randomcodespace.aiusage*.plist' | wc -l)" -ne 1 ]]; then
	echo "setup did not install exactly one aiusage LaunchAgent" >&2
	exit 1
fi
grep -Fx '<!-- aiusage-generated-unit -->' "$plist_path" >/dev/null
/usr/bin/plutil -lint "$plist_path" >/dev/null
/bin/launchctl print "$launch_target" >/dev/null

launch_pid() {
	/bin/launchctl print "$launch_target" 2>/dev/null | awk '$1 == "pid" && $2 == "=" { print $3; exit }' || true
}

wait_for_new_pid() {
	old_pid="$1"
	for _ in $(seq 1 45); do
		new_pid="$(launch_pid)"
		if [[ "$new_pid" =~ ^[1-9][0-9]*$ && "$new_pid" != "$old_pid" ]] && kill -0 "$new_pid" 2>/dev/null; then
			printf '%s\n' "$new_pid"
			return 0
		fi
		sleep 1
	done
	echo "launchd did not produce a replacement collector PID" >&2
	return 1
}

first_pid="$(launch_pid)"
if [[ ! "$first_pid" =~ ^[1-9][0-9]*$ ]]; then
	echo "launchd did not report the initial collector PID" >&2
	exit 1
fi
kill -KILL "$first_pid"
restart_pid="$(wait_for_new_pid "$first_pid")"

/bin/launchctl bootout "$launch_target"
/bin/launchctl bootstrap "$launch_domain" "$plist_path"
bootstrap_pid="$(wait_for_new_pid "$restart_pid")"

printf '%s\n' "v0.0.0-stale" >"$version_path"
clean_run "$binary" summary --json >"$lifecycle_root/restart-summary.json" 2>"$lifecycle_root/restart-summary.err"
jq -e '.Totals.Events == 0' "$lifecycle_root/restart-summary.json" >/dev/null
grep -F "restarted $label" "$lifecycle_root/restart-summary.err" >/dev/null
mismatch_pid="$(wait_for_new_pid "$bootstrap_pid")"
if [[ "$(cat "$version_path")" != "$version" ]]; then
	echo "native mismatch restart did not restore the candidate version stamp" >&2
	exit 1
fi

clean_run "$binary" setup --remove >"$lifecycle_root/remove.txt"
if [[ -e "$plist_path" ]] || /bin/launchctl print "$launch_target" >/dev/null 2>&1; then
	echo "removal left the LaunchAgent installed or loaded" >&2
	exit 1
fi
if [[ ! -f "$db_path" || ! -f "$lifecycle_root/data/preserve-me" ]]; then
	echo "LaunchAgent removal deleted user data" >&2
	exit 1
fi

failure_path="$lifecycle_root/failure-bin"
mkdir -p "$failure_path"
cat >"$failure_path/launchctl" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "kickstart" ]]; then
	echo "injected activation failure" >&2
	exit 75
fi
exec /bin/launchctl "$@"
EOF
chmod 0755 "$failure_path/launchctl"
if run_with_failure "$binary" setup --force >"$lifecycle_root/failed-setup.txt" 2>&1; then
	echo "forced activation unexpectedly succeeded" >&2
	exit 1
fi
if [[ -e "$plist_path" ]] || /bin/launchctl print "$launch_target" >/dev/null 2>&1; then
	echo "failed activation did not restore the absent launchd state" >&2
	exit 1
fi

run_with_failure "$binary" summary --json >"$lifecycle_root/fallback-summary.json" 2>"$lifecycle_root/fallback-summary.err"
jq -e '.Totals.Events == 0' "$lifecycle_root/fallback-summary.json" >/dev/null
grep -F "could not install the aiusage service" "$lifecycle_root/fallback-summary.err" >/dev/null
for _ in $(seq 1 20); do
	if [[ -f "$pid_path" ]]; then
		detached_pid="$(cat "$pid_path")"
		if [[ "$detached_pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$detached_pid" 2>/dev/null; then
			break
		fi
	fi
	sleep 1
done
if [[ ! "${detached_pid:-}" =~ ^[1-9][0-9]*$ ]] || ! kill -0 "$detached_pid" 2>/dev/null; then
	echo "data command did not start the detached fallback" >&2
	exit 1
fi
collector_pids="$(pgrep -f "$binary run" || true)"
collector_count="$(printf '%s\n' "$collector_pids" | awk 'NF { count++ } END { print count + 0 }')"
if [[ "$collector_count" -ne 1 ]] || [[ -e "$plist_path" ]] || /bin/launchctl print "$launch_target" >/dev/null 2>&1; then
	echo "activation failure left duplicate or native collection running" >&2
	exit 1
fi
kill -TERM "$detached_pid"
for _ in $(seq 1 10); do
	if ! kill -0 "$detached_pid" 2>/dev/null; then
		break
	fi
	sleep 1
done

cat >"$plist_path" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$binary</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
EOF
chmod 0644 "$plist_path"
/usr/bin/plutil -lint "$plist_path" >/dev/null
unstamped_checksum="$(shasum -a 256 "$plist_path" | awk '{print $1}')"
clean_run "$binary" setup >"$lifecycle_root/unstamped-install.txt"
if [[ "$(shasum -a 256 "$plist_path" | awk '{print $1}')" != "$unstamped_checksum" ]]; then
	echo "ordinary install rewrote an unstamped plist" >&2
	exit 1
fi
if ! /bin/launchctl print "$launch_target" >/dev/null 2>&1; then
	echo "ordinary install did not activate the preserved plist" >&2
	exit 1
fi
if clean_run "$binary" setup --remove >"$lifecycle_root/unstamped-remove.txt" 2>&1; then
	echo "ordinary removal accepted an unstamped plist" >&2
	exit 1
fi
if [[ ! -f "$plist_path" ]]; then
	echo "ordinary removal deleted an unstamped plist" >&2
	exit 1
fi
clean_run "$binary" setup --remove --force >"$lifecycle_root/force-remove.txt"
if [[ -e "$plist_path" ]] || /bin/launchctl print "$launch_target" >/dev/null 2>&1; then
	echo "forced cleanup left the LaunchAgent installed or loaded" >&2
	exit 1
fi
if [[ ! -f "$db_path" || ! -f "$lifecycle_root/data/preserve-me" ]]; then
	echo "failure rollback or fallback deleted user data" >&2
	exit 1
fi

echo "real launchd lifecycle passed for $candidate_sha (last native pid $mismatch_pid)"
