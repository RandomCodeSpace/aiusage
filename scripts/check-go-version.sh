#!/bin/sh
# Keep release builds inside the supported range without downloading a toolchain.
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
minimum=$(awk '$1 == "go" { print $2; exit }' "$repo_dir/go.mod")
# The build pin may use an older Go series with newer security backports.
# Keep the user's hard ceiling independent of that pin.
maximum=1.26.5
actual=$(GOTOOLCHAIN=local go env GOVERSION)

if ! awk -v actual="$actual" -v minimum="$minimum" -v maximum="$maximum" 'BEGIN {
    if (actual !~ /^go[0-9]+\.[0-9]+\.[0-9]+$/) exit 1
    sub(/^go/, "", actual)
    split(actual, a, "."); split(minimum, lo, "."); split(maximum, hi, ".")
    lower = 0; upper = 0
    for (i = 1; i <= 3; i++) {
        if (!lower && a[i] + 0 != lo[i] + 0) lower = (a[i] + 0 < lo[i] + 0 ? -1 : 1)
        if (!upper && a[i] + 0 != hi[i] + 0) upper = (a[i] + 0 < hi[i] + 0 ? -1 : 1)
    }
    exit (lower < 0 || upper > 0)
}'; then
    echo "unsupported Go toolchain $actual; use Go $minimum through $maximum" >&2
    exit 1
fi
printf '%s (supported: %s through %s)\n' "$actual" "$minimum" "$maximum"
