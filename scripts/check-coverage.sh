#!/usr/bin/env bash
# Coverage floor check for one Go module: fails below MIN (default 80).
# Usage: scripts/check-coverage.sh <module-dir> [min-percent]
set -euo pipefail

dir="${1:?module directory required}"
min="${2:-80}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

coverprofile="$(mktemp -t cover.XXXXXX)"
trap 'rm -f "$coverprofile"' EXIT

cd "$dir"
go test ./... -coverpkg=./... -coverprofile="$coverprofile" >/dev/null

# With -coverpkg across packages the same block appears once per test
# binary; merge to the max hit so coverage from any test counts.
merged="$(mktemp -t cover-merged.XXXXXX)"
python3 "$script_dir/merge-coverprofile.py" "$coverprofile" "$merged"
total="$(go tool cover -func="$merged" | awk '/^total:/ {sub(/%/, "", $3); print $3}')"
rm -f "$merged"

echo "$dir coverage: ${total}% (floor ${min}%)"
if awk "BEGIN{exit !($total < $min)}"; then
  echo "::error::${dir} coverage ${total}% is below the ${min}% floor"
  exit 1
fi
