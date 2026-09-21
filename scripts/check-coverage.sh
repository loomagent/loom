#!/usr/bin/env bash
#
# Enforce a floor on total statement coverage.
#
# `go test -cover` counts only each package's own tests, so this number is lower
# than the truth for packages whose API other packages exercise. Treat it as a
# ratchet against regressions, not as the branch-coverage target: raise the floor
# as coverage improves, and review uncovered branches by hand in between.
set -euo pipefail

profile="${1:-coverage.out}"
floor="${COVERAGE_FLOOR:-84}"

total="$(go tool cover -func="${profile}" | awk '/^total:/ {sub(/%/, "", $3); print $3}')"
if [[ -z "${total}" ]]; then
	echo "check-coverage: could not read a total from ${profile}" >&2
	exit 1
fi

printf 'total statement coverage: %s%% (floor %s%%)\n' "${total}" "${floor}"
if awk "BEGIN {exit !(${total} + 0 < ${floor})}"; then
	echo "check-coverage: coverage ${total}% is below the ${floor}% floor" >&2
	exit 1
fi
