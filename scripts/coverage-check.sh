#!/usr/bin/env bash
# coverage-check.sh — enforce a minimum total Go statement-coverage threshold.
#
# Usage: scripts/coverage-check.sh <profile> [<threshold-percent>]
#   profile   path to a `go test -coverprofile=` output (default: cov.out)
#   threshold integer percentage to require (default: 85)
#
# Used by the CI `coverage-gate` job (SIN-62210, PR 8/12 of Fase 0). Prints the
# observed coverage to stdout and exits non-zero with a clear message when the
# total is below threshold, e.g. `coverage 78% < 85%`.

set -euo pipefail

profile="${1:-cov.out}"
threshold="${2:-85}"

if [[ ! -f "$profile" ]]; then
  echo "coverage-check: profile not found: $profile" >&2
  exit 2
fi

# `go tool cover -func` prints a final `total: ...  XX.X%` line. Pull the percent
# off with awk; truncate the trailing % so we can compare numerically.
total_line=$(go tool cover -func="$profile" | awk '/^total:/ {print}')
if [[ -z "$total_line" ]]; then
  echo "coverage-check: no total line in $profile (was the profile produced by 'go test -coverprofile'?)" >&2
  exit 2
fi

pct=$(awk '{sub("%","",$NF); print $NF}' <<<"$total_line")
echo "coverage: ${pct}% (threshold ${threshold}%)"

# Integer comparison after multiplying by 10 to keep one decimal of precision
# without depending on bc(1) being installed on the runner.
pct_int=$(awk -v p="$pct" 'BEGIN { printf "%d", (p*10) + 0.5 }')
threshold_int=$(awk -v t="$threshold" 'BEGIN { printf "%d", (t*10) + 0.5 }')

if (( pct_int < threshold_int )); then
  printf 'coverage %s%% < %s%%\n' "$pct" "$threshold" >&2
  exit 1
fi
