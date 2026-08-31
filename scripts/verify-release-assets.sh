#!/usr/bin/env bash

set -euo pipefail

if (( $# != 3 )); then
	echo "usage: verify-release-assets.sh VERSION ASSET_DIRECTORY CANDIDATE_SHA" >&2
	exit 2
fi

readonly version="$1"
readonly asset_dir="$2"
readonly candidate_sha="$3"
readonly archive_version="${version#v}"

if [[ ! -d "$asset_dir" ]]; then
	echo "asset directory does not exist: ${asset_dir}" >&2
	exit 1
fi
if [[ "$archive_version" == "$version" || -z "$archive_version" ]]; then
	echo "version must begin with v" >&2
	exit 1
fi
if [[ ! "$candidate_sha" =~ ^[0-9a-f]{40}$ ]]; then
	echo "candidate SHA must be a full lowercase commit hash" >&2
	exit 1
fi

expected_archives=(
	"aiusage_${archive_version}_darwin_amd64.tar.gz"
	"aiusage_${archive_version}_darwin_arm64.tar.gz"
	"aiusage_${archive_version}_linux_amd64.tar.gz"
	"aiusage_${archive_version}_linux_arm64.tar.gz"
)
expected_files=("${expected_archives[@]}" "checksums.txt")
mapfile -t expected_files < <(printf '%s\n' "${expected_files[@]}" | sort)
mapfile -t actual_files < <(find "$asset_dir" -mindepth 1 -maxdepth 1 -type f \
	-printf '%f\n' | sort)

if [[ "${actual_files[*]}" != "${expected_files[*]}" ]]; then
	echo "release asset manifest differs" >&2
	printf 'want: %s\n' "${expected_files[*]}" >&2
	printf 'got:  %s\n' "${actual_files[*]}" >&2
	exit 1
fi

(
	cd "$asset_dir"
	sha256sum -c checksums.txt
)

mapfile -t checksum_names < <(awk '{name=$2; sub(/^\*/, "", name); print name}' \
	"${asset_dir}/checksums.txt" | sort)
mapfile -t expected_checksum_names < <(printf '%s\n' "${expected_archives[@]}" | sort)
if [[ "${checksum_names[*]}" != "${expected_checksum_names[*]}" ]]; then
	echo "checksums.txt does not name exactly the four archives" >&2
	exit 1
fi

verify_tmp="$(mktemp -d "${RUNNER_TEMP:-/tmp}/aiusage-release-assets.XXXXXX")"
cleanup() {
	rm -rf -- "$verify_tmp"
}
trap cleanup EXIT

for archive in "${expected_archives[@]}"; do
	contents="$(tar -tzf "${asset_dir}/${archive}" | sed 's#^\./##' | sort)"
	if [[ "$contents" != $'LICENSE\naiusage' ]]; then
		echo "${archive} must contain exactly LICENSE and aiusage" >&2
		printf '%s\n' "$contents" >&2
		exit 1
	fi

	case "$archive" in
		*_linux_amd64.tar.gz) expected_os=linux; expected_arch=amd64 ;;
		*_linux_arm64.tar.gz) expected_os=linux; expected_arch=arm64 ;;
		*_darwin_amd64.tar.gz) expected_os=darwin; expected_arch=amd64 ;;
		*_darwin_arm64.tar.gz) expected_os=darwin; expected_arch=arm64 ;;
		*) echo "unrecognized archive ${archive}" >&2; exit 1 ;;
	esac

	extract_dir="${verify_tmp}/${expected_os}-${expected_arch}"
	mkdir -p "$extract_dir"
	tar -xzf "${asset_dir}/${archive}" -C "$extract_dir"
	build_info="$(go version -m "${extract_dir}/aiusage")"
	grep -F $'build\tGOOS='"${expected_os}" <<<"$build_info" >/dev/null
	grep -F $'build\tGOARCH='"${expected_arch}" <<<"$build_info" >/dev/null
	grep -F $'build\tvcs.revision='"${candidate_sha}" <<<"$build_info" >/dev/null
	grep -F $'build\tvcs.modified=false' <<<"$build_info" >/dev/null
done

reported_version="$("${verify_tmp}/linux-amd64/aiusage" version)"
if [[ "$reported_version" != "$version" ]]; then
	echo "Linux amd64 binary reports ${reported_version}; want ${version}" >&2
	exit 1
fi

(
	cd "$asset_dir"
	sha256sum "${expected_archives[@]}"
)
