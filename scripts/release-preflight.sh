#!/usr/bin/env bash

set -euo pipefail

if (( $# < 3 || $# > 4 )); then
	echo "usage: release-preflight.sh VERSION CANDIDATE_SHA RUN_ATTEMPT [OUTPUT_FILE]" >&2
	exit 2
fi

readonly version="$1"
readonly candidate_sha="$2"
readonly run_attempt="$3"
readonly output_file="${4:-}"
readonly repository="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

if [[ "${GITHUB_REF:-}" != "refs/heads/main" ]]; then
	echo "release dispatch must select main, got ${GITHUB_REF:-<unset>}" >&2
	exit 1
fi
if [[ ! "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
	echo "invalid release version ${version}; want vMAJOR.MINOR.PATCH with an optional prerelease suffix" >&2
	exit 1
fi
if [[ ! "$candidate_sha" =~ ^[0-9a-f]{40}$ ]]; then
	echo "candidate SHA must be a full lowercase commit hash" >&2
	exit 1
fi
if [[ ! "$run_attempt" =~ ^[1-9][0-9]*$ ]]; then
	echo "run attempt must be a positive integer" >&2
	exit 1
fi
if [[ "$(git rev-parse HEAD)" != "$candidate_sha" ]]; then
	echo "checked-out commit does not equal captured candidate ${candidate_sha}" >&2
	exit 1
fi
if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
	echo "release checkout is dirty" >&2
	exit 1
fi

remote_main="$(gh api "repos/${repository}/git/ref/heads/main" --jq .object.sha)"
if [[ "$remote_main" != "$candidate_sha" ]]; then
	echo "remote main moved to ${remote_main}; captured candidate is ${candidate_sha}" >&2
	exit 1
fi

set +e
remote_tag_output="$(git ls-remote origin \
	"refs/tags/${version}" "refs/tags/${version}^{}" 2>&1)"
remote_tag_status=$?
set -e
if (( remote_tag_status != 0 )); then
	echo "$remote_tag_output" >&2
	exit "$remote_tag_status"
fi
remote_tag_lines=()
if [[ -n "$remote_tag_output" ]]; then
	mapfile -t remote_tag_lines <<<"$remote_tag_output"
fi
release_json=""
release_error="$(mktemp "${RUNNER_TEMP:-/tmp}/aiusage-release-error.XXXXXX")"
set +e
release_json="$(gh api "repos/${repository}/releases/tags/${version}" 2>"$release_error")"
release_status=$?
set -e

if (( run_attempt == 1 )); then
	if (( ${#remote_tag_lines[@]} != 0 )); then
		echo "tag ${version} already exists" >&2
		exit 1
	fi
	if (( release_status == 0 )); then
		echo "release ${version} already exists" >&2
		exit 1
	fi
	if ! grep -q 'HTTP 404' "$release_error"; then
		cat "$release_error" >&2
		exit "$release_status"
	fi
else
	if (( ${#remote_tag_lines[@]} != 0 )); then
		peeled="$(printf '%s\n' "${remote_tag_lines[@]}" | awk '$2 ~ /\^\{\}$/ {print $1}')"
		if [[ "$peeled" != "$candidate_sha" ]]; then
			echo "retry tag ${version} does not peel to ${candidate_sha}" >&2
			exit 1
		fi
	fi
	if (( release_status == 0 )); then
		if [[ "$(jq -r .draft <<<"$release_json")" != "true" ]]; then
			echo "retry refuses the published release ${version}" >&2
			exit 1
		fi
		if (( ${#remote_tag_lines[@]} == 0 )); then
			echo "retry refuses draft ${version} without its immutable tag" >&2
			exit 1
		fi
		if [[ "$(jq -r .target_commitish <<<"$release_json")" != "$candidate_sha" ]]; then
			echo "retry draft ${version} does not target ${candidate_sha}" >&2
			exit 1
		fi
	fi
	if (( release_status != 0 )) && ! grep -q 'HTTP 404' "$release_error"; then
		cat "$release_error" >&2
		exit "$release_status"
	fi
fi
rm -f -- "$release_error"

runs_json="$(gh api \
	"repos/${repository}/actions/workflows/ci.yml/runs?branch=main&event=push&status=completed&per_page=100")"
ci_run_id="$(jq -r --arg sha "$candidate_sha" \
	'[.workflow_runs[] | select(.head_sha == $sha and .head_branch == "main" and .event == "push" and .conclusion == "success")][0].id // empty' \
	<<<"$runs_json")"
if [[ -z "$ci_run_id" ]]; then
	echo "no successful push CI run exists for candidate ${candidate_sha}" >&2
	exit 1
fi

jobs_json="$(gh api "repos/${repository}/actions/runs/${ci_run_id}/jobs?per_page=100")"
for required_job in \
	"build & test" \
	"staticcheck" \
	"race detector" \
	"API and machine compatibility" \
	"Adapter compatibility" \
	"Production performance" \
	"SonarCloud scan" \
	"vulnerability scan"; do
	if ! jq -e --arg name "$required_job" \
		'[.jobs[] | select(.name == $name and .conclusion == "success")] | length == 1' \
		<<<"$jobs_json" >/dev/null; then
		echo "CI run ${ci_run_id} lacks one successful ${required_job} job" >&2
		exit 1
	fi
done

artifacts_json="$(gh api "repos/${repository}/actions/runs/${ci_run_id}/artifacts?per_page=100")"
for required_artifact in coverage compatibility adapter-compatibility performance; do
	artifact_id="$(jq -r --arg name "$required_artifact" \
		'[.artifacts[] | select(.name == $name and .expired == false)][0].id // empty' \
		<<<"$artifacts_json")"
	if [[ -z "$artifact_id" ]]; then
		echo "CI run ${ci_run_id} has no current ${required_artifact} artifact" >&2
		exit 1
	fi
	if [[ "$required_artifact" == "coverage" ]]; then
		coverage_artifact_id="$artifact_id"
	elif [[ "$required_artifact" == "adapter-compatibility" ]]; then
		adapter_artifact_id="$artifact_id"
	fi
done

ci_run_url="${GITHUB_SERVER_URL:-https://github.com}/${repository}/actions/runs/${ci_run_id}"
if [[ -n "$output_file" ]]; then
	{
		echo "ci_run_id=${ci_run_id}"
		echo "ci_run_url=${ci_run_url}"
		echo "coverage_artifact_id=${coverage_artifact_id}"
		echo "adapter_artifact_id=${adapter_artifact_id}"
	} >>"$output_file"
fi

echo "candidate ${candidate_sha} is backed by CI run ${ci_run_id}"
