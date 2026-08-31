#!/usr/bin/env bash

set -euo pipefail

readonly module_path="github.com/RandomCodeSpace/aiusage"
readonly first_api_baseline="9abd18cefe4f7cb8c556b9c17afa02abe156258b"
readonly apidiff_module="golang.org/x/exp/cmd/apidiff@v0.0.0-20260820122028-d6e0b57b1a69"

repo_root="$(git rev-parse --show-toplevel)"

latest_public_release() {
	if command -v gh >/dev/null 2>&1 && [[ -n "${GITHUB_REPOSITORY:-}" ]]; then
		gh api "repos/${GITHUB_REPOSITORY}/releases?per_page=100" \
			--jq '[.[] | select(.draft == false)][0].tag_name // ""'
		return
	fi
	git -C "$repo_root" tag --merged HEAD --list 'v[0-9]*' --sort=-v:refname | head -n 1
}

compat_base="${1:-${API_BASE_REF:-}}"
if [[ -z "$compat_base" ]]; then
	latest_release="$(latest_public_release)"
	if [[ "$latest_release" =~ ^v0\.([0-9]+)\. ]] && (( BASH_REMATCH[1] < 5 )); then
		compat_base="$first_api_baseline"
	elif [[ -n "$latest_release" ]]; then
		compat_base="$latest_release"
	else
		compat_base="$first_api_baseline"
	fi
fi

if ! git -C "$repo_root" cat-file -e "${compat_base}^{commit}" 2>/dev/null; then
	echo "API baseline ${compat_base} is not present in the checkout" >&2
	exit 1
fi

api_tmp="$(mktemp -d "${TMPDIR:-/tmp}/aiusage-api-compat.XXXXXX")"
baseline_tree="${api_tmp}/baseline"

cleanup() {
	git -C "$repo_root" worktree remove --force "$baseline_tree" >/dev/null 2>&1 || true
	rm -rf -- "$api_tmp"
}
trap cleanup EXIT

git -C "$repo_root" worktree add --detach "$baseline_tree" "$compat_base" >/dev/null

(
	cd "$baseline_tree"
	go run "$apidiff_module" -m -w "${api_tmp}/baseline.api" "$module_path"
)
(
	cd "$repo_root"
	go run "$apidiff_module" -m -w "${api_tmp}/candidate.api" "$module_path"
)

set +e
api_diff="$(go run "$apidiff_module" -m -incompatible \
	"${api_tmp}/baseline.api" "${api_tmp}/candidate.api" \
	2>"${api_tmp}/apidiff.stderr")"
apidiff_status=$?
set -e

if (( apidiff_status != 0 )); then
	cat "${api_tmp}/apidiff.stderr" >&2
	exit "$apidiff_status"
fi
if [[ -n "$api_diff" ]]; then
	echo "public Go API is incompatible with ${compat_base}:" >&2
	echo "$api_diff" >&2
	exit 1
fi

echo "public Go API is compatible with ${compat_base}"
