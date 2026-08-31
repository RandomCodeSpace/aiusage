#!/usr/bin/env bash
set -euo pipefail

if (( $# != 3 )); then
	echo "usage: linux-service-lifecycle.sh VERSION CANDIDATE_SHA ASSET_DIRECTORY" >&2
	exit 2
fi

readonly version="$1"
readonly candidate_sha="$2"
readonly asset_dir="$3"
readonly archive_version="${version#v}"
readonly archive="$asset_dir/aiusage_${archive_version}_linux_amd64.tar.gz"
readonly test_user="aiusageci"
readonly label="aiusage-collect.service"

if [[ "$(uname -s)/$(uname -m)" != "Linux/x86_64" ]]; then
	echo "Linux lifecycle gate requires a native linux/amd64 runner" >&2
	exit 1
fi
if [[ ! -f "$archive" ]]; then
	echo "missing lifecycle archive: $archive" >&2
	exit 1
fi
if id "$test_user" >/dev/null 2>&1; then
	echo "test account already exists: $test_user" >&2
	exit 1
fi

lifecycle_root="$(mktemp -d /tmp/aiusage-linux-lifecycle.XXXXXX)"
test_home=""
test_uid=""
runtime_dir=""
binary=""
failure_path=""

run_user() {
	sudo -u "$test_user" env -i \
		"HOME=$test_home" \
		"USER=$test_user" \
		"LOGNAME=$test_user" \
		"PATH=/usr/local/bin:/usr/bin:/bin" \
		"XDG_RUNTIME_DIR=$runtime_dir" \
		"DBUS_SESSION_BUS_ADDRESS=unix:path=$runtime_dir/bus" \
		"$@"
}

run_user_with_failure() {
	sudo -u "$test_user" env -i \
		"HOME=$test_home" \
		"USER=$test_user" \
		"LOGNAME=$test_user" \
		"PATH=$failure_path:/usr/local/bin:/usr/bin:/bin" \
		"XDG_RUNTIME_DIR=$runtime_dir" \
		"DBUS_SESSION_BUS_ADDRESS=unix:path=$runtime_dir/bus" \
		"$@"
}

cleanup() {
	set +e
	if [[ -n "$test_uid" ]] && id "$test_user" >/dev/null 2>&1; then
		run_user systemctl --user disable --now "$label" >/dev/null 2>&1
		sudo rm -rf -- "$test_home/.config/systemd/user/$label" "$test_home/.config/systemd/user/$label.d"
		run_user systemctl --user daemon-reload >/dev/null 2>&1
		sudo loginctl disable-linger "$test_user" >/dev/null 2>&1
		sudo systemctl stop "user@$test_uid.service" >/dev/null 2>&1
		sudo userdel -r "$test_user" >/dev/null 2>&1
	fi
	rm -rf -- "$lifecycle_root"
}
trap cleanup EXIT

sudo useradd --create-home --shell /bin/bash "$test_user"
test_home="$(getent passwd "$test_user" | cut -d: -f6)"
test_uid="$(id -u "$test_user")"
runtime_dir="/run/user/$test_uid"
sudo loginctl enable-linger "$test_user"
sudo systemctl start "user@$test_uid.service"

for _ in $(seq 1 20); do
	if [[ -S "$runtime_dir/bus" ]]; then
		break
	fi
	sleep 1
done
if [[ ! -S "$runtime_dir/bus" ]]; then
	echo "disposable systemd user manager did not create its bus" >&2
	exit 1
fi
run_user systemctl --user show-environment >/dev/null

extract_dir="$lifecycle_root/extract"
mkdir -p "$extract_dir"
tar -xzf "$archive" -C "$extract_dir"
chmod 0755 "$lifecycle_root" "$extract_dir" "$extract_dir/aiusage"
sudo -u "$test_user" mkdir -p "$test_home/bin"
sudo install -o "$test_user" -g "$test_user" -m 0755 "$extract_dir/aiusage" "$test_home/bin/aiusage"
binary="$test_home/bin/aiusage"
build_info="$(go version -m "$binary")"
grep -F $'build\tGOOS=linux' <<<"$build_info" >/dev/null
grep -F $'build\tGOARCH=amd64' <<<"$build_info" >/dev/null
grep -F $'build\tvcs.revision='"$candidate_sha" <<<"$build_info" >/dev/null
grep -F $'build\tvcs.modified=false' <<<"$build_info" >/dev/null

config_dir="$test_home/.config/aiusage"
data_dir="$test_home/.local/share/aiusage"
state_dir="$test_home/.local/state/aiusage"
discovery_dir="$test_home/empty-discovery"
unit_dir="$test_home/.config/systemd/user"
unit_path="$unit_dir/$label"
db_path="$data_dir/usage.db"
pid_path="$state_dir/aiusage.pid"
version_path="$state_dir/daemon.version"
log_path="$state_dir/aiusage.log"

sudo -u "$test_user" mkdir -p "$config_dir" "$data_dir" "$state_dir" "$discovery_dir" "$unit_dir"
config_tmp="$lifecycle_root/config.json"
printf '{"db_path":"%s","pid_path":"%s","log_path":"%s","home":"%s","pricing":{"refresh":false}}\n' \
	"$db_path" "$pid_path" "$log_path" "$discovery_dir" >"$config_tmp"
sudo install -o "$test_user" -g "$test_user" -m 0600 "$config_tmp" "$config_dir/config.json"
sudo -u "$test_user" touch "$data_dir/preserve-me"

once_output="$(run_user "$binary" once)"
if [[ "$once_output" != "adapters=15 sources=0 seen=0 inserted=0 activity=0 snapshots=0 errors=0" ]]; then
	echo "lifecycle seed collection differs: $once_output" >&2
	exit 1
fi

run_user "$binary" setup >"$lifecycle_root/setup.txt"
grep -F "persistence: linger confirmed" "$lifecycle_root/setup.txt" >/dev/null
if [[ "$(find "$unit_dir" -maxdepth 1 -type f -name 'aiusage*.service' | wc -l)" -ne 1 ]]; then
	echo "setup did not install exactly one aiusage service" >&2
	exit 1
fi
grep -Fx "# aiusage-generated-unit" "$unit_path" >/dev/null
run_user systemctl --user is-enabled "$label" | grep -Fx enabled >/dev/null
run_user systemctl --user is-active "$label" | grep -Fx active >/dev/null

systemd_pid() {
	run_user systemctl --user show "$label" --property=MainPID --value 2>/dev/null || true
}

wait_for_new_pid() {
	old_pid="$1"
	for _ in $(seq 1 45); do
		new_pid="$(systemd_pid)"
		if [[ "$new_pid" =~ ^[1-9][0-9]*$ && "$new_pid" != "$old_pid" ]] && run_user kill -0 "$new_pid" 2>/dev/null; then
			printf '%s\n' "$new_pid"
			return 0
		fi
		sleep 1
	done
	echo "systemd did not produce a replacement collector PID" >&2
	return 1
}

first_pid="$(systemd_pid)"
if [[ ! "$first_pid" =~ ^[1-9][0-9]*$ ]]; then
	echo "systemd did not report the initial collector PID" >&2
	exit 1
fi
run_user kill -KILL "$first_pid"
restart_pid="$(wait_for_new_pid "$first_pid")"

sudo systemctl stop "user@$test_uid.service"
sudo systemctl start "user@$test_uid.service"
for _ in $(seq 1 30); do
	if [[ -S "$runtime_dir/bus" ]] && run_user systemctl --user is-active "$label" 2>/dev/null | grep -Fx active >/dev/null; then
		break
	fi
	sleep 1
done
recreated_pid="$(systemd_pid)"
if [[ ! "$recreated_pid" =~ ^[1-9][0-9]*$ || "$recreated_pid" == "$restart_pid" ]]; then
	echo "enabled service did not start under the recreated lingering user manager" >&2
	exit 1
fi

printf '%s\n' "v0.0.0-stale" | sudo -u "$test_user" tee "$version_path" >/dev/null
run_user "$binary" summary --json >"$lifecycle_root/restart-summary.json" 2>"$lifecycle_root/restart-summary.err"
jq -e '.Totals.Events == 0' "$lifecycle_root/restart-summary.json" >/dev/null
grep -F "restarted $label" "$lifecycle_root/restart-summary.err" >/dev/null
mismatch_pid="$(wait_for_new_pid "$recreated_pid")"
if [[ "$(sudo -u "$test_user" cat "$version_path")" != "$version" ]]; then
	echo "native mismatch restart did not restore the candidate version stamp" >&2
	exit 1
fi

sudo -u "$test_user" sed -i '/^# aiusage-generated-unit$/d' "$unit_path"
unstamped_checksum="$(sha256sum "$unit_path" | awk '{print $1}')"
run_user "$binary" setup >"$lifecycle_root/unstamped-install.txt"
if [[ "$(sha256sum "$unit_path" | awk '{print $1}')" != "$unstamped_checksum" ]]; then
	echo "ordinary install rewrote an unstamped unit" >&2
	exit 1
fi
if run_user "$binary" setup --remove >"$lifecycle_root/unstamped-remove.txt" 2>&1; then
	echo "ordinary removal accepted an unstamped unit" >&2
	exit 1
fi
if [[ ! -f "$unit_path" ]]; then
	echo "ordinary removal deleted an unstamped unit" >&2
	exit 1
fi
run_user "$binary" setup --remove --force >"$lifecycle_root/force-remove.txt"
if [[ -e "$unit_path" ]] || run_user systemctl --user is-active "$label" >/dev/null 2>&1; then
	echo "forced cleanup left the systemd service installed or active" >&2
	exit 1
fi
if [[ ! -f "$db_path" || ! -f "$data_dir/preserve-me" ]]; then
	echo "service removal deleted user data" >&2
	exit 1
fi

failure_path="$lifecycle_root/failure-bin"
mkdir -p "$failure_path"
systemctl_path="$(command -v systemctl)"
cat >"$failure_path/systemctl" <<EOF
#!/usr/bin/env bash
for arg in "\$@"; do
	if [[ "\$arg" == "start" ]]; then
		echo "injected activation failure" >&2
		exit 75
	fi
done
exec "$systemctl_path" "\$@"
EOF
chmod 0755 "$failure_path/systemctl"
if run_user_with_failure "$binary" setup --force >"$lifecycle_root/failed-setup.txt" 2>&1; then
	echo "forced activation unexpectedly succeeded" >&2
	exit 1
fi
if [[ -e "$unit_path" ]] || run_user systemctl --user is-enabled "$label" >/dev/null 2>&1 || run_user systemctl --user is-active "$label" >/dev/null 2>&1; then
	echo "failed activation did not restore the absent systemd state" >&2
	exit 1
fi

run_user_with_failure "$binary" summary --json >"$lifecycle_root/fallback-summary.json" 2>"$lifecycle_root/fallback-summary.err"
jq -e '.Totals.Events == 0' "$lifecycle_root/fallback-summary.json" >/dev/null
grep -F "could not install the aiusage service" "$lifecycle_root/fallback-summary.err" >/dev/null
for _ in $(seq 1 20); do
	if [[ -f "$pid_path" ]]; then
		detached_pid="$(cat "$pid_path")"
		if [[ "$detached_pid" =~ ^[1-9][0-9]*$ ]] && run_user kill -0 "$detached_pid" 2>/dev/null; then
			break
		fi
	fi
	sleep 1
done
if [[ ! "${detached_pid:-}" =~ ^[1-9][0-9]*$ ]] || ! run_user kill -0 "$detached_pid" 2>/dev/null; then
	echo "data command did not start the detached fallback" >&2
	exit 1
fi
collector_pids="$(pgrep -u "$test_user" -f "$binary run" || true)"
collector_count="$(printf '%s\n' "$collector_pids" | awk 'NF { count++ } END { print count + 0 }')"
if [[ "$collector_count" -ne 1 ]] || [[ -e "$unit_path" ]] || run_user systemctl --user is-active "$label" >/dev/null 2>&1; then
	echo "activation failure left duplicate or native collection running" >&2
	exit 1
fi
run_user kill -TERM "$detached_pid"
for _ in $(seq 1 10); do
	if ! run_user kill -0 "$detached_pid" 2>/dev/null; then
		break
	fi
	sleep 1
done

if [[ ! -f "$db_path" || ! -f "$data_dir/preserve-me" ]]; then
	echo "failure rollback or fallback deleted user data" >&2
	exit 1
fi

echo "real systemd lifecycle passed for $candidate_sha (last native pid $mismatch_pid)"
